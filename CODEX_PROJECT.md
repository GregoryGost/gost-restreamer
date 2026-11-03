# Codex Project Profile — gost-restreamer

## Project Identity

- Project key / workspace: `gost-restreamer`.
- Project type: Go backend for self-hosted restreaming; the OBS plugin is a separate project.
- Primary programming language: Go. SQL is used for Goose migrations.
- Architecture source: `gost-restreamer-architecture.md`, revision 6.
- Current repository state: architecture and agent workflow only; no Go module, application source, migrations, tests or CI have been created.
- Selected technology families are approved and active now. Exact dependency/toolchain versions and unimplemented runtime settings remain unverified until backed by manifests and runtime evidence.

## Activation Semantics

Все перечисленные ниже материалы активны по умолчанию. Повторный выбор слоёв не требуется. Применяется только материал, соответствующий текущей задаче; отсутствие кода не отключает выбранный профиль.

`.codex/rules/request_routing.md` обязателен до любых предметных действий. `.codex/rules/material_dependencies.md` проверяет явно объявленные зависимости. `.codex/rules/subagent_coordination.md` обязателен перед делегированием. Отсутствующий обязательный файл или противоречие профиля нужно сообщить и исправить в разрешённой области, а не обходить.

Точные записи Active Rules и Active Skills являются сигналами активации. Незаполненная runtime-настройка не означает `none` для материала: перед работой, зависящей от неё, нужно получить проверяемое значение. Этот профиль не разрешает побочные действия за пределами пользовательского запроса.

## Natural Language Profile

- User-facing answers language: Russian.
- Git commit text language: Russian; конкретные выполненные изменения, без инфинитива.
- Documentation language policy: русский для проектной документации.
- Test/comment natural language policy: русский для комментариев, doc comments, пояснений в тестах и документирующих полей API; грамматически цельные предложения.
- Keep identifiers, library names, protocols, commands, paths, package names and exact contract values in English or original language: yes.
- Imported agent-projects-tools rules, role instructions, skill references, quoted official examples and license text retain their original language. Не переводить их при обычном изменении проекта.
- `comment-language-audit` применяется к создаваемому или изменяемому проектному тексту рядом с кодом. Не устанавливать языковые линтеры и не переводить весь репозиторий автоматически.

## Active Stack Profiles

- `go-core`
- `go-testing`
- `go-huma-v2`
- `go-local-cache`
- `go-gorm`
- `go-gorm-sqlite-goose`

## Active Rules

- Required routing rule: `.codex/rules/request_routing.md`
- Conditional bootstrap support: `.codex/rules/material_dependencies.md`, `.codex/rules/subagent_coordination.md`
- Core rules: `source_code_hygiene.md`
- Git rules: `git.md`
- Documentation retrieval: `context7_documentation.md`
- Go rules: `go_core.md`, `go_testing.md`, `go_huma_v2.md`, `go_local_cache.md`, `go_gorm.md`, `go_gorm_sqlite_goose.md`

Bare rule filenames resolve under `.codex/rules/`.

## Active Skills

- Code prose / language-policy skills: `comment-language-audit`
- Go skills: `go-core`, `go-testing`, `go-huma-v2`, `go-local-cache`, `go-gorm`, `go-gorm-sqlite-goose`

Skill names resolve to `.agents/skills/<name>/SKILL.md`. Required dependency closure:

- `go_core.md` → `go-core`.
- `go_testing.md` → `go-core`, `go-testing`; `go-testing` → `go-core`.
- `go_huma_v2.md` → `go-core`, `go-huma-v2`; `go-huma-v2` → `go-core`.
- `go_local_cache.md` → `go-core`, `go-local-cache`; `go-local-cache` → `go-core`.
- `go_gorm.md` → `go-core`, `go-gorm`; `go-gorm` → `go-core`.
- `go_gorm_sqlite_goose.md` → `go-core`, `go-gorm`, `go-gorm-sqlite-goose`; `go-gorm-sqlite-goose` → `go-gorm`.

## Agent Roles And Model Selection

- Subagents enabled: yes.
- Delegation scope: authorized executable application/test-code changes only; instructions, documentation, comments and docstrings alone are handled by the primary.
- Primary agent model/effort: `gpt-6.1-sol / xhigh`.
- Configuration: existing local `.codex/config.toml`; portable baseline in `.codex/config.example.toml`.
- Concurrent subagent threads: 4, excluding the primary, subject to the active client limit.
- Model availability: verify against the active client/account; report unavailable combinations without silently substituting models.
- Fresh thread policy: every assignment uses a new no-history `spawn_agent` with `fork_turns: "none"` on verified MultiAgentV2; never `followup_task`.
- Subagent handoff persistence: `in-session-only`.
- Selected role files: all ten exact files in the table below. Selection makes them available; only relevant roles are assigned for a task.

| Role | Custom agent file | Model | Effort |
| --- | --- | --- | --- |
| Architecture: stability | `.codex/agents/architect_stability.toml` | `gpt-6.1-sol` | `xhigh` |
| Architecture: evolution | `.codex/agents/architect_evolution.toml` | `gpt-6.1-sol` | `xhigh` |
| Database architecture | `.codex/agents/database_architect.toml` | `gpt-6.1-sol` | `xhigh` |
| Codebase research | `.codex/agents/code_explorer.toml` | `gpt-6-luna` | `high` |
| Documentation/API research | `.codex/agents/docs_researcher.toml` | `gpt-6-luna` | `high` |
| Complex implementation | `.codex/agents/complex_implementer.toml` | `gpt-6.1-sol` | `high` |
| Small fixes | `.codex/agents/simple_implementer.toml` | `gpt-6-luna` | `high` |
| Independent testing | `.codex/agents/test_engineer.toml` | `gpt-6.1-sol` | `high` |
| Independent code review | `.codex/agents/code_reviewer.toml` | `gpt-6.1-sol` | `high` |
| Difficult bugs / design disputes | `.codex/agents/deep_debugger.toml` | `gpt-6-astra` | `xhigh` |

### Architecture Gate And Database Assessment

Основной агент перед работой с исполняемым кодом явно фиксирует для области задачи `NOT_REQUIRED`, если архитекторы не назначены. При назначении архитектуры нужны оба системных архитектора; сразу действует `PENDING`. При `PENDING`, `REOPENED` или `BLOCKED` зависимые планирование реализации, изменения кода/тестов и приёмка остановлены у всех участников. Разрешённый сбор фактов продолжается.

Оба архитектора получают одинаковые исходные требования и факты, независимо готовят предложения, затем через основного агента обмениваются предметной критикой. До трёх продуктивных раундов; нерешённый фактический спор передаётся `deep_debugger`, пользовательский выбор возвращается пользователю. Оба архитектора должны подтвердить одну и ту же полную каноническую редакцию. Только после проверки двух подтверждений основной агент устанавливает `APPROVED` и выдаёт Architecture Handoff с областью, `decision_version`, исходной ревизией, утверждённым решением, контрактами, инвариантами, рисками и обязательными проверками.

Выбран `database_architect`: перед изменениями, затрагивающими данные, основной агент фиксирует `db_assessment_status`. Существенные изменения схемы/сохраняемых контрактов, миграции существующих данных, транзакций, приватности, восстановления или значимых характеристик запросов требуют оценки. `NOT_REQUIRED` допустим с проверенным обоснованием отсутствия существенного риска. Обязательные `PENDING`, `CHANGES_REQUIRED`, `BLOCKED` и `STALE` блокируют затронутую работу независимо от Architecture Gate.

Для архитектуры данных предметные факты DBA передаются обоим архитекторам до их независимых предложений. Финальная оценка DBA выполняется в новой чистой сессии для точного `candidate_revision`; до общего `APPROVED` требуется актуальный `CLEAR`. DBA не является третьим голосом и не подменяет тесты или ревью.

При закрытии gate основной агент добивается подтверждённой остановки команд и проверяет частичные изменения. Нарушение корректного решения кодом исправляет исполнитель; только доказанный дефект самого решения требует `REOPENED`. После сжатия контекста восстанавливаются текущие разрешения и явное решение `NOT_REQUIRED` либо актуальный утверждённый handoff с двумя подтверждениями.

### Ownership And Validation

- Основной агент назначает точные границы файлов. Одновременные исполнители не изменяют одни и те же код, тесты, фикстуры или конфигурацию.
- `test_engineer` независимо выводит ожидания из исходных требований и проверенных контрактов; пишет только назначенные тесты. Исполнители не ослабляют его проверки.
- Для сложного, существенного или чувствительного к безопасности кода обязательны независимые `test_engineer` и `code_reviewer`. Для доказанно локальной безопасной правки основной агент заранее обосновывает пропуск дополнительных ролей; обязательные проектные проверки сохраняются.
- Приёмка относится к стабильному снимку кода, тестов, фикстур и значимой конфигурации, включая новые файлы. Во время финальных проверок входные файлы не меняются; существенная последующая правка требует повторения затронутых проверок.
- Для исправлений, повторного тестирования, ревью и раундов критики создаются новые потоки. Завершённые отчёты проверяются основным агентом и передаются только в актуальной относящейся к задаче части.
- Статусы архитектуры, DBA, тестирования и ревью учитываются отдельно. Недоступная обязательная роль или среда сообщается как ограничение, без выдуманной приёмки.
- Правила read-only у ролей являются ограничениями поведения; разрешения и среда наследуются от основного агента. Проверять владение файлами при параллельных сессиях; не создавать worktree или файлы handoff автоматически.

## Technical Documentation Profile

- Active rule: `context7_documentation.md`.
- Версию и фактическую конфигурацию определяют файлы проекта и проверенная среда. Context7 используется для чтения документации по точной библиотеке и применимой версии после resolve library ID.
- Для последствий, связанных с данными, конкурентностью, безопасностью и совместимостью, проверять первичный официальный источник. Если Context7 недоступен или не покрывает нужную версию, использовать официальные источники из skill references и сообщать оставшиеся пробелы.
- Числа в переносимых материалах, описывающие состояние upstream на момент их подготовки, не устанавливают версию проекта. При расхождении источников сверять точный module tag, его `go.mod`, release notes и документацию.
- Context7 credentials принадлежат MCP-конфигурации. Не передавать ключи, приватный код и пользовательские данные в запросах.
- Используемый локальный Context7 сохраняется. Пример для нового окружения: `bearer_token_env_var = "CONTEXT7_API_KEY"`; ключ задаётся в окружении процесса Codex и не хранится в Git.
- OpenAI/Codex configuration references: [Subagents](https://learn.chatgpt.com/docs/agent-configuration/subagents), [Configuration Reference](https://learn.chatgpt.com/docs/config-file/config-reference).

## Go Profile

- Go enabled: yes; active pair: `go_core.md + go-core`.
- Module/workspace source: будущие `go.mod` и `go.sum`; сейчас отсутствуют. `go.work` не создан.
- Effective language/toolchain version: пока не установлена. Перед первым кодом проверить выбранные module requirements и доступный compiler/CI, затем закрепить совместимую версию. Не подменять её версией из примера или автоматически обновлять toolchain.
- Development console: macOS. Deployment target: Debian 13 по архитектуре; целевую архитектуру CPU и матрицу сборки подтвердить до сборки.
- CGO policy: enabled для исходного `gorm.io/driver/sqlite`; pure-Go замена требует отдельного решения и тестов.
- Dependency integrity: фиксировать выбранные версии в `go.mod` и `go.sum`.
- Formatter/static checks: `gofmt`, `go vet`; дополнительные линтеры не выбраны.
- Ownership: явные владельцы goroutine, очередей, соединений, файлов и остановки. Очереди и кэши ограничены; профилирование только по предметной необходимости, доступ к диагностике защищён.
- Design/review: активные Go references, версия спецификации и release notes, соответствующая модулю.

## Go Testing Profile

- Go testing enabled: yes; active pair: `go_testing.md + go-testing`.
- Runner: стандартный `go test`; unit-тесты рядом с кодом в `*_test.go`.
- HTTP: реальный router/middleware через `net/http/httptest`; проверять ответы и генерируемый OpenAPI.
- Storage: временная файловая SQLite через `t.TempDir`, настоящий runner Goose, реальные ограничения, транзакции, PRAGMA, WAL и блокировки.
- Media integration / E2E: планируемый `tests/integration/`; FFmpeg/GPU и реальные площадки только в явно разрешённой тестовой среде. Успешная сборка не подтверждает live fallback.
- Race: `go test -race ./...` при поддерживаемой платформе/toolchain/CGO; не считать проверкой утечек.
- Parallel/shuffle: только при доказанной изоляции БД, окружения, портов и глобального состояния. `t.Setenv` используется в последовательных тестах.
- Coverage: только по задаче, файлы в разрешённом временном каталоге; порог не установлен.
- Fuzz: только именованная цель и ограниченный бюджет, определённые конкретной задачей.
- Lifecycle: проверять отмену, завершение, освобождение ресурсов и повторный запуск для затронутых workers, streams, pools и caches; не требовать точного глобального числа goroutine или RSS.
- Новые тестовые библиотеки не выбраны.

## Go Huma v2 Profile

- Huma v2 enabled: yes; active pair: `go_huma_v2.md + go-huma-v2`.
- Selected module: `github.com/danielgtaylor/huma/v2`; версия будет закреплена в `go.mod`/`go.sum`. Совместимость Go проверяется по этому релизу.
- Router/adapter: Chi / `humachi` согласно архитектуре; зависимости ещё не установлены.
- Planned registration: `internal/api/`, composition root `cmd/gost-restreamer/main.go`.
- API prefix: `/api/v1`; operation IDs стабильные и уникальные. Маршруты спецификации пока являются проектным контрактом.
- DTO: типизированные отдельные API-структуры, явные правила обязательности/nullable/validation. ORM-модели и доменные структуры не используются как одна общая DTO.
- Errors/auth: доменные ошибки переводятся на HTTP-границе; поведение Problem Details сверяется с выбранной версией. Механизм авторизации ещё не реализован; OpenAPI metadata не заменяет runtime-проверки.
- OpenAPI/docs/schema exposure: точные пути и доступ определить до реализации публикации API, проверить контрактными тестами.
- Tests: реальная регистрация маршрутов через `httptest`; `humatest` допустим из выбранного Huma, если требуется задаче.

## Go Local Cache — Otter v2 Profile

- Go local cache enabled: yes; active pair: `go_local_cache.md + go-local-cache`.
- Selected library/module: `github.com/maypok86/otter/v2`; конкретная версия и её требование Go проверяются и фиксируются при реализации.
- Scope: кэш одного процесса. NATS KV и многоуровневый кэш не выбраны.
- Cached domain/read paths: пока не реализованы; перед первым кэшем определить конкретный восстанавливаемый результат чтения и допустимое устаревание.
- Owner/lifecycle: composition root создаёт и останавливает кэш; освобождение фоновых задач через проверенный API `StopAllGoroutines`.
- Capacity: конечный `MaximumSize` либо `MaximumWeight + Weigher`; численный бюджет выбирается по данным и нагрузке до создания экземпляра. TTL не подменяет бюджет памяти.
- Expiration: конкретный TTL и `ExpiryWriting`/`ExpiryAccessing` определяются для выбранного read path до реализации.
- Loader/refresh: выбрать по контракту чтения; автоматический фоновый refresh по умолчанию не применяется.
- Keys/values: версия схемы ключа, все влияющие входы и область доступа; неизменяемые значения или DTO, без секретов, ORM-сессий, HTTP-запросов и изменяемых runtime-объектов.
- Invalidation: после успешного authoritative commit; явно учесть гонку загрузки и допустимое устаревание. При потере кэша данные восстанавливаются из источника истины.
- Metrics/failure/memory: переиспользовать проектные средства, определить поведение miss/error до реализации; не добавлять диагностику автоматически.
- Tests: hit/miss, TTL, eviction, loading/invalidation и shutdown по затронутому контракту, с изолированными экземплярами.
- Файловые `FallbackVariant` из архитектуры остаются на диске; выбор Otter относится к данным в памяти и не меняет контракт хранения медиавариантов.

## Go GORM Profile

- GORM enabled: yes; active pair: `go_gorm.md + go-gorm`.
- Version source: будущие `go.mod`/`go.sum`; API style (Traditional/Generics) выбрать после проверки версии при реализации.
- Repository/model location: планируемый `internal/storage/`; repository interfaces и доменные сервисы — `internal/core/`.
- Transaction owner: доменный сервис определяет границы операции, storage реализует их через переданную транзакцию. Короткие транзакции, без внешнего I/O под блокировкой.
- Error translation: storage переводит ошибки драйвера/GORM в доменный контракт, API выполняет HTTP-отображение.
- Database overlay: `go-gorm-sqlite-goose`.

## Go SQLite + GORM + Goose Profile

- Go SQLite GORM Goose enabled: yes; active pair: `go_gorm_sqlite_goose.md + go-gorm-sqlite-goose`.
- Database toolkit/backend: GORM + SQLite.
- Migration owner/tool: `github.com/pressly/goose/v3`, единственный владелец production-схемы; SQL-миграции.
- Runtime dialect/driver: `gorm.io/driver/sqlite` с `mattn/go-sqlite3`/CGO как исходный выбор. Версии, SQLite engine и compile options подтвердить после фиксации модулей.
- Goose CLI/library: способ запуска пока не выбран; определить вместе с первым runner, закрепить его версию и проверить фактический SQLite-драйвер миграций.
- Driver parity: сверять `sqlite_version()`, compile options и используемые возможности runtime/migration connections; равенство движков не предполагается.
- Migration path: планируемый `migrations/sqlite/`; SQL-файлы с Goose Up и безопасным Down, когда откат применим. Схему нумерации выбрать перед первой миграцией.
- Execution owner/timing: один контролируемый владелец до готовности API; не запускать конкурентную миграцию из каждого worker/instance.
- Applied migrations: неизменяемые; исправления — следующими миграциями или отдельно согласованным восстановлением истории.
- Production backup/recovery: резервная копия перед изменениями, проверяемое восстановление с учётом WAL; разрушительные операции и реальные пользовательские БД требуют явного разрешения.
- No competing schema owner: GORM `AutoMigrate`/`Migrator` не изменяет runtime/production-схему и не подменяет Goose в тестах.
- DB location/DSN owner: конфигурация сервиса; планируемые рабочие данные в `/var/lib/gost-restreamer/`, вне Git. Точный путь файла задаётся конфигурацией.
- Foreign keys: `foreign_keys=ON` на каждом соединении, включая миграционное, с проверкой на нескольких соединениях.
- Busy timeout/pool: конкретные значения и retry policy определить при подключении БД и проверить тестами; не навязывать `MaxOpenConns(1)` без обоснования.
- Journal: WAL выбран архитектурой; короткие транзакции, одна одновременная запись, корректные checkpoint/backup.
- Process policy: один Go-сервис с независимыми выходами; координацию записи внутри процесса и возможных других процессов проверить при реализации storage.
- Integration DB: только временная файловая SQLite; реальные миграции, FK/unique/check, rollback, locks, история версий и сохранение заполненных данных при upgrade.

## Project Boundaries

Планируемые пути взяты из архитектуры и ещё не означают существующих пакетов:

- `internal/api/`: Huma/Chi, DTO, авторизация, SSE.
- `internal/core/`: домен, сервисы, repository interfaces; без импортов Huma/GORM/FFmpeg.
- `internal/storage/`: GORM/SQLite и транзакции.
- `internal/media/`, `internal/fallback/`, `internal/monitoring/`: медиатракт, варианты заглушек и наблюдение по своим обязанностям.
- `cmd/gost-restreamer/main.go`: сборка приложения и lifecycle.

Сначала проверяется медиапрототип и переключение, затем supervisor/storage/API согласно спецификации. Не считать Enhanced RTMP, UDS/NUT или непрерывный AV1 fallback реализованными и проверенными без соответствующих интеграционных результатов.

## Build And Validation Commands

Command source of truth: этот файл; будущие task runner/CI синхронизируются с ним. Используются доступные инструменты выбранного проекта; установка отсутствующих инструментов не выполняется автоматически.

Для текущего слоя инструкций:

- Синтаксис TOML: `python3` с модулем стандартной библиотеки `tomllib` (Python 3.11+) для `.codex/agents/*.toml` и `.codex/config.example.toml`.
- Профиль: проверить существование точных active rule/skill/role paths и полную цепочку объявленных hard dependencies; вручную проверить согласованность с архитектурой.
- Git: `git diff --check` для tracked изменений; новые файлы дополнительно проверяются на пробельные ошибки при сверке импорта.
- Автоматический Markdown-lint не настроен; не устанавливать его ради проверки этого слоя.

Следующие команды объявлены для этапа, когда появились `go.mod`, соответствующий код и совместимый Go toolchain; сейчас их выполнение не является проверкой этого слоя:

- Toolchain/modules: `go version`, `go env GOVERSION GOOS GOARCH CGO_ENABLED GOTOOLCHAIN`, `go list -m all`, `go mod verify`.
- Format: `gofmt -w <изменённые Go-файлы>`; check — `gofmt -l <изменённые Go-файлы>`.
- Static analysis: `go vet ./...`.
- Unit/integration tests: `go test ./...`.
- Race tests: `go test -race ./...` при поддерживаемой платформе/CGO.
- Build: `go build ./...`.
- Media E2E, fuzz, benchmarks и Goose runner commands определяются вместе с реальными целями и изолированной средой; текущих исполняемых команд для них нет.

Не запускать разрешённую команду, если её фактические побочные действия выходят за область задачи. Не представлять отсутствующие или непройденные проверки как успешные.

## Dependency Policy

- Existing dependency source of truth: пока нет манифестов; выбранные семейства стека перечислены в этом профиле и архитектуре.
- Одобрены Go, Huma v2/Chi, локальный Otter v2 и GORM/SQLite/Goose. Первые совместимые версии фиксируются при отдельно запрошенной реализации соответствующего кода, с проверкой официальных требований.
- Остальные новые runtime/dev dependencies и повышение toolchain требуют явного разрешения. Не устанавливать библиотеки, CLI, linters, контейнеры или hooks в рамках настройки инструкций.
- Наличие active profile не запускает сервисы, миграции, публикацию, трансляции или установку зависимостей.

## Material Provenance

- Source: [GregoryGost/agent-projects-tools](https://github.com/GregoryGost/agent-projects-tools).
- Source commit: `a084001df15dd6a4e0ceb3355e20d1c9ad41b296`.
- Upstream license: `licenses/agent-projects-tools.txt`.
- Local adaptations: root `AGENTS.md` targets this project and preserves the user's evidence/review/macOS instructions; this profile and the configuration example are project-owned.
- Обновлять переносимые материалы согласованно: bootstrap, routing, dependencies, coordination, роли, skills и этот профиль. Не заменять профиль полным шаблоном с чужими стеками.
