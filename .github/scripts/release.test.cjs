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

// execute позволяет ожидать завершения execFile и получать отказ дочерней команды через отклонение
// Promise.
const execute = promisify(execFile);

// version задаёт каноническую стабильную версию, общую для тестовых тегов, выпусков и имён файлов.
const version = 'v0.2.0';
// commit задаёт синтетический полный SHA коммита, на который должен разрешаться тестовый тег.
const commit = '1234567890abcdef1234567890abcdef12345678';
// names содержит ожидаемые имена трёх файлов тестовой поставки; позиция связывает имя с исходными
// байтами.
const names = [
  'gost-restreamer-v0.2.0-linux-amd64.tar.gz',
  'gost-restreamer_0.2.0_amd64.deb',
  'gost-restreamer-v0.2.0-SHA256SUMS',
];
// hash вычисляет SHA-256 синтетических байтов для независимой сверки тестовых артефактов.
const hash = (data) => createHash('sha256').update(data).digest('hex');

// fixture создаёт временные локальные файлы и управляемый GitHub API с журналом чтений и записей;
// сетевые запросы не выполняются.
async function fixture(t, options = {}) {
  // dist указывает на частный временный каталог файлов выпуска, удаляемый после теста.
  const dist = await mkdtemp(join(tmpdir(), 'gost-release-test-'));
  t.after(/* Удаляет частный каталог локальных файлов выпуска после завершения теста. */ () => rm(dist, { recursive: true, force: true }));
  // contents хранит синтетические байты архива, Debian-пакета и добавляемого манифеста в порядке
  // names.
  const contents = [Buffer.from('synthetic archive\n'), Buffer.from('synthetic deb\n')];
  contents.push(Buffer.from(contents.map(/* Формирует строку SHA256SUMS для соответствующих синтетических байтов и имени файла. */ (data, i) => `${hash(data)}  ${names[i]}\n`).join('')));
  // i выбирает соответствующие друг другу имя и байты файла, создаваемого в тестовом каталоге.
  for (let i = 0; i < names.length; i++) await writeFile(join(dist, names[i]), contents[i]);
  // draft задаёт исходный стабильный черновик: id определяет выпуск, tag_name задаёт версию,
  // draft отмечает отсутствие публикации, а prerelease — признак предварительной версии.
  const draft = { id: 7, tag_name: version, draft: true, prerelease: false };
  // allAssets содержит полный исходный набор файлов для построения удалённого состояния сценария.
  // В каждом описании id идентифицирует файл, name задаёт имя, size — длину в байтах,
  // state — состояние загрузки, digest — SHA-256, bytes — исходные байты.
  const allAssets = names.map(/* Создаёт тестовые метаданные артефакта с исходными байтами, размером и SHA-256. */ (name, i) => ({
    id: i + 11, name, size: contents[i].length, state: 'uploaded',
    digest: `sha256:${hash(contents[i])}`, bytes: contents[i],
  }));
  // state хранит управляемое состояние API: releases — выпуски, assets — их файлы, calls — журнал
  // обращений, writes — журнал изменений, outputs — выходы CI, hooks — подставленные действия, tag —
  // текущую ссылку, tags — объекты аннотированных тегов. В ссылках type задаёт вид Git-объекта,
  // sha — его идентификатор; в журналах method задаёт операцию, params — её аргументы.
  const state = {
    releases: options.releases ?? [draft],
    assets: options.assets ?? allAssets.map(/* Копирует описание артефакта, чтобы сценарий менял собственный набор удалённых файлов. */ (asset) => ({ ...asset })),
    calls: [], writes: [], outputs: {}, hooks: {},
    tag: { type: 'commit', sha: commit },
    tags: new Map(),
  };
  // endpoint оборачивает тестовый API журналом вызовов и позволяет сценарию изменить результат либо
  // состояние перед штатным действием.
  function endpoint(method, action) {
    return /* Записывает вызов тестового API, выполняет подставленный обработчик и при отсутствии его результата вызывает штатное действие. */ async (params) => {
      state.calls.push({ method, params });
      if (state.hooks[method]) {
        // response содержит результат подставленного действия; undefined разрешает выполнить штатную
        // операцию API.
        const response = await state.hooks[method](params, state);
        if (response !== undefined) return response;
      }
      return action(params);
    };
  }
  // page возвращает одну страницу тестовых объектов с Link на следующую, чтобы проверять полный обход
  // пагинации.
  function page(items, params, size) {
    // number содержит запрошенный номер страницы, начиная с 1; отсутствие номера выбирает первую
    // страницу.
    const number = params.page ?? 1;
    // start задаёт индекс первого элемента запрошенной страницы в исходном массиве.
    const start = (number - 1) * size;
    return {
      data: items.slice(start, start + size),
      headers: { link: start + size < items.length ? `<https://api.invalid/?page=${number + 1}>; rel="next"` : '' },
    };
  }
  // repos содержит управляемые операции API выпусков и файлов; каждая операция записывается в calls и
  // может быть заменена через hooks.
  const repos = {
    listReleases: endpoint('listReleases', /* Возвращает одну страницу тестовых выпусков для проверки полного обхода списка. */ (params) => page(state.releases, params, 1)),
    getRelease: endpoint('getRelease', /* Находит выпуск по release_id и возвращает копию его текущего состояния. */ ({ release_id }) => {
      // release содержит найденный по release_id тестовый выпуск, состояние которого копируется в ответ.
      const release = state.releases.find(/* Сопоставляет идентификатор выпуска с release_id тестового запроса. */ ({ id }) => id === release_id);
      assert.ok(release, 'Запрошен отсутствующий релиз.');
      return { data: { ...release } };
    }),
    listReleaseAssets: endpoint('listReleaseAssets', /* Возвращает одну страницу тестовых артефактов выпуска. */ (params) => page(state.assets, params, 1)),
    getReleaseAsset: endpoint('getReleaseAsset', /* Находит артефакт по asset_id и возвращает его сохранённые двоичные данные. */ ({ asset_id }) => {
      // asset содержит найденный по asset_id тестовый файл, байты которого возвращаются вместо сетевого
      // скачивания.
      const asset = state.assets.find(/* Сопоставляет идентификатор артефакта с asset_id тестового запроса. */ ({ id }) => id === asset_id);
      assert.ok(asset, 'Запрошен отсутствующий файл.');
      return { data: asset.bytes };
    }),
    uploadReleaseAsset: endpoint('uploadReleaseAsset', /* Проверяет параметры загрузки, записывает удалённое изменение и добавляет артефакт с переданными байтами. */ (params) => {
      state.writes.push({ method: 'uploadReleaseAsset', params });
      assert.equal(params.release_id, 7);
      assert.ok(Buffer.isBuffer(params.data));
      assert.equal(params.headers['content-length'], params.data.length);
      assert.equal(params.headers['content-type'], 'application/octet-stream');
      // asset описывает загруженный тестовый файл: id — новый идентификатор, name — имя запроса, bytes —
      // переданные байты, size — их длина, state — подтверждённое состояние uploaded, digest — SHA-256
      // этих байтов.
      const asset = {
        id: state.assets.length + 100, name: params.name, bytes: params.data,
        size: params.data.length, state: 'uploaded', digest: `sha256:${hash(params.data)}`,
      };
      state.assets.push(asset);
      return { data: asset };
    }),
  };
  // method обозначает операцию изменения выпуска, вызов которой должен завершить тестовый API ошибкой.
  for (const method of ['createRelease', 'updateRelease', 'deleteRelease', 'deleteReleaseAsset', 'updateReleaseAsset']) {
    repos[method] = endpoint(method, /* Фиксирует запрещённую операцию изменения выпуска и завершает тестовый вызов ошибкой. */ (params) => {
      state.writes.push({ method, params });
      throw new Error(`Запрещённая запись в тестовом API: ${method}`);
    });
  }
  // github объединяет подставные операции rest и обход paginate; repos обрабатывает выпуски и файлы, а
  // git — ссылки и объекты тегов.
  const github = {
    rest: {
      repos,
      git: {
        getRef: endpoint('getRef', /* Проверяет точную ссылку тега и возвращает управляемый объект Git. */ (params) => {
          assert.equal(params.ref, `tags/${version}`);
          return { data: { object: state.tag } };
        }),
        getTag: endpoint('getTag', /* Возвращает объект аннотированного тега из управляемой цепочки либо отклоняет неизвестный SHA. */ ({ tag_sha }) => {
          assert.ok(state.tags.has(tag_sha), 'Запрошен неизвестный объект тега.');
          return { data: { object: state.tags.get(tag_sha) } };
        }),
      },
    },
    // paginate обходит все страницы управляемой операции API по Link, сохраняя параметры и журнал вызовов;
    // незавершённый обход ограничен тысячей страниц.
    paginate: async (method, params) => {
      state.calls.push({ method: 'paginate', params });
      // result накапливает элементы всех прочитанных страниц в порядке их получения.
      const result = [];
      // number задаёт очередную страницу, начиная с 1; предел 1000 не позволяет подставному API зациклить
      // тест.
      for (let number = 1; number <= 1000; number++) {
        // response содержит очередную страницу API; data добавляется к результату, а headers.link определяет
        // продолжение обхода.
        const response = await method({ ...params, page: number });
        result.push(...response.data);
        if (!response.headers.link.includes('rel="next"')) return result;
      }
      throw new Error('Пагинация не завершилась.');
    },
  };
  // input объединяет аргументы проверяемой операции: github — управляемый API, dist — локальные файлы,
  // version и commit — ожидаемую идентичность, context — событие main и тестовый репозиторий, core —
  // получатели выходов и сообщений CI.
  const input = {
    github, dist, version, commit,
    context: {
      repo: { owner: 'test-owner', repo: 'test-repository' },
      ref: 'refs/heads/main',
      payload: { pull_request: { base: { ref: 'main' } } },
    },
    core: {
      // setOutput сохраняет последнее строковое значение выхода сценария CI для проверки результата.
      setOutput: (key, value) => { state.outputs[key] = value; },
      // info принимает диагностическое сообщение тестового сценария CI без физического вывода.
      info: () => {},
    },
  };
  return { input, state, allAssets, contents, draft };
}

// noWrites требует отсутствия любых удалённых изменений в журнале управляемого GitHub API.
function noWrites(state) {
  assert.deepEqual(state.writes, [], 'Проверка не должна изменять удалённый релиз.');
}

// Ожидания следуют требованиям безопасного повторного запуска и ручной публикации.
test('Новый main-релиз передаётся GoReleaser только после проверки локальных файлов и тега.', /* Проверяет передачу создания нового стабильного черновика GoReleaser после проверки тега и локальных файлов. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t, { releases: [] });
  await prepare(input);
  assert.deepEqual(state.outputs, { create: 'true', published: 'false' });
  noWrites(state);
});

test('Полный совпадающий Draft не создаётся повторно и проходит завершающую проверку.', /* Проверяет повторную подготовку и завершающую проверку полного совпадающего черновика без удалённых изменений. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t);
  await prepare(input);
  await verify(input);
  assert.deepEqual(state.outputs, { create: 'false', published: 'false' });
  noWrites(state);
});

test('Частичный Draft получает только отсутствующие файлы с исходными байтами, SHA256SUMS последним.', /* Проверяет дозагрузку только недостающих файлов с сохранением исходных байтов и последним SHA256SUMS. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария. allAssets содержит исходный полный набор тестовых артефактов, из которого
  // выбираются удалённые файлы сценария. contents хранит исходные байты тестовых файлов в порядке
  // names для независимого сравнения дозагрузки.
  const { input, state, allAssets, contents } = await fixture(t);
  state.assets = [allAssets[0]];
  await prepare(input);
  assert.deepEqual(state.writes.map(/* Извлекает имя каждого загруженного файла для проверки порядка дозагрузки. */ ({ params }) => params.name), names.slice(1));
  state.writes.forEach(/* Сравнивает переданные при дозагрузке байты с соответствующим локальным файлом. */ ({ params }, i) => assert.deepEqual(params.data, contents[i + 1]));
  assert.deepEqual(state.outputs, { create: 'false', published: 'false' });
  await verify(input);
});

test('Пустой Draft получает все три файла без создания другого релиза.', /* Проверяет загрузку всех трёх файлов в существующий пустой черновик без создания другого выпуска. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t, { assets: [] });
  await prepare(input);
  assert.deepEqual(state.writes.map(/* Извлекает имена загруженных файлов для проверки полного набора поставки. */ ({ params }) => params.name), names);
  assert.deepEqual(state.outputs, { create: 'false', published: 'false' });
});

// operation обозначает одну из двух проверяемых операций выпуска: подготовку либо завершающую
// сверку.
for (const operation of [prepare, verify]) {
  test(`${operation.name}: опубликованный релиз остаётся неизменным даже без локальных пакетов.`, /* Проверяет пропуск уже опубликованного выпуска выбранной операцией даже при отсутствии локальных пакетов. */ async (t) => {
    // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
    // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
    // результата сценария.
    const { input, state } = await fixture(t);
    state.releases[0].draft = false;
    input.dist = join(input.dist, 'absent');
    state.hooks.getRef = /* Отклоняет чтение тега, которое не должно выполняться для уже опубликованного выпуска. */ () => { throw new Error('Опубликованный релиз не требует тега.'); };
    await operation(input);
    assert.deepEqual(state.outputs, { create: 'false', published: 'true' });
    assert.equal(state.calls.some(/* Выявляет чтение тега или файлов выпуска, запрещённое в сценарии пропуска опубликованного выпуска. */ ({ method }) => ['getRef', 'listReleaseAssets'].includes(method)), false);
    noWrites(state);
  });

  // change содержит неподдерживаемое сочетание ветки и версии; label определяет описание сценария,
  // branch и version подставляются во вход операции.
  for (const change of [
    { label: 'ветка develop', branch: 'develop', version },
    { label: 'dev-версия', branch: 'main', version: 'v0.2.0-dev.1+abc123' },
    { label: 'prerelease', branch: 'main', version: 'v0.2.0-rc.1' },
    { label: 'неканоническая версия', branch: 'main', version: 'v00.2.0' },
  ]) {
    test(`${operation.name}: ${change.label} не допускает работу с GitHub Releases.`, /* Проверяет отклонение выбранной неподдерживаемой ветки либо версии до обращений к GitHub. */ async (t) => {
      // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
      // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
      // результата сценария.
      const { input, state } = await fixture(t);
      input.context.payload.pull_request.base.ref = change.branch;
      input.version = change.version;
      await assert.rejects(operation(input));
      assert.deepEqual(state.calls, []);
      noWrites(state);
    });
  }

  test(`${operation.name}: два релиза одного тега на разных страницах отклоняются до записи.`, /* Проверяет отказ двум выпускам одного тега, найденным на разных страницах, до удалённых изменений. */ async (t) => {
    // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
    // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
    // результата сценария. draft содержит исходное описание стабильного черновика, копии которого
    // используются в сценарии.
    const { input, state, draft } = await fixture(t);
    state.releases = [draft, { ...draft, id: 8 }];
    await assert.rejects(operation(input));
    assert.ok(state.calls.some(/* Проверяет чтение второй страницы выпусков перед выводом о неоднозначности тега. */ ({ method, params }) => method === 'listReleases' && params.page === 2));
    noWrites(state);
  });

  test(`${operation.name}: prerelease=true несовместим со стабильным Draft.`, /* Проверяет отказ стабильному черновику с prerelease=true без удалённых изменений. */ async (t) => {
    // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
    // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
    // результата сценария.
    const { input, state } = await fixture(t);
    state.releases[0].prerelease = true;
    await assert.rejects(operation(input));
    noWrites(state);
  });

  test(`${operation.name}: тег другого commit не разрешает запись.`, /* Проверяет отказ тегу другого коммита перед любыми удалёнными изменениями. */ async (t) => {
    // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
    // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
    // результата сценария.
    const { input, state } = await fixture(t);
    state.tag.sha = 'f'.repeat(40);
    await assert.rejects(operation(input));
    noWrites(state);
  });

  test(`${operation.name}: полный commit обязателен до запросов к GitHub.`, /* Проверяет отклонение сокращённого SHA до запросов к GitHub. */ async (t) => {
    // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
    // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
    // результата сценария.
    const { input, state } = await fixture(t);
    input.commit = commit.slice(0, 12);
    await assert.rejects(operation(input));
    assert.deepEqual(state.calls, []);
    noWrites(state);
  });

  // field выбирает конфликт метаданных артефакта, который должен остановить обработку до дозагрузки.
  for (const field of ['digest', 'size', 'state', 'duplicate', 'unexpected']) {
    test(`${operation.name}: конфликт ${field} найден до загрузки недостающего файла.`, /* Проверяет выбранный конфликт существующего артефакта до загрузки недостающих файлов. */ async (t) => {
      // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
      // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
      // результата сценария. allAssets содержит исходный полный набор тестовых артефактов, из которого
      // выбираются удалённые файлы сценария.
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

  // endpoint задаёт операцию чтения API, отказ которой воспроизводится в текущем сценарии.
  for (const endpoint of ['listReleases', 'getRef', 'listReleaseAssets']) {
    test(`${operation.name}: ошибка чтения ${endpoint} останавливает запись.`, /* Проверяет остановку выбранной операции при ошибке чтения API с сохранением исходного объекта ошибки. */ async (t) => {
      // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
      // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
      // результата сценария.
      const { input, state } = await fixture(t, { assets: [] });
      // failure хранит точный объект искусственной ошибки чтения для проверки передачи вызывающему коду
      // без подмены.
      const failure = new Error(`Тестовый отказ ${endpoint}`);
      state.hooks[endpoint] = /* Возвращает выбранную тестовую ошибку чтения API без выполнения штатного действия. */ () => { throw failure; };
      await assert.rejects(operation(input), /* Проверяет передачу того же объекта ошибки чтения без замены другим отказом. */ (error) => error === failure);
      noWrites(state);
    });
  }
}

test('Релиз на последующей странице находится без создания дубликата.', /* Проверяет нахождение выпуска и всех его файлов на последующих страницах без создания дубликата. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария. draft содержит исходное описание стабильного черновика, копии которого
  // используются в сценарии.
  const { input, state, draft } = await fixture(t);
  state.releases = [{ ...draft, id: 2, tag_name: 'v0.1.0' }, draft];
  await prepare(input);
  assert.equal(state.outputs.create, 'false');
  assert.ok(state.calls.some(/* Выявляет чтение второй страницы списка выпусков. */ ({ method, params }) => method === 'listReleases' && params.page === 2));
  assert.ok(state.calls.some(/* Выявляет чтение третьей страницы списка артефактов выпуска. */ ({ method, params }) => method === 'listReleaseAssets' && params.page === 3));
  noWrites(state);
});

test('Без digest совпадение подтверждается скачанными двоичными данными.', /* Проверяет сверку файлов по скачанным двоичным данным при отсутствии digest, включая массив и представление с отступом. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t);
  state.assets.forEach(/* Убирает digest и задаёт разные представления двоичных данных для проверки точного расчёта SHA-256. */ (asset, i) => {
    delete asset.digest;
    if (i === 1) asset.bytes = Uint8Array.from(asset.bytes).buffer;
    if (i === 2) {
      // padded содержит исходные байты между посторонними префиксом и суффиксом, чтобы проверить границы
      // представления Uint8Array.
      const padded = Buffer.concat([Buffer.from('pad'), asset.bytes, Buffer.from('tail')]);
      asset.bytes = new Uint8Array(padded.buffer, padded.byteOffset + 3, asset.size);
    }
  });
  await verify(input);
  assert.equal(state.calls.filter(/* Отбирает скачивания артефактов для проверки сверки каждого файла без digest. */ ({ method }) => method === 'getReleaseAsset').length, 3);
  noWrites(state);
});

test('Без digest несовпадающие байты того же размера не заменяются.', /* Проверяет отказ файлу того же размера с другими байтами без замены или дозагрузки. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t);
  state.assets = [state.assets[0]];
  delete state.assets[0].digest;
  state.assets[0].bytes = Buffer.alloc(state.assets[0].size, 65);
  await assert.rejects(prepare(input));
  noWrites(state);
});

test('Отказ скачивания вместо digest останавливает загрузку остальных файлов.', /* Проверяет прекращение дозагрузки при отказе скачивания файла без digest. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t);
  state.assets = [state.assets[0]];
  delete state.assets[0].digest;
  state.hooks.getReleaseAsset = /* Имитирует ошибку скачивания, которая должна остановить сверку и последующие загрузки. */ () => { throw new Error('Тестовый отказ скачивания.'); };
  await assert.rejects(prepare(input));
  noWrites(state);
});

test('Аннотированный тег разрешается до проверенного commit.', /* Проверяет разрешение аннотированного тега до точного проверенного коммита. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t);
  state.tag = { type: 'tag', sha: 'a'.repeat(40) };
  state.tags.set(state.tag.sha, { type: 'commit', sha: commit });
  await verify(input);
  assert.equal(state.calls.filter(/* Отбирает чтения аннотированных тегов для проверки числа шагов разрешения. */ ({ method }) => method === 'getTag').length, 1);
  noWrites(state);
});

test('Цикл аннотированных тегов прекращает проверку без записи.', /* Проверяет остановку на цикле аннотированных тегов без удалённых изменений. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t, { assets: [] });
  state.tag = { type: 'tag', sha: 'a'.repeat(40) };
  state.tags.set(state.tag.sha, state.tag);
  await assert.rejects(prepare(input));
  noWrites(state);
});

// readNumber задаёт номер повторного чтения выпуска, при котором сценарий имитирует ручную
// публикацию.
for (const readNumber of [1, 2]) {
  test(`Ручная публикация при повторном чтении ${readNumber} запрещает следующую загрузку.`, /* Проверяет прекращение дозагрузки при ручной публикации на выбранном повторном чтении выпуска. */ async (t) => {
    // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
    // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
    // результата сценария.
    const { input, state } = await fixture(t, { assets: [] });
    // reads считает повторные чтения выпуска до выбранного момента ручной публикации.
    let reads = 0;
    state.hooks.getRelease = /* Переводит черновик в опубликованный выпуск на выбранном чтении его состояния. */ () => {
      if (++reads === readNumber) state.releases[0].draft = false;
    };
    await prepare(input);
    assert.equal(state.outputs.published, 'true');
    noWrites(state);
  });
}

test('Изменение тега между проверками не разрешает загрузку.', /* Проверяет отказ дозагрузке после смены тега между повторными проверками. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t, { assets: [] });
  // reads считает чтения ссылки тега; на втором чтении целевой коммит подменяется.
  let reads = 0;
  state.hooks.getRef = /* Подменяет целевой коммит при втором чтении тега. */ () => {
    if (++reads === 2) state.tag = { type: 'commit', sha: 'f'.repeat(40) };
  };
  await assert.rejects(prepare(input));
  noWrites(state);
});

test('Файл, появившийся между чтениями Draft, не загружается повторно.', /* Проверяет отсутствие повторной загрузки файлов, появившихся между чтениями черновика. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария. allAssets содержит исходный полный набор тестовых артефактов, из которого
  // выбираются удалённые файлы сценария.
  const { input, state, allAssets } = await fixture(t, { assets: [] });
  state.hooks.getRelease = /* Добавляет полный набор артефактов при повторном чтении выпуска для воспроизведения конкурентной дозагрузки. */ () => { state.assets = allAssets.map(/* Создаёт отдельное описание появившегося артефакта из исходного полного набора. */ (asset) => ({ ...asset })); };
  await prepare(input);
  noWrites(state);
  assert.equal(state.outputs.create, 'false');
});

test('Ошибка повторного чтения Draft не разрешает загрузку.', /* Проверяет прекращение дозагрузки при ошибке повторного чтения черновика. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t, { assets: [] });
  state.hooks.getRelease = /* Имитирует ошибку повторного чтения состояния выпуска перед загрузкой. */ () => { throw new Error('Тестовый отказ повторного чтения.'); };
  await assert.rejects(prepare(input));
  noWrites(state);
});

test('Ошибка upload не допускает продолжения и ложного подтверждения готовности.', /* Проверяет остановку после первой ошибки загрузки без ложных признаков готовности выпуска. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t, { assets: [] });
  state.hooks.uploadReleaseAsset = /* Имитирует ошибку загрузки артефакта до изменения тестового удалённого набора. */ () => { throw new Error('Тестовый отказ upload.'); };
  await assert.rejects(prepare(input));
  assert.equal(state.calls.filter(/* Отбирает попытки загрузки, чтобы проверить отсутствие продолжения после первой ошибки. */ ({ method }) => method === 'uploadReleaseAsset').length, 1);
  assert.equal(state.outputs.create, undefined);
  assert.equal(state.outputs.published, undefined);
});

test('Завершающая проверка требует найденный Draft со всеми тремя файлами.', /* Проверяет отказ завершающей проверки при отсутствии выпуска либо обязательных файлов черновика. */ async (t) => {
  // releases задаёт отсутствие выпуска либо существующий черновик без файлов для проверки отказа
  // завершающей сверки.
  for (const releases of [[], [{ id: 7, tag_name: version, draft: true, prerelease: false }]]) {
    // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
    // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
    // результата сценария.
    const { input, state } = await fixture(t, { releases, assets: [] });
    await assert.rejects(verify(input));
    noWrites(state);
  }
});

test('Отсутствие локального пакета не разрешает создание нового релиза.', /* Проверяет отсутствие разрешения создания выпуска при недостающем локальном пакете. */ async (t) => {
  // input содержит параметры выпуска, управляемый API и локальные файлы текущего сценария. state
  // хранит изменяемое удалённое состояние, подставленные действия и журналы API для проверки
  // результата сценария.
  const { input, state } = await fixture(t, { releases: [] });
  await rm(join(input.dist, names[1]));
  await assert.rejects(prepare(input));
  assert.equal(state.outputs.create, undefined);
  noWrites(state);
});

// stateQueries задаёт точный порядок чтения LoadState и ActiveState при проверке остановки службы.
const stateQueries = [
  'show --property=LoadState --value gost-restreamer.service',
  'show --property=ActiveState --value gost-restreamer.service',
];
// listUnitFiles задаёт ожидаемую команду проверки наличия файла службы перед отключением
// автозапуска.
const listUnitFiles = 'list-unit-files --no-legend --no-pager --full gost-restreamer.service';
// stopService задаёт ожидаемую команду остановки службы и выбирается как точка искусственного
// отказа.
const stopService = 'stop gost-restreamer.service';
// disableService задаёт ожидаемую команду отключения автозапуска при удалении пакета.
const disableService = 'disable gost-restreamer.service';

// maintainerFixture создаёт изолированную копию скрипта сопровождения пакета и подставной systemctl.
// В копии заменяется только путь проверки systemd; guard задаёт этот путь, log — журнал команд,
// run запускает скрипт с выбранным действием, commands возвращает его обращения к systemctl.
async function maintainerFixture(t, scriptName, settings = {}) {
  // root указывает на частный каталог копии скрипта, подставного systemctl и журнала его команд.
  const root = await mkdtemp(join(tmpdir(), 'gost-maintainer-test-'));
  t.after(/* Удаляет изолированные скрипты и журнал подставного systemctl после теста. */ () => rm(root, { recursive: true, force: true }));
  // script содержит исходный текст выбранного скрипта сопровождения пакета до замены пути проверки
  // systemd.
  const script = await readFile(join(__dirname, '../../deploy/debian', scriptName), 'utf8');
  // guard задаёт частный путь, существование которого имитирует наличие systemd в тесте.
  const guard = join(root, 'systemd');
  assert.equal(script.split('/run/systemd/system').length - 1, 1);
  // scriptPath указывает на изолированную копию проверяемого скрипта с заменённым путём systemd.
  const scriptPath = join(root, scriptName);
  await writeFile(scriptPath, script.replace('/run/systemd/system', `'${guard.replaceAll("'", "'\\''")}'`));
  // bin задаёт каталог подставного systemctl, который первым включается в PATH тестовой команды.
  const bin = join(root, 'bin');
  await mkdir(bin);
  // log указывает на файл, в который подставной systemctl записывает точные аргументы вызовов.
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
  // env задаёт изолированную среду скрипта: PATH выбирает подставной systemctl, SYSTEMCTL_TEST_LOG —
  // журнал, SYSTEMCTL_TEST_LOAD и SYSTEMCTL_TEST_ACTIVE — состояния службы, SYSTEMCTL_TEST_FILES —
  // список файлов, SYSTEMCTL_TEST_FAIL — команду искусственного отказа; settings переопределяет эти
  // значения для сценария.
  const env = {
    PATH: `${bin}:/usr/bin:/bin`, SYSTEMCTL_TEST_LOG: log,
    SYSTEMCTL_TEST_LOAD: 'loaded', SYSTEMCTL_TEST_ACTIVE: 'active',
    SYSTEMCTL_TEST_FILES: 'gost-restreamer.service enabled enabled', SYSTEMCTL_TEST_FAIL: '',
    ...settings,
  };
  return {
    guard, log,
    // run запускает копию скрипта сопровождения пакета с заданным действием и управляемой средой, позволяя
    // сценарию заменить отдельные значения systemctl.
    run: (action, args = [], overrides = {}) => execute('/bin/sh', [scriptPath, action, ...args], { env: { ...env, ...overrides } }),
    // commands возвращает записанные команды подставного systemctl; отсутствие файла журнала означает
    // отсутствие обращений.
    commands: async () => {
      // actual содержит журнал команд systemctl либо пустую строку, если обращений ещё не было.
      const actual = await readFile(log, 'utf8').catch(/* Считает отсутствующий файл журнала отсутствием вызовов systemctl, сохраняя остальные ошибки чтения. */ (error) => {
        if (error.code === 'ENOENT') return '';
        throw error;
      });
      return actual.trim() ? actual.trim().split('\n') : [];
    },
  };
}

// scriptName выбирает скрипт сопровождения пакета; scenarios содержит действия пакета и точные
// ожидаемые команды для него.
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
  test(`${scriptName}: команды systemctl соответствуют install/upgrade/remove и никогда не запускают сервис.`, /* Проверяет точные команды выбранного скрипта сопровождения пакета при наличии и отсутствии systemd без запуска службы. */ async (t) => {
    // subject содержит изолированную копию выбранного скрипта, путь признака systemd и операции запуска
    // и чтения журнала.
    const subject = await maintainerFixture(t, scriptName);
    // action обозначает очередное действие пакета; expected содержит разрешённые команды systemctl для
    // этого действия.
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

// action выбирает поддерживаемый путь остановки службы: remove, upgrade, deconfigure либо
// failed-upgrade.
for (const action of ['remove', 'upgrade', 'deconfigure', 'failed-upgrade']) {
  // load задаёт LoadState, active — ActiveState, files — ответ о файле службы; expectedStop и
  // expectedDisable определяют ожидаемую необходимость остановки и отключения автозапуска.
  for (const [load, active, files, expectedStop, expectedDisable] of [
    ['not-found', 'inactive', '', false, false],
    ['loaded', 'active', 'gost-restreamer.service enabled enabled', true, true],
    ['loaded', 'inactive', 'gost-restreamer.service disabled enabled', true, true],
    ['not-found', 'active', '', true, false],
    ['loaded', 'active', '', true, false],
    ['not-found', 'inactive', 'gost-restreamer.service disabled enabled', false, true],
  ]) {
    test(`prerm ${action}: ${load}/${active}, unit ${files ? 'присутствует' : 'отсутствует'}.`, /* Проверяет команды prerm для выбранного действия и сочетания наличия, загрузки и активности службы. */ async (t) => {
      // subject содержит копию prerm со средой, имитирующей выбранные наличие, загрузку и активность
      // службы.
      const subject = await maintainerFixture(t, 'prerm', {
        SYSTEMCTL_TEST_LOAD: load, SYSTEMCTL_TEST_ACTIVE: active, SYSTEMCTL_TEST_FILES: files,
      });
      await mkdir(subject.guard);
      await subject.run(action);
      // expected накапливает точную последовательность команд prerm для текущего действия и состояния
      // службы.
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

// failure выбирает команду systemctl, которая вернёт отказ; expected содержит выполненный до этого
// момента префикс команд.
for (const [failure, expected] of [
  [stateQueries[0], stateQueries.slice(0, 1)],
  [stateQueries[1], stateQueries],
  [stopService, [...stateQueries, stopService]],
  [listUnitFiles, [...stateQueries, stopService, listUnitFiles]],
  [disableService, [...stateQueries, stopService, listUnitFiles, disableService]],
]) {
  test(`prerm: отказ ${failure} возвращается вызывающему процессу и останавливает дальнейшие команды.`, /* Проверяет передачу выбранной ошибки systemctl и отсутствие последующих команд prerm. */ async (t) => {
    // subject содержит копию prerm с искусственным отказом выбранной команды systemctl.
    const subject = await maintainerFixture(t, 'prerm', { SYSTEMCTL_TEST_FAIL: failure });
    await mkdir(subject.guard);
    await assert.rejects(subject.run('remove'), { code: 23 });
    assert.deepEqual(await subject.commands(), expected);
  });
}

// Debian Policy 6.6 разрешает распаковку после успешного new-prerm failed-upgrade.
// Обе попытки обязаны сообщить об отказе остановки, чтобы не разрешить замену работающих бинарников.
// failure задаёт отказавшую команду проверки или остановки; expected содержит команды, которые
// допустимы до отказа в каждой попытке обновления.
for (const [failure, expected] of [
  [stateQueries[0], stateQueries.slice(0, 1)],
  [stateQueries[1], stateQueries],
  [stopService, [...stateQueries, stopService]],
]) {
  test(`prerm: upgrade и failed-upgrade не разрешают продолжение после отказа ${failure}.`, /* Проверяет отказ upgrade и failed-upgrade после выбранной неуспешной операции остановки службы. */ async (t) => {
    // subject содержит копию prerm, в которой один и тот же отказ проверяется для upgrade и
    // failed-upgrade.
    const subject = await maintainerFixture(t, 'prerm', { SYSTEMCTL_TEST_FAIL: failure });
    await mkdir(subject.guard);
    await assert.rejects(subject.run('upgrade', ['0.3.0']), { code: 23 });
    await assert.rejects(subject.run('failed-upgrade', ['0.2.0', '0.3.0']), { code: 23 });
    assert.deepEqual(await subject.commands(), [...expected, ...expected]);
  });
}

test('prerm: failed-upgrade разрешает продолжение после успешной повторной остановки.', /* Проверяет успешную повторную остановку через failed-upgrade после первоначального отказа upgrade. */ async (t) => {
  // subject содержит копию prerm с первоначальным отказом остановки, который снимается перед повторной
  // попыткой.
  const subject = await maintainerFixture(t, 'prerm', { SYSTEMCTL_TEST_FAIL: stopService });
  await mkdir(subject.guard);
  await assert.rejects(subject.run('upgrade', ['0.3.0']), { code: 23 });
  await subject.run('failed-upgrade', ['0.2.0', '0.3.0'], { SYSTEMCTL_TEST_FAIL: '' });
  assert.deepEqual(await subject.commands(), [
    ...stateQueries, stopService, ...stateQueries, stopService,
  ]);
});

// reproducibilityFixture создаёт изолированную поставку с известными датами файлов и подставным make
// для проверки повторной упаковки. Проверка restored требует точного восстановления байтов, дат и
// удаления собственного временного каталога.
async function reproducibilityFixture(t, mode = 'unchanged') {
  // root указывает на частный каталог синтетической поставки и подставных инструментов повторной
  // упаковки.
  const root = await mkdtemp(join(tmpdir(), 'gost-reproducibility-test-'));
  t.after(/* Удаляет изолированные файлы и инструменты проверки повторной упаковки после теста. */ () => rm(root, { recursive: true, force: true }));
  // config указывает на YAML-файл синтетической поставки, исходные байты и время которого проверяются
  // после сценария.
  const config = join(root, 'configs/example.yaml');
  // unit указывает на файл службы синтетической поставки, время которого меняется и затем должно
  // восстановиться.
  const unit = join(root, 'deploy/systemd/gost-restreamer.service');
  await mkdir(join(root, 'configs'));
  await mkdir(join(root, 'deploy/systemd'), { recursive: true });
  await writeFile(config, 'config: synthetic\n');
  await writeFile(unit, '[Unit]\nDescription=synthetic\n');
  await utimes(config, 1600000000.125, 1600000000.125);
  await utimes(unit, 1700000000.375, 1700000000.375);
  // originals хранит исходные пути, байты и времена изменения YAML и файла службы для независимой
  // проверки восстановления.
  const originals = await Promise.all([config, unit].map(/* Сохраняет исходные байты и точное время изменения каждого файла для проверки восстановления. */ async (path) => ({
    path, bytes: await readFile(path), mtime: (await stat(path, { bigint: true })).mtimeNs,
  })));
  await mkdir(join(root, 'dist'));
  // contents хранит исходные синтетические байты архива, Debian-пакета и добавляемого SHA256SUMS в
  // порядке names.
  const contents = ['synthetic archive\n', 'synthetic deb\n'];
  contents.push(contents.map(/* Формирует строку SHA256SUMS для исходных синтетических пакетов. */ (data, i) => `${hash(data)}  ${names[i]}\n`).join(''));
  // i связывает имя очередного файла поставки с соответствующим содержимым в contents.
  for (let i = 0; i < names.length; i++) await writeFile(join(root, 'dist', names[i]), contents[i]);
  // bin задаёт каталог подставных make и mktemp для управляемой проверки настоящего сценария повторной
  // упаковки.
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
  // env задаёт среду повторной упаковки: PATH выбирает подставные инструменты; VERSION, GORELEASER,
  // CGO_ENABLED, GOOS, GOARCH, GOTOOLCHAIN, GOWORK и GOFLAGS проверяют сохранение параметров сборки;
  // REPRO_TEST_ROOT и REPRO_TEST_MODE управляют сценарием, REPRO_TEST_ARCHIVE, REPRO_TEST_DEB и
  // REPRO_TEST_SUMS называют проверяемые файлы.
  const env = {
    PATH: `${bin}:/usr/bin:/bin:/sbin`, VERSION: version,
    GORELEASER: '/pinned/test/goreleaser', CGO_ENABLED: '1', GOOS: 'linux', GOARCH: 'amd64',
    GOTOOLCHAIN: 'local', GOWORK: 'off', GOFLAGS: '-mod=readonly', REPRO_TEST_ROOT: root,
    REPRO_TEST_MODE: mode, REPRO_TEST_ARCHIVE: names[0], REPRO_TEST_DEB: names[1], REPRO_TEST_SUMS: names[2],
  };
  return {
    root, originals,
    // run выполняет настоящий сценарий повторной упаковки в изолированном каталоге с управляемыми
    // инструментами и средой.
    run: () => execute('/bin/sh', [join(__dirname, 'package-reproducibility.sh')], { cwd: root, env }),
    // restored требует исходные байты и точные времена YAML и файла службы после сценария и отсутствие
    // созданного временного каталога.
    restored: async () => {
      // original обозначает очередной исходный файл, байты и точное время которого сравниваются после
      // сценария.
      for (const original of originals) {
        assert.deepEqual(await readFile(original.path), original.bytes);
        assert.equal((await stat(original.path, { bigint: true })).mtimeNs, original.mtime);
      }
      // temporary содержит путь временного каталога, созданного настоящим сценарием; после восстановления
      // он должен отсутствовать.
      const temporary = (await readFile(join(root, 'temp.path'), 'utf8')).trim();
      assert.equal(temporary.startsWith('/tmp/gost-package-reproducibility.'), true);
      await assert.rejects(stat(temporary), { code: 'ENOENT' });
    },
  };
}

test('Повторная упаковка меняет даты checkout, сохраняет среду сборки и восстанавливает исходные даты.', /* Проверяет изменение дат при повторной упаковке, точную среду сборки и восстановление исходных файлов. */ async (t) => {
  // subject содержит изолированную поставку и операции запуска сценария и проверки восстановления.
  const subject = await reproducibilityFixture(t);
  await subject.run();
  assert.deepEqual((await readFile(join(subject.root, 'make.log'), 'utf8')).trim().split('\n'), [
    'package-debian', `VERSION=${version}`, '/pinned/test/goreleaser', '1', 'linux', 'amd64',
    'local', 'off', '-mod=readonly',
  ]);
  // i связывает наблюдавшееся время с соответствующим исходным файлом; file обозначает копию отметки
  // времени, увиденного подставным make.
  for (const [i, file] of ['config.seen', 'unit.seen'].entries()) {
    assert.notEqual((await stat(join(subject.root, file), { bigint: true })).mtimeNs, subject.originals[i].mtime);
  }
  await subject.restored();
});

// mode выбирает изменение архива, Debian-пакета, порядка манифеста либо отказ make, которое сценарий
// должен обнаружить.
for (const mode of ['archive', 'deb', 'manifest', 'failure']) {
  test(`Повторная упаковка отклоняет ${mode === 'failure' ? 'отказ make' : `изменение ${mode}`} и восстанавливает исходные даты.`, /* Проверяет отказ выбранному изменению пакета или манифеста либо ошибке make с восстановлением исходных дат. */ async (t) => {
    // subject содержит изолированную поставку с выбранным искусственным изменением либо отказом
    // упаковки.
    const subject = await reproducibilityFixture(t, mode);
    await assert.rejects(subject.run(), mode === 'failure' ? { code: 27 } : undefined);
    await subject.restored();
  });
}

// missing задаёт входной файл, отсутствие которого должно остановить сценарий до изменения времён и
// вызова make.
for (const missing of [...names.map(/* Преобразует имя пакета в проверяемый путь внутри dist. */ (name) => `dist/${name}`), 'configs/example.yaml', 'deploy/systemd/gost-restreamer.service']) {
  test(`Повторная упаковка без ${missing} останавливается до изменения дат или запуска make.`, /* Проверяет остановку до изменения дат и запуска make при отсутствии выбранного входного файла. */ async (t) => {
    // subject содержит исходную изолированную поставку, из которой удаляется выбранный обязательный
    // вход.
    const subject = await reproducibilityFixture(t);
    await rm(join(subject.root, missing));
    await assert.rejects(subject.run());
    await assert.rejects(readFile(join(subject.root, 'make.log')), { code: 'ENOENT' });
    // original обозначает сохранившийся исходный файл, который не должен измениться после раннего отказа
    // сценария.
    for (const original of subject.originals.filter(/* Исключает удалённый вход из проверки сохранности оставшихся исходных файлов. */ ({ path }) => path !== join(subject.root, missing))) {
      assert.deepEqual(await readFile(original.path), original.bytes);
      assert.equal((await stat(original.path, { bigint: true })).mtimeNs, original.mtime);
    }
  });
}
