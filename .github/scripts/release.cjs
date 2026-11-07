'use strict';

const { createHash } = require('node:crypto');
const { readFile } = require('node:fs/promises');
const { join } = require('node:path');

function parameters({ github, context, core, version, commit, dist = 'dist' }) {
  const branch = context.payload?.pull_request
    ? context.payload.pull_request.base?.ref
    : context.ref?.replace(/^refs\/heads\//, '');
  if (branch !== 'main' || !/^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(version)) {
    throw new Error('Draft Release разрешён только для стабильной версии ветки main.');
  }
  if (!/^[0-9a-f]{40}$/.test(commit)) {
    throw new Error('Для релиза нужен полный SHA-1 проверенного коммита.');
  }
  return { github, core, version, commit, dist, repo: context.repo };
}

function outputs(options, create, published) {
  options.core.setOutput('create', String(create));
  options.core.setOutput('published', String(published));
}

function published(options, release) {
  if (release.draft !== false) return false;
  outputs(options, false, true);
  options.core.info(`Релиз ${options.version} уже опубликован; изменения пропущены.`);
  return true;
}

function checkDraft(options, release) {
  if (release.tag_name !== options.version || release.draft !== true || release.prerelease !== false) {
    throw new Error('Релиз должен быть Draft с точным стабильным тегом и prerelease=false.');
  }
}

async function findRelease(options) {
  const releases = await options.github.paginate(options.github.rest.repos.listReleases, {
    ...options.repo, per_page: 100,
  });
  const matches = releases.filter((release) => release.tag_name === options.version);
  if (matches.length > 1) {
    throw new Error(`Найдено несколько релизов для ${options.version}; изменения запрещены.`);
  }
  return matches[0];
}

async function readRelease(options, id) {
  const { data } = await options.github.rest.repos.getRelease({ ...options.repo, release_id: id });
  if (data.tag_name !== options.version) {
    throw new Error('Тег релиза изменился во время проверки.');
  }
  return data;
}

async function checkTag(options) {
  const { data } = await options.github.rest.git.getRef({
    ...options.repo, ref: `tags/${options.version}`,
  });
  let object = data.object;
  const visited = new Set();
  while (object.type === 'tag') {
    if (visited.has(object.sha)) throw new Error('Обнаружен цикл объектов Git-тега.');
    visited.add(object.sha);
    const tag = await options.github.rest.git.getTag({ ...options.repo, tag_sha: object.sha });
    object = tag.data.object;
  }
  if (object.type !== 'commit' || object.sha !== options.commit) {
    throw new Error(`Тег ${options.version} не указывает на проверенный коммит.`);
  }
}

function sha256(data) {
  return createHash('sha256').update(data).digest('hex');
}

async function localAssets(options) {
  const debVersion = options.version.slice(1).replace('-dev.', '~dev.');
  const names = [
    `gost-restreamer-${options.version}-linux-amd64.tar.gz`,
    `gost-restreamer_${debVersion}_amd64.deb`,
    `gost-restreamer-${options.version}-SHA256SUMS`,
  ];
  const assets = new Map();
  for (const name of names) {
    const data = await readFile(join(options.dist, name));
    assets.set(name, { data, size: data.length, hash: sha256(data) });
  }
  return assets;
}

async function remoteHash(options, asset) {
  if (/^sha256:[0-9a-f]{64}$/i.test(asset.digest)) return asset.digest.slice(7).toLowerCase();
  const { data } = await options.github.rest.repos.getReleaseAsset({
    ...options.repo, asset_id: asset.id, headers: { accept: 'application/octet-stream' },
  });
  let bytes;
  if (data instanceof ArrayBuffer) bytes = Buffer.from(data);
  else if (ArrayBuffer.isView(data)) bytes = Buffer.from(data.buffer, data.byteOffset, data.byteLength);
  else throw new Error(`Не получены двоичные данные артефакта ${asset.name}.`);
  if (bytes.length !== asset.size) throw new Error(`Размер скачанного ${asset.name} не совпадает с API.`);
  return sha256(bytes);
}

async function checkAssets(options, release, expected, complete) {
  const assets = await options.github.paginate(options.github.rest.repos.listReleaseAssets, {
    ...options.repo, release_id: release.id, per_page: 100,
  });
  const found = new Set();
  for (const asset of assets) {
    const local = expected.get(asset.name);
    if (!local || found.has(asset.name)) {
      throw new Error(`Неожиданный или повторный артефакт релиза: ${asset.name}.`);
    }
    found.add(asset.name);
    if (asset.state !== 'uploaded' || asset.size !== local.size) {
      throw new Error(`Состояние или размер артефакта ${asset.name} не совпадает; замена запрещена.`);
    }
    if (await remoteHash(options, asset) !== local.hash) {
      throw new Error(`SHA-256 артефакта ${asset.name} не совпадает; замена запрещена.`);
    }
  }
  if (complete && found.size !== expected.size) {
    throw new Error('В Draft Release отсутствуют обязательные артефакты.');
  }
  return found;
}

async function prepare(input) {
  const options = parameters(input);
  let release = await findRelease(options);
  if (release && published(options, release)) return;
  if (release) checkDraft(options, release);
  await checkTag(options);
  const expected = await localAssets(options);
  if (!release) {
    outputs(options, true, false);
    return;
  }

  let found = await checkAssets(options, release, expected, false);
  for (const [name, asset] of expected) {
    if (found.has(name)) continue;
    release = await readRelease(options, release.id);
    if (published(options, release)) return;
    checkDraft(options, release);
    found = await checkAssets(options, release, expected, false);
    if (found.has(name)) continue;
    await checkTag(options);
    release = await readRelease(options, release.id);
    if (published(options, release)) return;
    checkDraft(options, release);
    await options.github.rest.repos.uploadReleaseAsset({
      ...options.repo,
      release_id: release.id,
      name,
      data: asset.data,
      headers: { 'content-type': 'application/octet-stream', 'content-length': asset.size },
    });
  }
  release = await readRelease(options, release.id);
  if (published(options, release)) return;
  checkDraft(options, release);
  await checkAssets(options, release, expected, true);
  outputs(options, false, false);
}

async function verify(input) {
  const options = parameters(input);
  let release = await findRelease(options);
  if (!release) throw new Error(`Релиз ${options.version} не найден.`);
  if (published(options, release)) return;
  checkDraft(options, release);
  await checkTag(options);
  const expected = await localAssets(options);
  await checkAssets(options, release, expected, true);
  release = await readRelease(options, release.id);
  if (published(options, release)) return;
  checkDraft(options, release);
  outputs(options, false, false);
  options.core.info(`Draft Release ${options.version} содержит все проверенные артефакты.`);
}

module.exports = { prepare, verify };
