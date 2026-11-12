'use strict';

const { createHash } = require('node:crypto');
const { readFile } = require('node:fs/promises');
const { join } = require('node:path');

// parameters допускает операции выпуска только для main, канонической стабильной версии и полного SHA
// проверенного коммита; неверные параметры отклоняются до GitHub-запросов.
function parameters({ github, context, core, version, commit, dist = 'dist' }) {
  // branch хранит целевую ветку pull request либо ветку события без префикса refs/heads/ для проверки
  // допуска к стабильному выпуску.
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

// outputs передаёт сценарию CI признаки создания нового черновика и уже опубликованного выпуска
// строковыми значениями.
function outputs(options, create, published) {
  options.core.setOutput('create', String(create));
  options.core.setOutput('published', String(published));
}

// published распознаёт уже опубликованный выпуск, выставляет признак пропуска и завершает дальнейшую
// обработку без изменения файлов.
function published(options, release) {
  if (release.draft !== false) return false;
  outputs(options, false, true);
  options.core.info(`Релиз ${options.version} уже опубликован; изменения пропущены.`);
  return true;
}

// checkDraft требует точный тег, draft=true и prerelease=false перед работой со стабильным черновиком.
function checkDraft(options, release) {
  if (release.tag_name !== options.version || release.draft !== true || release.prerelease !== false) {
    throw new Error('Релиз должен быть Draft с точным стабильным тегом и prerelease=false.');
  }
}

// findRelease читает все страницы выпусков и возвращает единственный выпуск точного тега;
// неоднозначность останавливает обработку.
async function findRelease(options) {
  // releases содержит выпуски со всех страниц API, среди которых ищется точный тег.
  const releases = await options.github.paginate(options.github.rest.repos.listReleases, {
    ...options.repo, per_page: 100,
  });
  // matches содержит только выпуски запрошенного тега; больше одного элемента означает
  // неоднозначность.
  const matches = releases.filter(/* Отбирает выпуски с точным тегом запрошенной версии перед проверкой единственности. */ (release) => release.tag_name === options.version);
  if (matches.length > 1) {
    throw new Error(`Найдено несколько релизов для ${options.version}; изменения запрещены.`);
  }
  return matches[0];
}

// readRelease перечитывает выпуск по ID и отклоняет сменившийся тег перед следующей операцией.
async function readRelease(options, id) {
  // data содержит перечитанное состояние выпуска, тег которого проверяется перед следующей операцией.
  const { data } = await options.github.rest.repos.getRelease({ ...options.repo, release_id: id });
  if (data.tag_name !== options.version) {
    throw new Error('Тег релиза изменился во время проверки.');
  }
  return data;
}

// checkTag разрешает неаннотированный тег или цепочку аннотированных тегов до точного проверенного
// коммита, отклоняя цикл и другой объект.
async function checkTag(options) {
  // data содержит ссылку Git запрошенного тега и начальный объект для разрешения до коммита.
  const { data } = await options.github.rest.git.getRef({
    ...options.repo, ref: `tags/${options.version}`,
  });
  // object хранит текущий объект цепочки тегов; после обхода он должен обозначать проверенный коммит.
  let object = data.object;
  // visited хранит SHA уже прочитанных объектов тегов для обнаружения цикла.
  const visited = new Set();
  while (object.type === 'tag') {
    if (visited.has(object.sha)) throw new Error('Обнаружен цикл объектов Git-тега.');
    visited.add(object.sha);
    // tag содержит прочитанный объект аннотированного тега, из которого берётся следующая ссылка.
    const tag = await options.github.rest.git.getTag({ ...options.repo, tag_sha: object.sha });
    object = tag.data.object;
  }
  if (object.type !== 'commit' || object.sha !== options.commit) {
    throw new Error(`Тег ${options.version} не указывает на проверенный коммит.`);
  }
}

// sha256 возвращает SHA-256 переданных байтов в шестнадцатеричной записи.
function sha256(data) {
  return createHash('sha256').update(data).digest('hex');
}

// localAssets читает три ожидаемых файла поставки и сохраняет исходные байты, размеры и SHA-256 для
// сверки и дозагрузки.
async function localAssets(options) {
  // debVersion содержит версию для имени Debian-пакета без начального v и с разделителем ~dev. для
  // сборки разработки.
  const debVersion = options.version.slice(1).replace('-dev.', '~dev.');
  // names задаёт имена архива, Debian-пакета и SHA256SUMS в порядке их загрузки; контрольные суммы
  // идут последними.
  const names = [
    `gost-restreamer-${options.version}-linux-amd64.tar.gz`,
    `gost-restreamer_${debVersion}_amd64.deb`,
    `gost-restreamer-${options.version}-SHA256SUMS`,
  ];
  // assets связывает имя каждого локального файла с его исходными байтами, размером и SHA-256 для
  // проверки удалённого набора.
  const assets = new Map();
  // name обозначает очередное ожидаемое имя файла, читаемого из каталога поставки.
  for (const name of names) {
    // data содержит исходные байты очередного локального файла; именно они используются для сверки и
    // дозагрузки.
    const data = await readFile(join(options.dist, name));
    assets.set(name, { data, size: data.length, hash: sha256(data) });
  }
  return assets;
}

// remoteHash использует контрольную сумму SHA-256 из корректного поля digest API GitHub либо скачивает
// двоичный файл и проверяет его размер перед расчётом хеша.
async function remoteHash(options, asset) {
  if (/^sha256:[0-9a-f]{64}$/i.test(asset.digest)) return asset.digest.slice(7).toLowerCase();
  // data содержит ответ скачивания артефакта, который должен представлять двоичные данные
  // поддерживаемого вида.
  const { data } = await options.github.rest.repos.getReleaseAsset({
    ...options.repo, asset_id: asset.id, headers: { accept: 'application/octet-stream' },
  });
  // bytes хранит точное представление скачанного файла в Buffer, включая границы исходного
  // представления массива.
  let bytes;
  if (data instanceof ArrayBuffer) bytes = Buffer.from(data);
  else if (ArrayBuffer.isView(data)) bytes = Buffer.from(data.buffer, data.byteOffset, data.byteLength);
  else throw new Error(`Не получены двоичные данные артефакта ${asset.name}.`);
  if (bytes.length !== asset.size) throw new Error(`Размер скачанного ${asset.name} не совпадает с API.`);
  return sha256(bytes);
}

// checkAssets сверяет все удалённые файлы с локальными по именам, состоянию, размеру и SHA-256 без
// замены или удаления. complete дополнительно требует полный набор файлов.
async function checkAssets(options, release, expected, complete) {
  // assets содержит все удалённые файлы выпуска со всех страниц API для сверки с локальным набором.
  const assets = await options.github.paginate(options.github.rest.repos.listReleaseAssets, {
    ...options.repo, release_id: release.id, per_page: 100,
  });
  // found хранит имена уже сверенных файлов и позволяет обнаружить дубликат либо неполный набор.
  const found = new Set();
  // asset обозначает очередное удалённое описание файла, проверяемое по имени, состоянию, размеру и
  // SHA-256.
  for (const asset of assets) {
    // local содержит ожидаемые байты, размер и хеш файла с тем же именем; отсутствие значения означает
    // посторонний артефакт.
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

// prepare передаёт GoReleaser создание нового черновика либо дозагружает в проверенный черновик только
// отсутствующие файлы. Перед дозагрузками повторно проверяются состояние выпуска, имеющиеся файлы и
// тег; опубликованный выпуск пропускается.
async function prepare(input) {
  // options содержит проверенные параметры стабильного выпуска и зависимости GitHub API для подготовки
  // файлов.
  const options = parameters(input);
  // release хранит найденное состояние выпуска и заменяется результатом повторного чтения перед
  // изменениями.
  let release = await findRelease(options);
  if (release && published(options, release)) return;
  if (release) checkDraft(options, release);
  await checkTag(options);
  // expected содержит полный набор прочитанных локальных файлов с размерами и хешами для проверки и
  // дозагрузки.
  const expected = await localAssets(options);
  if (!release) {
    outputs(options, true, false);
    return;
  }

  // found содержит имена подтверждённых удалённых файлов и обновляется после повторной сверки перед
  // дозагрузкой.
  let found = await checkAssets(options, release, expected, false);
  // name обозначает ожидаемое имя очередного файла; asset хранит его исходные байты, размер и хеш для
  // дозагрузки.
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

// verify проверяет точный тег и полный совпадающий набор файлов черновика, затем повторно читает его
// состояние. Уже опубликованный выпуск пропускается без изменений.
async function verify(input) {
  // options содержит проверенные параметры стабильного выпуска и зависимости API для завершающей
  // проверки.
  const options = parameters(input);
  // release хранит найденный выпуск, который перечитывается после сверки файлов для проверки его
  // текущего состояния.
  let release = await findRelease(options);
  if (!release) throw new Error(`Релиз ${options.version} не найден.`);
  if (published(options, release)) return;
  checkDraft(options, release);
  await checkTag(options);
  // expected содержит локальные файлы, с которыми должен полностью совпасть удалённый набор выпуска.
  const expected = await localAssets(options);
  await checkAssets(options, release, expected, true);
  release = await readRelease(options, release.id);
  if (published(options, release)) return;
  checkDraft(options, release);
  outputs(options, false, false);
  options.core.info(`Draft Release ${options.version} содержит все проверенные артефакты.`);
}

module.exports = { prepare, verify };
