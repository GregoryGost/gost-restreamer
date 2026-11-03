# Codex Project Profile — gost-restreamer

## Project Identity

- Project key / workspace: `gost-restreamer`.
- Project type: Go backend for personal self-hosted restreaming in the owner's local environment; the OBS plugin is a separate project.
- Primary programming language: Go. SQL is used for Goose migrations.
- Architecture source: `gost-restreamer-architecture.md`, revision 6.
- Current repository state: созданы Go-модуль, Makefile, конфигурация линтера и базовый workflow CI; исходников приложения, миграций и тестов пока нет.
- Selected technology families are approved and active now. Go закреплён в `go.mod`, линтер — в `Makefile`; версии runtime-зависимостей и нереализованные настройки подтверждаются при появлении соответствующего кода.

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
- `go-observability`
- `go-security`
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
- Go rules: `go_core.md`, `go_testing.md`, `go_observability.md`, `go_security.md`, `go_huma_v2.md`, `go_local_cache.md`, `go_gorm.md`, `go_gorm_sqlite_goose.md`

Bare rule filenames resolve under `.codex/rules/`.

## Active Skills

- Code prose / language-policy skills: `comment-language-audit`
- Go skills: `go-core`, `go-testing`, `go-observability`, `go-security`, `go-huma-v2`, `go-local-cache`, `go-gorm`, `go-gorm-sqlite-goose`

Skill names resolve to `.agents/skills/<name>/SKILL.md`. Required dependency closure:

- `go_core.md` → `go-core`.
- `go_testing.md` → `go-core`, `go-testing`; `go-testing` → `go-core`.
- `go_observability.md` → `go-core`, `go-observability`; `go-observability` → `go-core`.
- `go_security.md` → `go-core`, `go-security`; `go-security` → `go-core`.
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
- Module/workspace source: `go.mod`, module path `github.com/GregoryGost/gost-restreamer`; `go.sum` создаётся после добавления зависимостей и хранится в Git. `go.work` не используется.
- Effective language/toolchain version: Go `1.27.3` в `go.mod`; локальный compiler `go1.27.3 darwin/arm64` проверен. Make проверяет точную версию, задаёт `GOTOOLCHAIN=local` и `GOWORK=off`; совместимость будущих зависимостей проверяется по выбранным тегам без автоматического обновления SDK.
- Development console: macOS. Deployment target: Debian 13 по архитектуре; workflow собирает для `linux/amd64` в `golang:1.27.3-trixie`. Фактическая аппаратная среда проверяется при целевой приёмке; локальное наличие Docker не предполагается.
- CGO policy: enabled для исходного `gorm.io/driver/sqlite`; pure-Go замена требует отдельного решения и тестов.
- Dependency integrity: фиксировать выбранные версии в `go.mod` и `go.sum`.
- Formatter/static checks: `gofmt`, `go vet`, `golangci-lint` `2.15.0`; `.golangci.yml` включает `errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused`. Версия и установка линтера заданы в `Makefile`; `make tools` устанавливает его только в `bin/` проекта. Линтер естественного языка не добавляется.
- Ownership: явные владельцы goroutine, очередей, соединений, файлов и остановки. Очереди и кэши ограничены; профилирование только по предметной необходимости, доступ к диагностике защищён.
- Delve terminal debugging policy: необязательный сценарий для воспроизводимого сбоя; проверить установленный совместимый `dlv`, Go/OS/архитектуру и доступный интерактивный stdin/TTY. Установка или обновление отладчика не выполняется автоматически.
- Delve target/attachment policy: только изолированная программа или тест, запускаемые в рамках задачи; attach к существующему процессу и удалённый/headless listener требуют отдельного разрешения.
- Debug artifacts: временные бинарные файлы и результаты диагностики — в принадлежащем текущей задаче системном временном каталоге вне репозитория; владелец сессии учитывает созданные процессы и очищает только свои артефакты.
- Startup/shutdown: composition root владеет общими зависимостями и освобождает их после завершения пользователей; отмена по сигналу, ограниченное ожидание workers/Publishers/FFmpeg и отдельный контекст завершения с таймаутом. Численные сроки определяются при реализации lifecycle.
- Health/readiness: предусмотренные архитектурой `/health/live` и `/health/ready` под префиксом `/api/v1`; liveness описывает работоспособность процесса, readiness — готовность обслуживать предусмотренные операции. Точные условия проверяются вместе с реализацией. Дополнительные listeners не выбраны.
- Module/build provenance: проверять `go.mod`, `go.sum`, возможный `go.work`, эффективный toolchain, build tags, `GOOS`/`GOARCH` и CGO. Выходные файлы сборки — в явно заданном каталоге артефактов или системном временном каталоге; генераторы и изменения модулей запускаются только в разрешённой области задачи.
- Performance/PGO: представительные измерения по конкретному вопросу с повторениями и сопоставимыми входами, версиями и флагами. PGO не выбран; `benchstat` используется только при наличии и разрешении. Профили CPU/heap/trace хранятся во временном каталоге задачи.
- Design/review: активные Go references, версия спецификации и release notes, соответствующая модулю.

## Go Application Context And Data Disclosure Policy

Это единая проектная политика, выбранная владельцем для личного приложения и применяемая при работе с `go-observability` и `go-security`. Она не пересматривается и не классифицируется заново при каждой задаче.

- Policy selection and owner approval: владелец явно выбрал личный локальный контур и режим логирования приложения `full`.
- Application role: `personal-local`; самостоятельная установка и эксплуатация в собственном локальном контуре владельца.
- Exposure/trust boundary: OBS и управляющие клиенты работают в контуре владельца; исходящие медиасоединения идут на выбранные им стриминговые площадки. Адреса привязки и транспорт задаются конфигурацией соответствующей инсталляции.
- Protected assets and accepted risks: ключи трансляций, токены, остальные учётные данные, конфигурация, медиаматериалы и история сессий. Их полные значения в локальных логах приложения — явно выбранное поведение.
- Required controls: действующие контракты архитектуры по авторизации публикаций и API, границам файлов, запуску FFmpeg, хранению данных и владению ресурсами. Дополнительная инфраструктура безопасности не выбрана.
- Application-owned log capture mode: `full`.
- Exact log field categories: токены авторизации и публикации, ключи потоков, остальные учётные данные, заголовки, параметры и тела запросов/ответов, URL назначений, идентификаторы, ошибки, аргументы и диагностика FFmpeg — `full`.
- Masking/omission format: не применяется. Значения записываемых диагностических полей сохраняются полностью; не добавлять маскирование, хеширование, усечение или исключение этих значений как скрытый режим безопасности.
- Event selection: режим `full` определяет полноту значений в записываемых событиях. Уровни и набор событий выбираются по диагностической задаче; непрерывный дамп всех медиапакетов не является требованием.
- Log sinks/readers/retention: базовый вывод приложения — структурированные записи в `stderr` внутри контура владельца. Сбор, доступ и срок хранения определяются локальным получателем этого вывода; отдельное файловое хранилище и архиватор логов в приложении не выбраны.
- External log/trace/metric export: внешние экспортёры и получатели телеметрии не выбраны. Состояния, события и метрики управляющего API/SSE остаются частью архитектуры приложения.
- Agent/PR/CI disclosure: режим логов приложения не переносится на ответы агента, PR, CI-артефакты и сторонние сервисы; для этих получателей действуют собственные разрешения.
- Profile change policy: изменения этой политики выполняются по явному решению владельца. При противоречии фактической инсталляции сообщить точное расхождение; не заменять `full` другим режимом самостоятельно.

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
- Focused Delve tests: необязательный `dlv test <package> -- -test.run <regex> -test.count=1 -test.parallel=1` для изолированной цели с интерактивным вводом. Сохранить первое падение; после исправления повторить обычный тест без отладчика и затронутые проверки.
- Benchmark regression: по конкретной задаче — представительные данные, несколько сопоставимых запусков и отдельная проверка корректности; результаты во временном каталоге задачи. Само прохождение benchmark не подтверждает SLO или отсутствие утечек.
- Logging tests: проверять полные значения синтетических токенов, заголовков и полезной нагрузки в выводе реального handler согласно режиму `full`.
- Новые тестовые библиотеки не выбраны.

## Go Observability Profile

- Go observability enabled: yes; active pair: `go_observability.md + go-observability`.
- Logging backend: стандартный `log/slog` со структурированным JSON-выводом в `stderr`; закреплённый Go `1.27.3` поддерживает этот API. Сам handler проверяется вместе с реализацией логирования.
- Event/level/key policy: стабильные имена событий, уровень, время и компонент; `stream_id`, `session_id`, идентификаторы группы/назначения и операции — там, где они применимы. Попытка подключения, reconnect и фактический результат операции учитываются отдельно.
- Logging data disclosure: только общий раздел Go Application Context And Data Disclosure Policy, режим `full`.
- Metrics/tracing: предусмотренные архитектурой состояния и счётчики в `internal/monitoring/` и управляющем API. OpenTelemetry API/SDK, tracing, collectors, exporters и sampling не выбраны.
- Metric cardinality: ограниченные измерения по компоненту, типу операции и состоянию; произвольные URL, токены и идентификаторы сессий не используются как неограниченные metric labels.
- Telemetry ownership: composition root владеет logger и выбранными средствами наблюдения. При появлении требующего flush провайдера использовать отдельный ограниченный контекст завершения и сообщать ошибки; текущий профиль не добавляет такого провайдера.
- Validation: после появления кода — изолированные `go test` с захватом вывода реального handler; проверка событий, уровней, корреляции, точных `full`-значений и фактических единиц/смысла метрик. Внешний collector для тестов не требуется.

## Go Security Profile

- Go security enabled: yes; active pair: `go_security.md + go-security`.
- Application context/accepted risks: общий раздел Go Application Context And Data Disclosure Policy; личное приложение в собственном локальном контуре.
- Technical controls/surfaces: существующие контракты авторизации OBS и управляющего API, входные данные, загрузка и пути fallback, SQL-параметры, отдельные аргументы `exec.CommandContext`, выбранный транспорт и освобождение ресурсов.
- Review scope: подтверждать достижимый путь, нарушенный контракт и конкретный эффект по фактическому коду и конфигурации. Отличать дефект от принятого владельцем поведения и необязательного усиления.
- Vulnerability/advisory review: официальная Go vulnerability database и данные для закреплённых Go/module versions. `govulncheck` не является обязательным инструментом, не установлен этим обновлением и не запускается автоматически.
- Validation/exception procedure: после появления кода — стандартные изолированные `go test`, реальные временные SQLite/HTTP-границы по области задачи и независимое ревью по действующей политике. Изменения зависимостей и исключений требуют обычной проверки области разрешений.
- Shared application logging contract: общий режим `full`; security review не вводит собственное маскирование или запрет полных локальных значений.

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
- Version source: `go.mod` и создаваемый `go.sum`; GORM пока не установлен. API style (Traditional/Generics) выбрать после проверки версии при реализации.
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

Command source of truth: этот профиль задаёт обязательные проверки, `Makefile` реализует их; `.github/workflows/go.yml` использует те же цели. Версия Go задаётся в `go.mod`, версия линтера — в `Makefile`. Явная команда `make tools` загружает и собирает закреплённый линтер в `bin/`; обычные проверки не устанавливают инструменты автоматически.

Текущая основа проверяется через `make tools`, затем `make bootstrap-check`: версия Go, `gofmt` без правки файлов, `go mod tidy -diff`, `go mod verify` и `golangci-lint config verify`. При отсутствии Go-пакетов CI явно пропускает анализ, тесты и сборку. Полная цель `make check` в таком состоянии завершается ошибкой с пояснением; прохождение bootstrap не является проверкой приложения.

Для текущего слоя инструкций:

- Синтаксис TOML: `python3` с модулем стандартной библиотеки `tomllib` (Python 3.11+) для `.codex/agents/*.toml` и `.codex/config.example.toml`.
- Профиль: проверить существование точных active rule/skill/role paths и полную цепочку объявленных hard dependencies; вручную проверить согласованность с архитектурой.
- Git: `git diff --check` для tracked изменений; новые файлы дополнительно проверяются на пробельные ошибки при сверке импорта.
- Автоматический Markdown-lint не настроен; не устанавливать его ради проверки этого слоя.

Команды для Go-кода выполняются после появления соответствующих пакетов; модульные проверки и форматирование доступны уже сейчас:

- Toolchain/modules: `go version`, `go env GOVERSION GOOS GOARCH CGO_ENABLED GOTOOLCHAIN`, `go list -m all`, `go mod verify`.
- Format: `gofmt -w <изменённые Go-файлы>`; check — `gofmt -l <изменённые Go-файлы>`.
- Static analysis: `make vet` → `go vet ./...`; `make lint` → закреплённый `golangci-lint run ./...`.
- Unit/integration tests: `go test ./...`.
- Race tests: `go test -race ./...` при поддерживаемой платформе/CGO.
- Build: `make build` → `go build -o /dev/null ./...`; проверка компиляции не оставляет бинарники в корне репозитория. Выпускаемый бинарник создаётся через `make binary`.
- Module consistency: `go mod tidy -diff` при поддержке закреплённым Go и разрешении на возможные cache/network effects. Обычный `go mod tidy` изменяет модули и требует разрешённой задачи на их изменение.
- Make: `make fmt`, `make fmt-check`, `make tidy`, `make tidy-check`, `make mod-verify`, `make test`, `make test-race`, `make build` реализуют соответствующие команды. `make check` объединяет bootstrap, анализ, тесты, race и сборку. `make binary` после появления точки входа создаёт `bin/gost-restreamer` для текущей платформы с CGO и `-trimpath`; Linux CI дополнительно создаёт архив и SHA-256 в `dist/`.
- Benchmarks: `go test -run '^$' -bench '<проверенная цель>' -benchmem -count=10 <package>` только для выбранной задачи и изолированной среды; `benchstat` — если уже доступен и разрешён.
- Delve diagnostics: `dlv version` и `dlv help test`, затем согласованная локальная цель `dlv debug`/`dlv test`/`dlv exec` с интерактивным stdin и временными артефактами. Отладчик запускает настоящий код; наблюдения не заменяют обычные тесты.
- Конкретные цели Media E2E, fuzz и benchmarks, а также команды Goose runner определяются вместе с реальными пакетами и изолированной средой.

Не запускать разрешённую команду, если её фактические побочные действия выходят за область задачи. Не представлять отсутствующие или непройденные проверки как успешные.

## Dependency Policy

- Existing dependency source of truth: `go.mod` и создаваемый по импортам `go.sum`; пока runtime-зависимостей нет. Выбранные семейства стека перечислены в этом профиле и архитектуре. Dev dependency `golangci-lint` `2.15.0` закреплён отдельно в `Makefile` и не входит в runtime-граф приложения.
- Одобрены Go, Huma v2/Chi, локальный Otter v2 и GORM/SQLite/Goose. Первые совместимые версии фиксируются при отдельно запрошенной реализации соответствующего кода, с проверкой официальных требований.
- Остальные новые runtime/dev dependencies и повышение toolchain требуют явного разрешения. Не устанавливать библиотеки, CLI, linters, контейнеры или hooks в рамках настройки инструкций.
- Наличие active profile не запускает сервисы, миграции, публикацию, трансляции или установку зависимостей.

## Material Provenance

- Source: [GregoryGost/agent-projects-tools](https://github.com/GregoryGost/agent-projects-tools).
- Source version: `v1.16.0`.
- Source commit: `f7a68e48271c78bf61401c42af62ca97c918f820`.
- Local adaptations: root `AGENTS.md` targets this project and preserves the user's evidence/review/macOS instructions; this profile and the configuration example are project-owned.
- Обновлять переносимые материалы согласованно: bootstrap, routing, dependencies, coordination, роли, skills и этот профиль. Не заменять профиль полным шаблоном с чужими стеками.
