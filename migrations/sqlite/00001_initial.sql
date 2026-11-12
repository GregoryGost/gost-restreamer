-- +goose Up

-- storage_format хранит единственный маркер совместимости данных. singleton=1 задаёт ключ этой строки,
-- version=2 — поддерживаемый формат управления, проверяемый отдельно от версии Goose.
CREATE TABLE storage_format (singleton INTEGER PRIMARY KEY CHECK(singleton=1), version INTEGER NOT NULL);
INSERT INTO storage_format(singleton,version) VALUES(1,2);

-- fallback_assets хранит стабильные объекты заглушек и выбор текущего либо подготавливаемого поколения.
-- Составные внешние ключи связывают оба указателя с той же заглушкой, а текущему и ожидающему поколениям
-- запрещено совпадать.
CREATE TABLE fallback_assets (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- incarnation сохраняет идентичность открытия хранилища для попытки подготовки заглушки.
    incarnation TEXT NOT NULL,
    -- state содержит совместимую проекцию готовности заглушки: PREPARING, READY либо FAILED.
    state TEXT NOT NULL CHECK(state IN ('PREPARING','READY','FAILED')),
    -- current_generation_id выбирает опубликованный комплект той же заглушки, NULL означает отсутствие
    -- текущего поколения.
    current_generation_id TEXT,
    -- pending_generation_id указывает подготавливаемое поколение той же заглушки, NULL означает
    -- отсутствие ожидающего задания.
    pending_generation_id TEXT,
    FOREIGN KEY(current_generation_id,id) REFERENCES fallback_generations(id,asset_id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(pending_generation_id,id) REFERENCES fallback_generations(id,asset_id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    CHECK(current_generation_id IS NULL OR current_generation_id IS NOT pending_generation_id)
);

-- fallback_generations хранит отдельные попытки подготовки комплектов заглушки и долговечные признаки их
-- выдачи и очистки. Составные уникальные ключи позволяют проверять принадлежность вариантов той же
-- заглушке и исходному содержимому.
CREATE TABLE fallback_generations (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- asset_id указывает заглушку, которой принадлежит запись.
    asset_id TEXT NOT NULL REFERENCES fallback_assets(id) ON DELETE RESTRICT,
    -- incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
    incarnation TEXT NOT NULL,
    -- state содержит долговечный этап отдельного задания подготовки от QUEUED до терминального
    -- результата.
    state TEXT NOT NULL CHECK(state IN ('QUEUED','CLAIMED','ISSUED','PUBLISHED','FAILED','INTERRUPTED')),
    -- issued отмечает выдачу внешней попытки, завершение которой требует подтверждения.
    issued INTEGER NOT NULL CHECK(issued IN (0,1)),
    -- cleanup_pending отмечает неподтверждённую очистку выданных ресурсов.
    cleanup_pending INTEGER NOT NULL CHECK(cleanup_pending IN (0,1)),
    -- source_hash содержит хеш оригинала данного поколения и входит в составной ключ происхождения
    -- вариантов.
    source_hash TEXT NOT NULL,
    UNIQUE(id,asset_id),
    UNIQUE(id,asset_id,source_hash)
);

-- streams хранит текущие настройки потоков, постоянные ключи публикации и необязательный выбор заглушки.
CREATE TABLE streams (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- publication_key содержит постоянный уникальный ключ приёма публикации OBS для этого потока.
    publication_key TEXT NOT NULL UNIQUE,
    -- asset_id задаёт необязательную выбранную заглушку потока, NULL означает отсутствие выбора.
    asset_id TEXT REFERENCES fallback_assets(id) ON DELETE RESTRICT
);

-- profiles хранит полные версионированные профили преобразования. Настройки кодирования и медиаформат
-- находятся в payload.
CREATE TABLE profiles (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL
);

-- groups связывает текущую группу с потоком и профилем преобразования. Составные уникальные ключи
-- позволяют другим таблицам проверять согласованную принадлежность группы.
CREATE TABLE groups (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- stream_id указывает поток, к которому относится запись.
    stream_id TEXT NOT NULL REFERENCES streams(id) ON DELETE RESTRICT,
    -- profile_id указывает текущий профиль преобразования.
    profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE RESTRICT,
    UNIQUE(id,stream_id),
    UNIQUE(id,stream_id,profile_id)
);

-- destinations хранит настройки назначений. Составной внешний ключ требует, чтобы выбранная группа
-- принадлежала тому же потоку.
CREATE TABLE destinations (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- stream_id указывает поток, к которому относится запись.
    stream_id TEXT NOT NULL REFERENCES streams(id) ON DELETE RESTRICT,
    -- group_id указывает группу преобразования.
    group_id TEXT NOT NULL,
    FOREIGN KEY(group_id,stream_id) REFERENCES groups(id,stream_id) ON DELETE RESTRICT,
    UNIQUE(id,stream_id,group_id)
);

-- fallback_variants хранит подготовленные варианты поколения. Связи проверяют происхождение управляемого
-- пути, заглушку и хеш оригинала, а полные канонические байты профиля различают варианты одного
-- поколения.
CREATE TABLE fallback_variants (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- asset_id указывает заглушку, которой принадлежит запись.
    asset_id TEXT NOT NULL REFERENCES fallback_assets(id) ON DELETE RESTRICT,
    -- generation_id указывает поколение подготовки заглушки, к которому относится запись.
    generation_id TEXT NOT NULL,
    -- incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
    incarnation TEXT NOT NULL,
    -- source_hash содержит хеш оригинала, связывающий вариант с содержимым поколения.
    source_hash TEXT NOT NULL,
    -- profile_key содержит вычисленный ключ профиля, сверяемый с каноническими байтами и доменными
    -- метаданными.
    profile_key TEXT NOT NULL,
    -- canonical_profile содержит полные канонические байты профиля для единственности варианта внутри
    -- поколения.
    canonical_profile BLOB NOT NULL,
    -- generator_version идентифицирует контракт генератора подготовленного варианта.
    generator_version TEXT NOT NULL,
    -- managed_path содержит зарегистрированный управляемый путь файла либо каталога.
    managed_path TEXT NOT NULL UNIQUE,
    -- size_bytes содержит размер файла в байтах как двадцатизначный uint64 без потери старшего бита.
    size_bytes TEXT COLLATE BINARY NOT NULL CHECK(length(size_bytes)=20 AND size_bytes NOT GLOB '*[^0-9]*' AND size_bytes<='18446744073709551615'),
    -- state сохраняет этап подготовки варианта: PREPARING, READY либо FAILED.
    state TEXT NOT NULL CHECK(state IN ('PREPARING','READY','FAILED')),
    UNIQUE(id,asset_id,generation_id),
    UNIQUE(generation_id,canonical_profile),
    FOREIGN KEY(managed_path,generation_id,asset_id) REFERENCES owned_paths(managed_path,generation_id,asset_id) ON DELETE RESTRICT,
    FOREIGN KEY(generation_id,asset_id,source_hash) REFERENCES fallback_generations(id,asset_id,source_hash) ON DELETE RESTRICT
);

-- sessions хранит историю запусков и долговечное управление их жизненным циклом. Исходные настройки
-- запуска находятся в payload, оперативные измерения здесь не сохраняются.
CREATE TABLE sessions (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- stream_id сохраняет ID исходного потока в истории запуска без внешнего ключа на текущую
    -- конфигурацию Stream.
    stream_id TEXT NOT NULL,
    -- incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
    incarnation TEXT NOT NULL,
    -- state сохраняет долговечный этап запуска от STARTING до STOPPED, FAILED либо INTERRUPTED.
    state TEXT NOT NULL CHECK(state IN ('STARTING','RUNNING','STOPPING','STOPPED','FAILED','INTERRUPTED')),
    -- lifecycle хранит поколение жизненного цикла Session как двадцатизначный uint64.
    lifecycle TEXT COLLATE BINARY NOT NULL CHECK(length(lifecycle)=20 AND lifecycle NOT GLOB '*[^0-9]*' AND lifecycle<='18446744073709551615'),
    -- issued отмечает выдачу внешней попытки, завершение которой требует подтверждения.
    issued INTEGER NOT NULL CHECK(issued IN (0,1)),
    -- cleanup_pending отмечает неподтверждённую очистку выданных ресурсов.
    cleanup_pending INTEGER NOT NULL CHECK(cleanup_pending IN (0,1))
);

-- outputs хранит самостоятельные снимки и поколения исполнения выходов Session. Текущее состояние
-- сетевого соединения остаётся оперативным наблюдением.
CREATE TABLE outputs (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- session_id указывает сохраняемый запуск, к которому относится запись.
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    -- generation различает поколения исполнения одного выхода как двадцатизначный uint64.
    generation TEXT COLLATE BINARY NOT NULL CHECK(length(generation)=20 AND generation NOT GLOB '*[^0-9]*' AND generation<='18446744073709551615'),
    -- attempt хранит номер конкретной выданной попытки исполнения как двадцатизначный uint64.
    attempt TEXT COLLATE BINARY NOT NULL CHECK(length(attempt)=20 AND attempt NOT GLOB '*[^0-9]*' AND attempt<='18446744073709551615'),
    -- issued отмечает выдачу внешней попытки, завершение которой требует подтверждения.
    issued INTEGER NOT NULL CHECK(issued IN (0,1)),
    -- cleanup_pending отмечает неподтверждённую очистку выданных ресурсов.
    cleanup_pending INTEGER NOT NULL CHECK(cleanup_pending IN (0,1)),
    UNIQUE(id,session_id)
);

-- command_receipts хранит идентичность запросов управления, их долговечное состояние и точные поколения
-- внешних попыток. Уникальный request_id связывает повторы с одной квитанцией.
CREATE TABLE command_receipts (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- request_id содержит исходный ключ идемпотентности команды и уникален среди квитанций.
    request_id TEXT NOT NULL UNIQUE,
    -- session_id связывает команду с Session, если эта ссылка задана.
    session_id TEXT REFERENCES sessions(id) ON DELETE RESTRICT,
    -- output_id связывает команду с выходом указанной Session, если команда адресована выходу.
    output_id TEXT,
    -- incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
    incarnation TEXT NOT NULL,
    -- state сохраняет статус команды от PENDING через CLAIMED и ISSUED до терминального результата.
    state TEXT NOT NULL CHECK(state IN ('PENDING','CLAIMED','ISSUED','SUCCEEDED','FAILED','INTERRUPTED')),
    -- lifecycle хранит поколение жизненного цикла Session как двадцатизначный uint64.
    lifecycle TEXT COLLATE BINARY NOT NULL CHECK(length(lifecycle)=20 AND lifecycle NOT GLOB '*[^0-9]*' AND lifecycle<='18446744073709551615'),
    -- generation сохраняет поколение целевого выхода как двадцатизначный uint64.
    generation TEXT COLLATE BINARY NOT NULL CHECK(length(generation)=20 AND generation NOT GLOB '*[^0-9]*' AND generation<='18446744073709551615'),
    -- attempt хранит номер конкретной выданной попытки исполнения как двадцатизначный uint64.
    attempt TEXT COLLATE BINARY NOT NULL CHECK(length(attempt)=20 AND attempt NOT GLOB '*[^0-9]*' AND attempt<='18446744073709551615'),
    FOREIGN KEY(output_id,session_id) REFERENCES outputs(id,session_id) ON DELETE RESTRICT
);

-- cleanup_intents хранит намерения очистки ресурсов исполнения либо файлов. При заполненных составляющих
-- составные ссылки проверяют, что выход относится к указанной Session, а файловая запись — к указанным
-- пути и поколению.
CREATE TABLE cleanup_intents (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- kind различает очистку исполнения runtime и управляемого файла file.
    kind TEXT NOT NULL CHECK(kind IN ('runtime','file')),
    -- stream_id содержит ID потока очищаемого ресурса, если он задан.
    stream_id TEXT,
    -- session_id указывает Session очищаемого исполнения и обязателен для kind=runtime.
    session_id TEXT REFERENCES sessions(id) ON DELETE RESTRICT,
    -- output_id уточняет очищаемый выход указанной Session, если очистка относится к выходу.
    output_id TEXT,
    -- incarnation идентифицирует открытие хранилища, которому принадлежит сохраняемая попытка.
    incarnation TEXT NOT NULL,
    -- lifecycle хранит поколение жизненного цикла Session как двадцатизначный uint64.
    lifecycle TEXT COLLATE BINARY NOT NULL CHECK(length(lifecycle)=20 AND lifecycle NOT GLOB '*[^0-9]*' AND lifecycle<='18446744073709551615'),
    -- generation сохраняет поколение очищаемого выхода как двадцатизначный uint64.
    generation TEXT COLLATE BINARY NOT NULL CHECK(length(generation)=20 AND generation NOT GLOB '*[^0-9]*' AND generation<='18446744073709551615'),
    -- attempt хранит номер конкретной выданной попытки исполнения как двадцатизначный uint64.
    attempt TEXT COLLATE BINARY NOT NULL CHECK(length(attempt)=20 AND attempt NOT GLOB '*[^0-9]*' AND attempt<='18446744073709551615'),
    -- managed_path указывает зарегистрированный очищаемый путь и обязателен для kind=file.
    managed_path TEXT REFERENCES owned_paths(managed_path) ON DELETE RESTRICT,
    -- artifact_generation_id указывает поколение заглушки, которому принадлежит очищаемый файл.
    artifact_generation_id TEXT,
    -- file_id указывает запись происхождения файла, согласованную с managed_path и
    -- artifact_generation_id.
    file_id TEXT REFERENCES owned_paths(id) ON DELETE RESTRICT,
    -- acknowledged сохраняет подтверждение выполнения данного намерения очистки.
    acknowledged INTEGER NOT NULL CHECK(acknowledged IN (0,1)),
    FOREIGN KEY(file_id,managed_path,artifact_generation_id) REFERENCES owned_paths(id,managed_path,generation_id) ON DELETE RESTRICT,
    FOREIGN KEY(output_id,session_id) REFERENCES outputs(id,session_id) ON DELETE RESTRICT,
    CHECK((kind='file' AND managed_path IS NOT NULL) OR (kind='runtime' AND session_id IS NOT NULL))
);

-- owned_paths хранит происхождение управляемых путей независимо от существования соответствующего файла.
-- Составные уникальные ключи дают вариантам и очистке возможность ссылаться на точные путь, поколение и
-- заглушку.
CREATE TABLE owned_paths (
    -- id идентифицирует сохраняемую доменную запись.
    id TEXT PRIMARY KEY NOT NULL CHECK(length(id)>0),
    -- revision хранит uint64 двадцатью десятичными цифрами, чтобы сравнение BINARY сохраняло числовой
    -- порядок.
    revision TEXT COLLATE BINARY NOT NULL CHECK(length(revision)=20 AND revision NOT GLOB '*[^0-9]*' AND revision<='18446744073709551615'),
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- updated_at содержит время последнего изменения в микросекундах Unix.
    updated_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- managed_path содержит зарегистрированный управляемый путь файла либо каталога.
    managed_path TEXT NOT NULL UNIQUE,
    -- asset_id указывает заглушку, которой принадлежит запись.
    asset_id TEXT NOT NULL,
    -- generation_id указывает поколение подготовки заглушки, к которому относится запись.
    generation_id TEXT NOT NULL,
    -- kind различает каталог поколения directory, оригинал original, вариант variant и промежуточный
    -- путь staging.
    kind TEXT NOT NULL CHECK(kind IN ('directory','original','variant','staging')),
    UNIQUE(id,managed_path,generation_id),
    UNIQUE(managed_path,generation_id,asset_id)
);

-- session_holds удерживает конфигурацию потока, заглушку и её поколение для конкретной Session. Внешние
-- ключи с RESTRICT запрещают удаление удерживаемых объектов до освобождения ссылок.
CREATE TABLE session_holds (
    -- session_id идентифицирует единственное удержание общего комплекта заглушки этой Session.
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    -- stream_id удерживает конфигурацию потока запуска.
    stream_id TEXT NOT NULL REFERENCES streams(id) ON DELETE RESTRICT,
    -- asset_id удерживает объект выбранной заглушки.
    asset_id TEXT NOT NULL REFERENCES fallback_assets(id) ON DELETE RESTRICT,
    -- generation_id удерживает выбранное поколение той же заглушки.
    generation_id TEXT NOT NULL,
    FOREIGN KEY(generation_id,asset_id) REFERENCES fallback_generations(id,asset_id) ON DELETE RESTRICT
);

-- output_holds удерживает настройки и точный вариант заглушки для отдельного выхода. Внешние ключи с
-- RESTRICT сохраняют связанные объекты до освобождения удержания.
CREATE TABLE output_holds (
    -- output_id идентифицирует единственное удержание конфигурации и варианта данного выхода.
    output_id TEXT PRIMARY KEY NOT NULL,
    -- session_id связывает удержание с той же Session, которой принадлежит выход.
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    -- stream_id удерживает текущую конфигурацию потока для этого выхода.
    stream_id TEXT NOT NULL REFERENCES streams(id) ON DELETE RESTRICT,
    -- group_id удерживает конфигурацию группы выхода.
    group_id TEXT NOT NULL,
    -- profile_id удерживает профиль преобразования выхода.
    profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE RESTRICT,
    -- destination_id удерживает конфигурацию назначения выхода.
    destination_id TEXT NOT NULL,
    -- asset_id удерживает объект заглушки выбранного варианта.
    asset_id TEXT NOT NULL REFERENCES fallback_assets(id) ON DELETE RESTRICT,
    -- variant_id удерживает конкретный вариант той же заглушки и поколения.
    variant_id TEXT NOT NULL,
    -- generation_id удерживает поколение, которому принадлежит выбранный вариант.
    generation_id TEXT NOT NULL,
    FOREIGN KEY(output_id,session_id) REFERENCES outputs(id,session_id) ON DELETE RESTRICT,
    FOREIGN KEY(group_id) REFERENCES groups(id) ON DELETE RESTRICT,
    FOREIGN KEY(destination_id) REFERENCES destinations(id) ON DELETE RESTRICT,
    FOREIGN KEY(variant_id,asset_id,generation_id) REFERENCES fallback_variants(id,asset_id,generation_id) ON DELETE RESTRICT
);

-- events хранит неизменяемые долговечные события протокола управления, отдельно от оперативных измерений
-- и потока RAM-событий.
CREATE TABLE events (
    -- id идентифицирует неизменяемое событие управления.
    id TEXT PRIMARY KEY NOT NULL,
    -- created_at содержит время создания в микросекундах Unix.
    created_at INTEGER NOT NULL,
    -- payload содержит полную версионированную доменную запись, включая поля, не выделенные в
    -- SQL-колонки.
    payload BLOB NOT NULL,
    -- session_id указывает сохраняемый запуск, к которому относится запись.
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    -- output_id уточняет выход той же Session, если событие относится к отдельному выходу.
    output_id TEXT,
    FOREIGN KEY(output_id,session_id) REFERENCES outputs(id,session_id) ON DELETE RESTRICT
);

-- sessions_one_active_stream допускает не более одной STARTING, RUNNING либо STOPPING-сессии каждого
-- потока.
CREATE UNIQUE INDEX sessions_one_active_stream ON sessions(stream_id) WHERE state IN ('STARTING','RUNNING','STOPPING');

-- sessions_cleanup_stream индексирует проверку неподтверждённой очистки запусков конкретного потока.
CREATE INDEX sessions_cleanup_stream ON sessions(stream_id,cleanup_pending);

-- fallback_assets_created задаёт индекс порядка страниц заглушек по created_at и id.
CREATE INDEX fallback_assets_created ON fallback_assets(created_at,id);

-- fallback_assets_state_created поддерживает тот же порядок страниц заглушек при фильтре state.
CREATE INDEX fallback_assets_state_created ON fallback_assets(state,created_at,id);

-- streams_created задаёт индекс порядка страниц потоков по created_at и id.
CREATE INDEX streams_created ON streams(created_at,id);

-- profiles_created задаёт индекс порядка страниц профилей по created_at и id.
CREATE INDEX profiles_created ON profiles(created_at,id);

-- groups_created задаёт индекс порядка страниц групп по created_at и id.
CREATE INDEX groups_created ON groups(created_at,id);

-- destinations_created задаёт индекс порядка страниц назначений по created_at и id.
CREATE INDEX destinations_created ON destinations(created_at,id);

-- fallback_variants_created задаёт индекс порядка страниц вариантов по created_at и id.
CREATE INDEX fallback_variants_created ON fallback_variants(created_at,id);

-- fallback_variants_state_created индексирует страницы вариантов выбранного state.
CREATE INDEX fallback_variants_state_created ON fallback_variants(state,created_at,id);

-- sessions_created задаёт индекс порядка истории запусков по created_at и id.
CREATE INDEX sessions_created ON sessions(created_at,id);

-- sessions_state_created индексирует историю запусков выбранного state.
CREATE INDEX sessions_state_created ON sessions(state,created_at,id);

-- outputs_created задаёт индекс порядка страниц выходов по created_at и id.
CREATE INDEX outputs_created ON outputs(created_at,id);

-- command_receipts_created задаёт индекс порядка страниц квитанций по created_at и id.
CREATE INDEX command_receipts_created ON command_receipts(created_at,id);

-- command_receipts_state_created индексирует страницы квитанций выбранного state.
CREATE INDEX command_receipts_state_created ON command_receipts(state,created_at,id);

-- cleanup_intents_created задаёт индекс порядка страниц намерений очистки по created_at и id.
CREATE INDEX cleanup_intents_created ON cleanup_intents(created_at,id);

-- cleanup_intents_acknowledged_created индексирует страницы очистки с выбранным признаком acknowledged.
CREATE INDEX cleanup_intents_acknowledged_created ON cleanup_intents(acknowledged,created_at,id);

-- events_created задаёт индекс порядка страниц долговечных событий по created_at и id.
CREATE INDEX events_created ON events(created_at,id);

-- groups_stream_id_created индексирует страницы групп конкретного потока.
CREATE INDEX groups_stream_id_created ON groups(stream_id,created_at,id);

-- destinations_stream_id_created индексирует страницы назначений конкретного потока.
CREATE INDEX destinations_stream_id_created ON destinations(stream_id,created_at,id);

-- fallback_variants_asset_id_created индексирует страницы вариантов конкретной заглушки.
CREATE INDEX fallback_variants_asset_id_created ON fallback_variants(asset_id,created_at,id);

-- fallback_variants_asset_id_state_created индексирует страницы вариантов заглушки с выбранным state.
CREATE INDEX fallback_variants_asset_id_state_created ON fallback_variants(asset_id,state,created_at,id);

-- sessions_stream_id_created индексирует историю запусков конкретного потока.
CREATE INDEX sessions_stream_id_created ON sessions(stream_id,created_at,id);

-- sessions_stream_id_state_created индексирует историю запусков потока с выбранным state.
CREATE INDEX sessions_stream_id_state_created ON sessions(stream_id,state,created_at,id);

-- outputs_session_id_created индексирует страницы выходов конкретной Session.
CREATE INDEX outputs_session_id_created ON outputs(session_id,created_at,id);

-- events_session_id_created индексирует страницы событий конкретной Session.
CREATE INDEX events_session_id_created ON events(session_id,created_at,id);

-- command_receipts_session_id_created индексирует страницы квитанций конкретной Session.
CREATE INDEX command_receipts_session_id_created ON command_receipts(session_id,created_at,id);

-- command_receipts_session_id_state_created индексирует страницы квитанций Session с выбранным state.
CREATE INDEX command_receipts_session_id_state_created ON command_receipts(session_id,state,created_at,id);

-- cleanup_intents_stream_id_created индексирует страницы очистки конкретного потока.
CREATE INDEX cleanup_intents_stream_id_created ON cleanup_intents(stream_id,created_at,id);

-- cleanup_intents_stream_id_acknowledged_created индексирует очистку потока с выбранным подтверждением
-- acknowledged.
CREATE INDEX cleanup_intents_stream_id_acknowledged_created ON cleanup_intents(stream_id,acknowledged,created_at,id);

-- cleanup_intents_session_id_acknowledged_created индексирует очистку Session с выбранным подтверждением
-- acknowledged.
CREATE INDEX cleanup_intents_session_id_acknowledged_created ON cleanup_intents(session_id,acknowledged,created_at,id);

-- output_holds_session_id индексирует удержания, освобождаемые при завершении всей Session.
CREATE INDEX output_holds_session_id ON output_holds(session_id);

-- output_holds_stream_id индексирует удержания конфигурации потока выходами.
CREATE INDEX output_holds_stream_id ON output_holds(stream_id);

-- output_holds_group_id индексирует удержания группы выходами.
CREATE INDEX output_holds_group_id ON output_holds(group_id);

-- output_holds_profile_id индексирует удержания профиля выходами.
CREATE INDEX output_holds_profile_id ON output_holds(profile_id);

-- output_holds_destination_id индексирует удержания назначения выходами.
CREATE INDEX output_holds_destination_id ON output_holds(destination_id);

-- output_holds_asset_id индексирует удержания заглушки выходами.
CREATE INDEX output_holds_asset_id ON output_holds(asset_id);

-- output_holds_variant_id индексирует удержания конкретного варианта выходами.
CREATE INDEX output_holds_variant_id ON output_holds(variant_id);

-- session_holds_stream_id индексирует удержания конфигурации потока сессиями.
CREATE INDEX session_holds_stream_id ON session_holds(stream_id);

-- session_holds_asset_id индексирует удержания заглушки сессиями.
CREATE INDEX session_holds_asset_id ON session_holds(asset_id);

-- streams_asset_id индексирует текущие ссылки потоков на выбранную заглушку.
CREATE INDEX streams_asset_id ON streams(asset_id);

-- groups_profile_id индексирует текущие ссылки групп на профиль.
CREATE INDEX groups_profile_id ON groups(profile_id);

-- fallback_generations_created задаёт индекс порядка страниц поколений по created_at и id.
CREATE INDEX fallback_generations_created ON fallback_generations(created_at,id);
-- fallback_generations_asset_created индексирует страницы поколений конкретной заглушки.
CREATE INDEX fallback_generations_asset_created ON fallback_generations(asset_id,created_at,id);
-- fallback_generations_pending индексирует этап, выдачу и незавершённую очистку подготовки вместе с
-- порядком создания.
CREATE INDEX fallback_generations_pending ON fallback_generations(state,issued,cleanup_pending,created_at,id);
-- fallback_generations_one_executor допускает не более одного поколения с CLAIMED/ISSUED либо
-- установленным issued или cleanup_pending во всём хранилище.
CREATE UNIQUE INDEX fallback_generations_one_executor ON fallback_generations((1)) WHERE state IN ('CLAIMED','ISSUED') OR issued=1 OR cleanup_pending=1;
-- owned_paths_generation_created индексирует страницы управляемых путей конкретного поколения.
CREATE INDEX owned_paths_generation_created ON owned_paths(generation_id,created_at,id);
-- fallback_variants_generation_created индексирует страницы вариантов конкретного поколения.
CREATE INDEX fallback_variants_generation_created ON fallback_variants(generation_id,created_at,id);
-- output_holds_generation индексирует проверку удержаний поколения отдельными выходами.
CREATE INDEX output_holds_generation ON output_holds(generation_id);
-- session_holds_generation индексирует проверку удержаний поколения сессиями.
CREATE INDEX session_holds_generation ON session_holds(generation_id);

-- cleanup_intents_one_file запрещает повтор файловых намерений очистки для одного file_id, когда он
-- задан.
CREATE UNIQUE INDEX cleanup_intents_one_file ON cleanup_intents(file_id) WHERE kind='file';
-- cleanup_intents_artifact_generation индексирует страницы очистки файлов конкретного поколения
-- заглушки.
CREATE INDEX cleanup_intents_artifact_generation ON cleanup_intents(artifact_generation_id,created_at,id);
