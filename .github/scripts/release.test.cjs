'use strict';

const assert = require('node:assert/strict');
const { execFile } = require('node:child_process');
const { createHash } = require('node:crypto');
const { mkdir, mkdtemp, readFile, writeFile, rm, stat, utimes } = require('node:fs/promises');
const { tmpdir } = require('node:os');
const { join } = require('node:path');
const test = require('node:test');
const { promisify } = require('node:util');
const { prepare, verify } = require('./release.cjs');

const execute = promisify(execFile);

const version = 'v0.2.0';
const commit = '1234567890abcdef1234567890abcdef12345678';
const names = [
  'gost-restreamer-v0.2.0-linux-amd64.tar.gz',
  'gost-restreamer_0.2.0_amd64.deb',
  'gost-restreamer-v0.2.0-SHA256SUMS',
];
const hash = (data) => createHash('sha256').update(data).digest('hex');

async function fixture(t, options = {}) {
  const dist = await mkdtemp(join(tmpdir(), 'gost-release-test-'));
  t.after(() => rm(dist, { recursive: true, force: true }));
  const contents = [Buffer.from('synthetic archive\n'), Buffer.from('synthetic deb\n')];
  contents.push(Buffer.from(contents.map((data, i) => `${hash(data)}  ${names[i]}\n`).join('')));
  for (let i = 0; i < names.length; i++) await writeFile(join(dist, names[i]), contents[i]);
  const draft = { id: 7, tag_name: version, draft: true, prerelease: false };
  const allAssets = names.map((name, i) => ({
    id: i + 11, name, size: contents[i].length, state: 'uploaded',
    digest: `sha256:${hash(contents[i])}`, bytes: contents[i],
  }));
  const state = {
    releases: options.releases ?? [draft],
    assets: options.assets ?? allAssets.map((asset) => ({ ...asset })),
    calls: [], writes: [], outputs: {}, hooks: {},
    tag: { type: 'commit', sha: commit },
    tags: new Map(),
  };
  function endpoint(method, action) {
    return async (params) => {
      state.calls.push({ method, params });
      if (state.hooks[method]) {
        const response = await state.hooks[method](params, state);
        if (response !== undefined) return response;
      }
      return action(params);
    };
  }
  function page(items, params, size) {
    const number = params.page ?? 1;
    const start = (number - 1) * size;
    return {
      data: items.slice(start, start + size),
      headers: { link: start + size < items.length ? `<https://api.invalid/?page=${number + 1}>; rel="next"` : '' },
    };
  }
  const repos = {
    listReleases: endpoint('listReleases', (params) => page(state.releases, params, 1)),
    getRelease: endpoint('getRelease', ({ release_id }) => {
      const release = state.releases.find(({ id }) => id === release_id);
      assert.ok(release, 'Запрошен отсутствующий релиз.');
      return { data: { ...release } };
    }),
    listReleaseAssets: endpoint('listReleaseAssets', (params) => page(state.assets, params, 1)),
    getReleaseAsset: endpoint('getReleaseAsset', ({ asset_id }) => {
      const asset = state.assets.find(({ id }) => id === asset_id);
      assert.ok(asset, 'Запрошен отсутствующий файл.');
      return { data: asset.bytes };
    }),
    uploadReleaseAsset: endpoint('uploadReleaseAsset', (params) => {
      state.writes.push({ method: 'uploadReleaseAsset', params });
      assert.equal(params.release_id, 7);
      assert.ok(Buffer.isBuffer(params.data));
      assert.equal(params.headers['content-length'], params.data.length);
      assert.equal(params.headers['content-type'], 'application/octet-stream');
      const asset = {
        id: state.assets.length + 100, name: params.name, bytes: params.data,
        size: params.data.length, state: 'uploaded', digest: `sha256:${hash(params.data)}`,
      };
      state.assets.push(asset);
      return { data: asset };
    }),
  };
  for (const method of ['createRelease', 'updateRelease', 'deleteRelease', 'deleteReleaseAsset', 'updateReleaseAsset']) {
    repos[method] = endpoint(method, (params) => {
      state.writes.push({ method, params });
      throw new Error(`Запрещённая запись в тестовом API: ${method}`);
    });
  }
  const github = {
    rest: {
      repos,
      git: {
        getRef: endpoint('getRef', (params) => {
          assert.equal(params.ref, `tags/${version}`);
          return { data: { object: state.tag } };
        }),
        getTag: endpoint('getTag', ({ tag_sha }) => {
          assert.ok(state.tags.has(tag_sha), 'Запрошен неизвестный объект тега.');
          return { data: { object: state.tags.get(tag_sha) } };
        }),
      },
    },
    paginate: async (method, params) => {
      state.calls.push({ method: 'paginate', params });
      const result = [];
      for (let number = 1; number <= 1000; number++) {
        const response = await method({ ...params, page: number });
        result.push(...response.data);
        if (!response.headers.link.includes('rel="next"')) return result;
      }
      throw new Error('Пагинация не завершилась.');
    },
  };
  const input = {
    github, dist, version, commit,
    context: {
      repo: { owner: 'test-owner', repo: 'test-repository' },
      ref: 'refs/heads/main',
      payload: { pull_request: { base: { ref: 'main' } } },
    },
    core: {
      setOutput: (key, value) => { state.outputs[key] = value; },
      info: () => {},
    },
  };
  return { input, state, allAssets, contents, draft };
}

function noWrites(state) {
  assert.deepEqual(state.writes, [], 'Проверка не должна изменять удалённый релиз.');
}

// Ожидания следуют требованиям безопасного повторного запуска и ручной публикации.
test('Новый main-релиз передаётся GoReleaser только после проверки локальных файлов и тега.', async (t) => {
  const { input, state } = await fixture(t, { releases: [] });
  await prepare(input);
  assert.deepEqual(state.outputs, { create: 'true', published: 'false' });
  noWrites(state);
});

test('Полный совпадающий Draft не создаётся повторно и проходит завершающую проверку.', async (t) => {
  const { input, state } = await fixture(t);
  await prepare(input);
  await verify(input);
  assert.deepEqual(state.outputs, { create: 'false', published: 'false' });
  noWrites(state);
});

test('Частичный Draft получает только отсутствующие файлы с исходными байтами, SHA256SUMS последним.', async (t) => {
  const { input, state, allAssets, contents } = await fixture(t);
  state.assets = [allAssets[0]];
  await prepare(input);
  assert.deepEqual(state.writes.map(({ params }) => params.name), names.slice(1));
  state.writes.forEach(({ params }, i) => assert.deepEqual(params.data, contents[i + 1]));
  assert.deepEqual(state.outputs, { create: 'false', published: 'false' });
  await verify(input);
});

test('Пустой Draft получает все три файла без создания другого релиза.', async (t) => {
  const { input, state } = await fixture(t, { assets: [] });
  await prepare(input);
  assert.deepEqual(state.writes.map(({ params }) => params.name), names);
  assert.deepEqual(state.outputs, { create: 'false', published: 'false' });
});

for (const operation of [prepare, verify]) {
  test(`${operation.name}: опубликованный релиз остаётся неизменным даже без локальных пакетов.`, async (t) => {
    const { input, state } = await fixture(t);
    state.releases[0].draft = false;
    input.dist = join(input.dist, 'absent');
    state.hooks.getRef = () => { throw new Error('Опубликованный релиз не требует тега.'); };
    await operation(input);
    assert.deepEqual(state.outputs, { create: 'false', published: 'true' });
    assert.equal(state.calls.some(({ method }) => ['getRef', 'listReleaseAssets'].includes(method)), false);
    noWrites(state);
  });

  for (const change of [
    { label: 'ветка develop', branch: 'develop', version },
    { label: 'dev-версия', branch: 'main', version: 'v0.2.0-dev.1+abc123' },
    { label: 'prerelease', branch: 'main', version: 'v0.2.0-rc.1' },
    { label: 'неканоническая версия', branch: 'main', version: 'v00.2.0' },
  ]) {
    test(`${operation.name}: ${change.label} не допускает работу с GitHub Releases.`, async (t) => {
      const { input, state } = await fixture(t);
      input.context.payload.pull_request.base.ref = change.branch;
      input.version = change.version;
      await assert.rejects(operation(input));
      assert.deepEqual(state.calls, []);
      noWrites(state);
    });
  }

  test(`${operation.name}: два релиза одного тега на разных страницах отклоняются до записи.`, async (t) => {
    const { input, state, draft } = await fixture(t);
    state.releases = [draft, { ...draft, id: 8 }];
    await assert.rejects(operation(input));
    assert.ok(state.calls.some(({ method, params }) => method === 'listReleases' && params.page === 2));
    noWrites(state);
  });

  test(`${operation.name}: prerelease=true несовместим со стабильным Draft.`, async (t) => {
    const { input, state } = await fixture(t);
    state.releases[0].prerelease = true;
    await assert.rejects(operation(input));
    noWrites(state);
  });

  test(`${operation.name}: тег другого commit не разрешает запись.`, async (t) => {
    const { input, state } = await fixture(t);
    state.tag.sha = 'f'.repeat(40);
    await assert.rejects(operation(input));
    noWrites(state);
  });

  test(`${operation.name}: полный commit обязателен до запросов к GitHub.`, async (t) => {
    const { input, state } = await fixture(t);
    input.commit = commit.slice(0, 12);
    await assert.rejects(operation(input));
    assert.deepEqual(state.calls, []);
    noWrites(state);
  });

  for (const field of ['digest', 'size', 'state', 'duplicate', 'unexpected']) {
    test(`${operation.name}: конфликт ${field} найден до загрузки недостающего файла.`, async (t) => {
      const { input, state, allAssets } = await fixture(t);
      state.assets = [{ ...allAssets[0] }];
      if (field === 'digest') state.assets[0].digest = `sha256:${'0'.repeat(64)}`;
      if (field === 'size') state.assets[0].size++;
      if (field === 'state') state.assets[0].state = 'starter';
      if (field === 'duplicate') state.assets.push({ ...state.assets[0], id: 90 });
      if (field === 'unexpected') state.assets.push({ ...allAssets[1], name: 'unexpected.deb' });
      await assert.rejects(operation(input));
      noWrites(state);
    });
  }

  for (const endpoint of ['listReleases', 'getRef', 'listReleaseAssets']) {
    test(`${operation.name}: ошибка чтения ${endpoint} останавливает запись.`, async (t) => {
      const { input, state } = await fixture(t, { assets: [] });
      const failure = new Error(`Тестовый отказ ${endpoint}`);
      state.hooks[endpoint] = () => { throw failure; };
      await assert.rejects(operation(input), (error) => error === failure);
      noWrites(state);
    });
  }
}

test('Релиз на последующей странице находится без создания дубликата.', async (t) => {
  const { input, state, draft } = await fixture(t);
  state.releases = [{ ...draft, id: 2, tag_name: 'v0.1.0' }, draft];
  await prepare(input);
  assert.equal(state.outputs.create, 'false');
  assert.ok(state.calls.some(({ method, params }) => method === 'listReleases' && params.page === 2));
  assert.ok(state.calls.some(({ method, params }) => method === 'listReleaseAssets' && params.page === 3));
  noWrites(state);
});

test('Без digest совпадение подтверждается скачанными двоичными данными.', async (t) => {
  const { input, state } = await fixture(t);
  state.assets.forEach((asset, i) => {
    delete asset.digest;
    if (i === 1) asset.bytes = Uint8Array.from(asset.bytes).buffer;
    if (i === 2) {
      const padded = Buffer.concat([Buffer.from('pad'), asset.bytes, Buffer.from('tail')]);
      asset.bytes = new Uint8Array(padded.buffer, padded.byteOffset + 3, asset.size);
    }
  });
  await verify(input);
  assert.equal(state.calls.filter(({ method }) => method === 'getReleaseAsset').length, 3);
  noWrites(state);
});

test('Без digest несовпадающие байты того же размера не заменяются.', async (t) => {
  const { input, state } = await fixture(t);
  state.assets = [state.assets[0]];
  delete state.assets[0].digest;
  state.assets[0].bytes = Buffer.alloc(state.assets[0].size, 65);
  await assert.rejects(prepare(input));
  noWrites(state);
});

test('Отказ скачивания вместо digest останавливает загрузку остальных файлов.', async (t) => {
  const { input, state } = await fixture(t);
  state.assets = [state.assets[0]];
  delete state.assets[0].digest;
  state.hooks.getReleaseAsset = () => { throw new Error('Тестовый отказ скачивания.'); };
  await assert.rejects(prepare(input));
  noWrites(state);
});

test('Аннотированный тег разрешается до проверенного commit.', async (t) => {
  const { input, state } = await fixture(t);
  state.tag = { type: 'tag', sha: 'a'.repeat(40) };
  state.tags.set(state.tag.sha, { type: 'commit', sha: commit });
  await verify(input);
  assert.equal(state.calls.filter(({ method }) => method === 'getTag').length, 1);
  noWrites(state);
});

test('Цикл аннотированных тегов прекращает проверку без записи.', async (t) => {
  const { input, state } = await fixture(t, { assets: [] });
  state.tag = { type: 'tag', sha: 'a'.repeat(40) };
  state.tags.set(state.tag.sha, state.tag);
  await assert.rejects(prepare(input));
  noWrites(state);
});

for (const readNumber of [1, 2]) {
  test(`Ручная публикация при повторном чтении ${readNumber} запрещает следующую загрузку.`, async (t) => {
    const { input, state } = await fixture(t, { assets: [] });
    let reads = 0;
    state.hooks.getRelease = () => {
      if (++reads === readNumber) state.releases[0].draft = false;
    };
    await prepare(input);
    assert.equal(state.outputs.published, 'true');
    noWrites(state);
  });
}

test('Изменение тега между проверками не разрешает загрузку.', async (t) => {
  const { input, state } = await fixture(t, { assets: [] });
  let reads = 0;
  state.hooks.getRef = () => {
    if (++reads === 2) state.tag = { type: 'commit', sha: 'f'.repeat(40) };
  };
  await assert.rejects(prepare(input));
  noWrites(state);
});

test('Файл, появившийся между чтениями Draft, не загружается повторно.', async (t) => {
  const { input, state, allAssets } = await fixture(t, { assets: [] });
  state.hooks.getRelease = () => { state.assets = allAssets.map((asset) => ({ ...asset })); };
  await prepare(input);
  noWrites(state);
  assert.equal(state.outputs.create, 'false');
});

test('Ошибка повторного чтения Draft не разрешает загрузку.', async (t) => {
  const { input, state } = await fixture(t, { assets: [] });
  state.hooks.getRelease = () => { throw new Error('Тестовый отказ повторного чтения.'); };
  await assert.rejects(prepare(input));
  noWrites(state);
});

test('Ошибка upload не допускает продолжения и ложного подтверждения готовности.', async (t) => {
  const { input, state } = await fixture(t, { assets: [] });
  state.hooks.uploadReleaseAsset = () => { throw new Error('Тестовый отказ upload.'); };
  await assert.rejects(prepare(input));
  assert.equal(state.calls.filter(({ method }) => method === 'uploadReleaseAsset').length, 1);
  assert.equal(state.outputs.create, undefined);
  assert.equal(state.outputs.published, undefined);
});

test('Завершающая проверка требует найденный Draft со всеми тремя файлами.', async (t) => {
  for (const releases of [[], [{ id: 7, tag_name: version, draft: true, prerelease: false }]]) {
    const { input, state } = await fixture(t, { releases, assets: [] });
    await assert.rejects(verify(input));
    noWrites(state);
  }
});

test('Отсутствие локального пакета не разрешает создание нового релиза.', async (t) => {
  const { input, state } = await fixture(t, { releases: [] });
  await rm(join(input.dist, names[1]));
  await assert.rejects(prepare(input));
  assert.equal(state.outputs.create, undefined);
  noWrites(state);
});

const stateQueries = [
  'show --property=LoadState --value gost-restreamer.service',
  'show --property=ActiveState --value gost-restreamer.service',
];
const listUnitFiles = 'list-unit-files --no-legend --no-pager --full gost-restreamer.service';
const stopService = 'stop gost-restreamer.service';
const disableService = 'disable gost-restreamer.service';

// Заменяется только путь проверки systemd; все команды проходят через тестовый systemctl.
async function maintainerFixture(t, scriptName, settings = {}) {
  const root = await mkdtemp(join(tmpdir(), 'gost-maintainer-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const script = await readFile(join(__dirname, '../../deploy/debian', scriptName), 'utf8');
  const guard = join(root, 'systemd');
  assert.equal(script.split('/run/systemd/system').length - 1, 1);
  const scriptPath = join(root, scriptName);
  await writeFile(scriptPath, script.replace('/run/systemd/system', `'${guard.replaceAll("'", "'\\''")}'`));
  const bin = join(root, 'bin');
  await mkdir(bin);
  const log = join(root, 'systemctl.log');
  await writeFile(join(bin, 'systemctl'), `#!/bin/sh
printf '%s\\n' "$*" >> "$SYSTEMCTL_TEST_LOG"
if [ "$*" = "$SYSTEMCTL_TEST_FAIL" ]; then exit 23; fi
case "$*" in
  'show --property=LoadState --value gost-restreamer.service') printf '%s\\n' "$SYSTEMCTL_TEST_LOAD" ;;
  'show --property=ActiveState --value gost-restreamer.service') printf '%s\\n' "$SYSTEMCTL_TEST_ACTIVE" ;;
  'list-unit-files --no-legend --no-pager --full gost-restreamer.service') printf '%s\\n' "$SYSTEMCTL_TEST_FILES" ;;
esac
`, { mode: 0o755 });
  const env = {
    PATH: `${bin}:/usr/bin:/bin`, SYSTEMCTL_TEST_LOG: log,
    SYSTEMCTL_TEST_LOAD: 'loaded', SYSTEMCTL_TEST_ACTIVE: 'active',
    SYSTEMCTL_TEST_FILES: 'gost-restreamer.service enabled enabled', SYSTEMCTL_TEST_FAIL: '',
    ...settings,
  };
  return {
    guard, log,
    run: (action, args = [], overrides = {}) => execute('/bin/sh', [scriptPath, action, ...args], { env: { ...env, ...overrides } }),
    commands: async () => {
      const actual = await readFile(log, 'utf8').catch((error) => {
        if (error.code === 'ENOENT') return '';
        throw error;
      });
      return actual.trim() ? actual.trim().split('\n') : [];
    },
  };
}

for (const [scriptName, scenarios] of [
  ['postinst', [
    ['configure', ['daemon-reload']],
    ['abort-upgrade', ['daemon-reload']],
    ['abort-remove', ['daemon-reload']],
    ['abort-deconfigure', ['daemon-reload']],
    ['unexpected', []],
  ]],
  ['prerm', [
    ['remove', [...stateQueries, stopService, listUnitFiles, disableService]],
    ['upgrade', [...stateQueries, stopService]],
    ['deconfigure', [...stateQueries, stopService]],
    ['failed-upgrade', [...stateQueries, stopService]],
    ['unexpected', []],
  ]],
  ['postrm', [
    ['remove', ['daemon-reload']],
    ['purge', ['daemon-reload']],
    ['upgrade', ['daemon-reload']],
    ['failed-upgrade', ['daemon-reload']],
    ['abort-install', ['daemon-reload']],
    ['abort-upgrade', ['daemon-reload']],
    ['disappear', ['daemon-reload']],
    ['unexpected', []],
  ]],
]) {
  test(`${scriptName}: команды systemctl соответствуют install/upgrade/remove и никогда не запускают сервис.`, async (t) => {
    const subject = await maintainerFixture(t, scriptName);
    for (const [action, expected] of scenarios) {
      await rm(subject.log, { force: true });
      await subject.run(action);
      await assert.rejects(readFile(subject.log), { code: 'ENOENT' });
      await mkdir(subject.guard);
      try {
        await subject.run(action);
        assert.deepEqual(await subject.commands(), expected, action);
      } finally {
        await rm(subject.guard, { recursive: true });
      }
    }
  });
}

for (const action of ['remove', 'upgrade', 'deconfigure', 'failed-upgrade']) {
  for (const [load, active, files, expectedStop, expectedDisable] of [
    ['not-found', 'inactive', '', false, false],
    ['loaded', 'active', 'gost-restreamer.service enabled enabled', true, true],
    ['loaded', 'inactive', 'gost-restreamer.service disabled enabled', true, true],
    ['not-found', 'active', '', true, false],
    ['loaded', 'active', '', true, false],
    ['not-found', 'inactive', 'gost-restreamer.service disabled enabled', false, true],
  ]) {
    test(`prerm ${action}: ${load}/${active}, unit ${files ? 'присутствует' : 'отсутствует'}.`, async (t) => {
      const subject = await maintainerFixture(t, 'prerm', {
        SYSTEMCTL_TEST_LOAD: load, SYSTEMCTL_TEST_ACTIVE: active, SYSTEMCTL_TEST_FILES: files,
      });
      await mkdir(subject.guard);
      await subject.run(action);
      const expected = [...stateQueries];
      if (expectedStop) expected.push(stopService);
      if (action === 'remove') {
        expected.push(listUnitFiles);
        if (expectedDisable) expected.push(disableService);
      }
      assert.deepEqual(await subject.commands(), expected);
    });
  }
}

for (const [failure, expected] of [
  [stateQueries[0], stateQueries.slice(0, 1)],
  [stateQueries[1], stateQueries],
  [stopService, [...stateQueries, stopService]],
  [listUnitFiles, [...stateQueries, stopService, listUnitFiles]],
  [disableService, [...stateQueries, stopService, listUnitFiles, disableService]],
]) {
  test(`prerm: отказ ${failure} возвращается вызывающему процессу и останавливает дальнейшие команды.`, async (t) => {
    const subject = await maintainerFixture(t, 'prerm', { SYSTEMCTL_TEST_FAIL: failure });
    await mkdir(subject.guard);
    await assert.rejects(subject.run('remove'), { code: 23 });
    assert.deepEqual(await subject.commands(), expected);
  });
}

// Debian Policy 6.6 разрешает распаковку после успешного new-prerm failed-upgrade.
// Обе попытки обязаны сообщить об отказе остановки, чтобы не разрешить замену работающих бинарников.
for (const [failure, expected] of [
  [stateQueries[0], stateQueries.slice(0, 1)],
  [stateQueries[1], stateQueries],
  [stopService, [...stateQueries, stopService]],
]) {
  test(`prerm: upgrade и failed-upgrade не разрешают продолжение после отказа ${failure}.`, async (t) => {
    const subject = await maintainerFixture(t, 'prerm', { SYSTEMCTL_TEST_FAIL: failure });
    await mkdir(subject.guard);
    await assert.rejects(subject.run('upgrade', ['0.3.0']), { code: 23 });
    await assert.rejects(subject.run('failed-upgrade', ['0.2.0', '0.3.0']), { code: 23 });
    assert.deepEqual(await subject.commands(), [...expected, ...expected]);
  });
}

test('prerm: failed-upgrade разрешает продолжение после успешной повторной остановки.', async (t) => {
  const subject = await maintainerFixture(t, 'prerm', { SYSTEMCTL_TEST_FAIL: stopService });
  await mkdir(subject.guard);
  await assert.rejects(subject.run('upgrade', ['0.3.0']), { code: 23 });
  await subject.run('failed-upgrade', ['0.2.0', '0.3.0'], { SYSTEMCTL_TEST_FAIL: '' });
  assert.deepEqual(await subject.commands(), [
    ...stateQueries, stopService, ...stateQueries, stopService,
  ]);
});

async function reproducibilityFixture(t, mode = 'unchanged') {
  const root = await mkdtemp(join(tmpdir(), 'gost-reproducibility-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const config = join(root, 'configs/example.yaml');
  const unit = join(root, 'deploy/systemd/gost-restreamer.service');
  await mkdir(join(root, 'configs'));
  await mkdir(join(root, 'deploy/systemd'), { recursive: true });
  await writeFile(config, 'config: synthetic\n');
  await writeFile(unit, '[Unit]\nDescription=synthetic\n');
  await utimes(config, 1600000000.125, 1600000000.125);
  await utimes(unit, 1700000000.375, 1700000000.375);
  const originals = await Promise.all([config, unit].map(async (path) => ({
    path, bytes: await readFile(path), mtime: (await stat(path, { bigint: true })).mtimeNs,
  })));
  await mkdir(join(root, 'dist'));
  const contents = ['synthetic archive\n', 'synthetic deb\n'];
  contents.push(contents.map((data, i) => `${hash(data)}  ${names[i]}\n`).join(''));
  for (let i = 0; i < names.length; i++) await writeFile(join(root, 'dist', names[i]), contents[i]);
  const bin = join(root, 'bin');
  await mkdir(bin);
  await writeFile(join(bin, 'mktemp'), `#!/bin/sh
set -eu
created="$(PATH=/usr/bin:/bin mktemp "$@")"
printf '%s\\n' "$created" > "$REPRO_TEST_ROOT/temp.path"
printf '%s\\n' "$created"
`, { mode: 0o755 });
  await writeFile(join(bin, 'make'), `#!/bin/sh
set -eu
printf '%s\\n' "$@" "$GORELEASER" "$CGO_ENABLED" "$GOOS" "$GOARCH" "$GOTOOLCHAIN" "$GOWORK" "$GOFLAGS" > make.log
touch -r configs/example.yaml config.seen
touch -r deploy/systemd/gost-restreamer.service unit.seen
case "$REPRO_TEST_MODE" in
  archive|deb)
    if [ "$REPRO_TEST_MODE" = archive ]; then target="$REPRO_TEST_ARCHIVE"; else target="$REPRO_TEST_DEB"; fi
    printf 'changed\\n' >> "dist/$target"
    (cd dist && sha256sum "$REPRO_TEST_ARCHIVE" "$REPRO_TEST_DEB" > "$REPRO_TEST_SUMS")
    ;;
  manifest)
    awk 'NR == 1 { first=$0; next } { print } END { print first }' "dist/$REPRO_TEST_SUMS" > "dist/new-sums"
    mv dist/new-sums "dist/$REPRO_TEST_SUMS"
    ;;
  failure) exit 27 ;;
esac
`, { mode: 0o755 });
  const env = {
    PATH: `${bin}:/usr/bin:/bin:/sbin`, VERSION: version,
    GORELEASER: '/pinned/test/goreleaser', CGO_ENABLED: '1', GOOS: 'linux', GOARCH: 'amd64',
    GOTOOLCHAIN: 'local', GOWORK: 'off', GOFLAGS: '-mod=readonly', REPRO_TEST_ROOT: root,
    REPRO_TEST_MODE: mode, REPRO_TEST_ARCHIVE: names[0], REPRO_TEST_DEB: names[1], REPRO_TEST_SUMS: names[2],
  };
  return {
    root, originals,
    run: () => execute('/bin/sh', [join(__dirname, 'package-reproducibility.sh')], { cwd: root, env }),
    restored: async () => {
      for (const original of originals) {
        assert.deepEqual(await readFile(original.path), original.bytes);
        assert.equal((await stat(original.path, { bigint: true })).mtimeNs, original.mtime);
      }
      const temporary = (await readFile(join(root, 'temp.path'), 'utf8')).trim();
      assert.equal(temporary.startsWith('/tmp/gost-package-reproducibility.'), true);
      await assert.rejects(stat(temporary), { code: 'ENOENT' });
    },
  };
}

test('Повторная упаковка меняет даты checkout, сохраняет среду сборки и восстанавливает исходные даты.', async (t) => {
  const subject = await reproducibilityFixture(t);
  await subject.run();
  assert.deepEqual((await readFile(join(subject.root, 'make.log'), 'utf8')).trim().split('\n'), [
    'package-debian', `VERSION=${version}`, '/pinned/test/goreleaser', '1', 'linux', 'amd64',
    'local', 'off', '-mod=readonly',
  ]);
  for (const [i, file] of ['config.seen', 'unit.seen'].entries()) {
    assert.notEqual((await stat(join(subject.root, file), { bigint: true })).mtimeNs, subject.originals[i].mtime);
  }
  await subject.restored();
});

for (const mode of ['archive', 'deb', 'manifest', 'failure']) {
  test(`Повторная упаковка отклоняет ${mode === 'failure' ? 'отказ make' : `изменение ${mode}`} и восстанавливает исходные даты.`, async (t) => {
    const subject = await reproducibilityFixture(t, mode);
    await assert.rejects(subject.run(), mode === 'failure' ? { code: 27 } : undefined);
    await subject.restored();
  });
}

for (const missing of [...names.map((name) => `dist/${name}`), 'configs/example.yaml', 'deploy/systemd/gost-restreamer.service']) {
  test(`Повторная упаковка без ${missing} останавливается до изменения дат или запуска make.`, async (t) => {
    const subject = await reproducibilityFixture(t);
    await rm(join(subject.root, missing));
    await assert.rejects(subject.run());
    await assert.rejects(readFile(join(subject.root, 'make.log')), { code: 'ENOENT' });
    for (const original of subject.originals.filter(({ path }) => path !== join(subject.root, missing))) {
      assert.deepEqual(await readFile(original.path), original.bytes);
      assert.equal((await stat(original.path, { bigint: true })).mtimeNs, original.mtime);
    }
  });
}
