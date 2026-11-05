# Codex Project Profile — gost-restreamer

## Project Identity

- Project key / workspace: `gost-restreamer`.
- Project type: Go backend for personal self-hosted restreaming in the owner's local environment; the OBS plugin is a separate project.
- Primary programming language: Go. SQL is used for Goose migrations.
- Architecture source: `gost-restreamer-architecture.md`, revision 10.
- Current repository state: созданы Go-модуль, Makefile, линтер и workflow CI; реализованы общие медиатипы, домен, Repository-интерфейсы, транзакционные операции сессий/команд, GORM/SQLite/Goose и общая runtime authority. Пакеты media содержат Router, raw Enhanced RTMP ingest, fMP4 и FFmpeg Supervisor/UDS; их приёмка относится к T03. Добавлены поколения и файловая подготовка fallback, группы/Switcher; их контракты и границы проверки описаны в документации. Добавлены нативные Publishers RTMP/RTMPS с независимыми очередями, переподключениями, фактическими RAM-счётчиками и физическим владением выполнением; локальная приёмка T05 завершена. SessionManager и processowner/helper реализованы и приняты локально в T06. Сборщики, API и исполняемый сервис остаются следующими задачами. Контракты описаны в `docs/media-contracts.md`, `docs/domain-storage-contracts.md` и `docs/session-manager-contracts.md`.
- Адаптация основы к редакции 8: T01–T03 сохраняют результаты прежней приёмки. Реализованы ограниченные RAM-наблюдения под общей runtime authority, отделённые от долговечного управления, и преобразование FPS/размеров по профилю. `SessionOutput` больше не сохраняет наблюдаемые состояния/ошибки; окончательный отказ проходит отдельную транзакцию с точным владельцем и событием управления. `BuildNVIDIACommand` использует явную FPS-сетку, фактические шкалы обеих дорожек и проверку точности рациональной частоты. Сбор FFmpeg `-progress`, GPU и полных оперативных метрик относится к T07; аппаратная приёмка NVIDIA остаётся T11.
- Требования редакции 9 для T04: новая загрузка без сравнения содержимого заменяет явно выбранную заглушку, остальные сохраняются; варианты подготавливаются заново, прежняя рабочая версия сохраняется при ошибке. Генерация и rebuild запрещены во время эфира, включая ручную паузу. Эти требования согласованы владельцем; прежние результаты приёмки не подтверждают их реализацию.
- Требования редакции 10 для T06: начальная готовность локальных групп выбранных выходов обязательна; отказ назначения изолирован. Прежняя терминальная Session или отозванная терминальная подготовка с неподтверждённой очисткой не запрещает новый ручной start, включая ту же incarnation. Остатки очищаются автоматически с сохранением точных удержаний и изоляцией нового выполнения. Реализация и независимая локальная приёмка T06 завершены; результат и ограничения указаны в `docs/session-manager-contracts.md`.
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
- Subagent handoff persistence: `local-ephemeral`.
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
- Правила read-only у ролей являются ограничениями поведения; разрешения и среда наследуются от основного агента. Проверять владение файлами при параллельных сессиях; не создавать worktree автоматически.
- При разрешённых текущим запросом локальных записях только основной агент сохраняет проверенные завершённые отчёты в `.agent-handoffs/<task_scope_id>/`: неизменяемые версии отчётов и `index.md` с актуальными ссылками и статусами. Каталог создаётся при первом сохранении; порядок проверки и публикации задан в `.codex/rules/subagent_coordination.md`.
- `.agent-handoffs/` исключён из Git; отчёты не коммитятся, не экспортируются и не удаляются автоматически. Режим не расширяет область текущего запроса: при запрете таких записей, в том числе в read-only и documentation-only задачах, отчёты остаются в сессии.

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
- Development console: команды выбираются по фактической ОС и shell; macOS, Windows и Linux рассматриваются как возможные среды разработки. Текущая проверенная среда — macOS/arm64; переносимость инструментов и платформозависимых проверок подтверждается при смене среды. Deployment target: Debian 13 по архитектуре; workflow собирает для `linux/amd64` в `golang:1.27.3-trixie`. Фактическая аппаратная среда проверяется при целевой приёмке; локальное наличие Docker не предполагается.
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

## Среды разработки и локальные медиапроверки

ОС разработки не закреплена за проектом. Для выбранной macOS, Windows или Linux отдельно проверяются Go/CGO, C-компилятор, Make/shell, файловые пути, UDS, блокировки и управление процессами. Команды выполняются в соответствии с фактической средой; её выбор сам по себе не подтверждает нативную сборку или прохождение всех проверок на этой ОС. Целевая серверная среда остаётся Debian 13 / `linux/amd64` / RTX A1000.

Обязательные проверки Go/CI выполняются без FFmpeg, GPU и внешних площадок. Короткий CPU round-trip Go ↔ FFmpeg проверяется отдельно при реализации медиакомпонентов. Дополнительные аппаратные интеграционные проверки используют доступный в выбранной среде API и явно заданную тестовую команду FFmpeg; проверяются те же рабочие компоненты Go и медиаконтракты. Доступность VideoToolbox или другого локального ускорителя не является требованием к машине разработчика. Эти подстановки принадлежат тестам; автоматическая замена production-пути NVDEC/NVENC ими не вводится. Параметры NVIDIA и её аппаратная/live-совместимость проверяются отдельно в целевой среде.

Пример доступной среды, проверенный 4 ноября 2026 года:

- macOS `27.0.1` / `arm64`, MacBook Air с Apple M4, 10-ядерными CPU и GPU, 16 ГБ памяти; установлены FFmpeg и ffprobe `7.1.5`.
- В короткой синтетической проверке перед реализацией T03 прошли 15 кадров `640×360` при `30 FPS`: AV1/AAC → NUT/pipe → `libdav1d` на CPU → `h264_videotoolbox` с `-allow_sw 0` и копированием AAC → NUT/pipe → успешное декодирование H.264/AAC. Это подтверждает доступность локальной аппаратной H.264-подстановки; рабочая Go/UDS-интеграция проверяется отдельно.
- Apple заявляет аппаратное декодирование AV1 в [Media Engine M4](https://support.apple.com/en-us/122209), однако попытка использовать его через VideoToolbox в проверенной связке FFmpeg/macOS завершилась ошибкой. Для этой локальной проверки AV1 декодировался через `libdav1d`; аппаратное декодирование не считается подтверждённым.
- Аппаратная H.264-проверка внутри sandbox Codex завершилась ошибкой создания сессии `-12908`; тот же запуск вне sandbox прошёл. Доступ к ускорителю проверяется в контексте запуска тестового процесса с соблюдением действующих разрешений.

Эти наблюдения относятся только к указанной среде и короткому тесту. Они не подтверждают 1440p60, длительную устойчивость, CUDA/NVDEC/NVENC, целевые площадки или переносимость на другую ОС. Другая машина разработки использует собственную проверенную конфигурацию; macOS и VideoToolbox остаются одним из возможных вариантов.

В отдельной приёмке T12 рабочая цепочка Go/UDS/fMP4/Supervisor прошла преобразование короткого AV1 1440p60 в H.264 1080p30 через `h264_videotoolbox -allow_sw 0` с перекодированием AAC. AV1-декодирование, `fps` и масштабирование в этой тестовой команде выполняются на CPU; аппаратно проверено кодирование H.264. Отдельные CPU-сценарии покрывают рациональную частоту, положительные/отрицательные начала и общий транспортный сдвиг. Размеры, все PTS/DTS, длительности, число кадров/сэмплов и A/V сверены с независимыми ожиданиями. Это короткая локальная проверка, без вывода о производительности, длительной устойчивости или NVIDIA.

## Go Application Context And Data Disclosure Policy

Это единая проектная политика, выбранная владельцем для личного приложения и применяемая при работе с `go-observability` и `go-security`. Она не пересматривается и не классифицируется заново при каждой задаче.

- Policy selection and owner approval: владелец явно выбрал личный локальный контур и режим логирования приложения `full`.
- Application role: `personal-local`; самостоятельная установка и эксплуатация в собственном локальном контуре владельца.
- Exposure/trust boundary: OBS и управляющие клиенты работают в контуре владельца; исходящие медиасоединения идут на выбранные им стриминговые площадки. Адреса привязки и транспорт задаются конфигурацией соответствующей инсталляции.
- Management API access: по решению владельца управляющий API в локальной сети работает без авторизации и токенов доступа; это относится к REST, SSE, OpenAPI/docs/schema и health.
- Protected assets and accepted risks: ключи трансляций, токены, остальные учётные данные, конфигурация, медиаматериалы и история сессий. Их полные значения в локальных логах приложения — явно выбранное поведение.
- Required controls: проверка постоянного ключа Stream при публикации OBS и привязка к активной Session, валидация входных данных API, границы файлов, запуск FFmpeg, хранение данных и владение ресурсами. Дополнительная инфраструктура безопасности не выбрана.
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
- Media integration / E2E: `tests/integration/`; FFmpeg/GPU и реальные площадки только в явно разрешённой тестовой среде. CPU round-trip и дополнительные аппаратные проверки разделяются по [профилю среды разработки](#среды-разработки-и-локальные-медиапроверки); macOS/VideoToolbox не становятся обязательными зависимостями CI. Успешная сборка не подтверждает live fallback.
- Race: `go test -race ./...` при поддерживаемой платформе/toolchain/CGO; не считать проверкой утечек.
- Parallel/shuffle: только при доказанной изоляции БД, окружения, портов и глобального состояния. `t.Setenv` используется в последовательных тестах.
- Statement coverage enabled: yes; владелец явно выбрал обязательный порог 85% при обновлении материалов до `v1.16.1`.
- Statement coverage minimum: 85% суммарного покрытия инструкций, взвешенного по числу инструкций; `100 * coveredStatements >= 85 * totalStatements`, без сравнения округлённого процента.
- Covered production package set and exclusions: все пакеты текущего Go-модуля `./...`, включая инструмент проверки покрытия; исключений нет. Используются фактические GOOS/GOARCH и CGO, без дополнительных build tags. Отдельные FFmpeg/GPU/live-проверки не входят в обязательный запуск; их отсутствие и платформенно недоступные пути указываются как непроверенные, без исключения доступного медиакода из покрытия.
- Coverage execution command and environment: `make coverage` выполняет `go test -count=1 -covermode=atomic -coverpkg=./... -coverprofile=<fresh-profile> ./...` с закреплённым Go, `CGO_ENABLED=1`, `GOTOOLCHAIN=local`, `GOWORK=off` и штатным `GOFLAGS=-mod=readonly` по Makefile. Для этого запуска `GOST_MEDIA_FFMPEG`, `GOST_FMP4_FFMPEG` и `GOST_MEDIA_VIDEOTOOLBOX` явно пусты; отдельные проверки включаются другими тестовыми командами. `GOST_TEST_PROCESS_HELPER_COVERDIR` указывает тестам свежий каталог: настоящий `cmd/gost-process-helper` собирается с `-cover -covermode=atomic -coverpkg=github.com/GregoryGost/gost-restreamer/...` и получает этот каталог через `GOCOVERDIR`. После успешного полного запуска `go tool covdata textfmt -hw` преобразует данные действительно исполненных helper; строки их профиля объединяются с основным после проверки режима `atomic`. Исходники остаются неизменными во время сборки и исполнения. В обычных тестах и race специальная переменная не задаётся; рабочий бинарник собирается без инструментации покрытия.
- Coverage profile artifact path and retention: новый отдельный системный временный каталог для каждого запуска; основной и промежуточный профили, данные helper и текстовый отчёт удаляются владельцем команды после её завершения. Профили не хранятся в исходниках, не коммитятся и не отправляются внешним сервисам.
- Coverage gate implementation: `tools/coveragecheck`, только стандартная библиотека Go; нормализация повторяющихся блоков основного и дочернего профилей по правилам Go, проверка формата и точное сравнение порога. Дубли не увеличивают число инструкций. `make coverage`, `make check` и CI используют одну проверку. Ошибка тестов, преобразования данных helper или недостаточное покрытие завершают команду с ошибкой; отсутствующий, пустой, некорректный профиль, несовместимые режимы или отсутствие исполняемых инструкций не дают `PASS`.
- Coverage reporting and comparison baseline: команда, версия Go, область пакетов, результат тестов, расположение профиля, числитель/знаменатель, процент, порог и фактически выполненная проверка отражаются в результате. Пропуски и недоступные проверки указываются отдельно. Историческая база пока не закреплена; сравнение допускается только для проверенных сопоставимых запусков. Порядок проверки задан в `.agents/skills/go-testing/references/coverage-policy.md`.
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
- Граница оперативных и долговечных данных: текущие наблюдаемые состояния, метрики входа/групп/назначений, FFmpeg/GPU, reconnect и ошибки — в RAM; диагностика — в журнале приложения, оперативные события — в ограниченном потоке SSE. Телеметрия и её агрегаты не сохраняются в SQLite. Долговечными остаются конфигурации, история запусков, жизненный цикл Session и протокол команд/поколений/удержаний/cleanup, включая служебные записи управления и итоговые причины завершения. Основа уже отделяет минимальные текущие наблюдения от SQLite; счётчики и сборщики реализуются в T07, SSE — в T08.
- Metric cardinality: ограниченные измерения по компоненту, типу операции и состоянию; произвольные URL, токены и идентификаторы сессий не используются как неограниченные metric labels.
- Telemetry ownership: composition root владеет logger и выбранными средствами наблюдения. При появлении требующего flush провайдера использовать отдельный ограниченный контекст завершения и сообщать ошибки; текущий профиль не добавляет такого провайдера.
- Владение оперативным состоянием: реестр минимальных наблюдений принадлежит общей `RuntimeAuthority` Store, использует типизированных владельцев, точные tokens/attempts и явные `ObservationLimits` без вытеснения. После завершения и рестарта текущие значения недоступны; будущие REST/SSE не подменяют их сохранённой историей. SessionManager использует этот реестр; сборщики и REST/SSE интегрируются в T07–T09. Реестр не заменяется кэшем Otter.
- Validation: после появления кода — изолированные `go test` с захватом вывода реального handler; проверка событий, уровней, корреляции, точных `full`-значений и фактических единиц/смысла метрик. Внешний collector для тестов не требуется.

## Go Security Profile

- Go security enabled: yes; active pair: `go_security.md + go-security`.
- Application context/accepted risks: общий раздел Go Application Context And Data Disclosure Policy; личное приложение в собственном локальном контуре.
- Technical controls/surfaces: проверка постоянного ключа Stream и активной Session для OBS, входные данные управляющего API, загрузка и пути fallback, SQL-параметры, прямые аргументы `os/exec`, отдельный process helper и доказательства освобождения ресурсов. Управляющий API и SSE работают без авторизации и токенов доступа по общей политике проекта.
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
- Errors: доменные ошибки переводятся на HTTP-границе; поведение Problem Details сверяется с выбранной версией.
- API access: REST и SSE работают без авторизации и токенов доступа; middleware авторизации и требования учётных данных в OpenAPI не вводятся.
- OpenAPI/docs/schema exposure: точные пути определить до реализации публикации API; OpenAPI/docs/schema и health доступны без авторизации и токенов. Доступ проверить контрактными тестами.
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
- Version source: `go.mod` и `go.sum`; GORM `v1.31.2`, Traditional API.
- Repository/model location: `internal/storage/`; repository interfaces, UnitOfWork и доменные сервисы — `internal/core/`.
- Transaction owner: доменный сервис определяет границы операции, storage реализует их через переданную транзакцию. Короткие транзакции, без внешнего I/O под блокировкой.
- Error translation: storage переводит ошибки драйвера/GORM в доменный контракт, API выполняет HTTP-отображение.
- Database overlay: `go-gorm-sqlite-goose`.

## Go SQLite + GORM + Goose Profile

- Go SQLite GORM Goose enabled: yes; active pair: `go_gorm_sqlite_goose.md + go-gorm-sqlite-goose`.
- Database toolkit/backend: GORM + SQLite.
- Migration owner/tool: `github.com/pressly/goose/v3`, единственный владелец production-схемы; SQL-миграции.
- Runtime dialect/driver: `gorm.io/driver/sqlite v1.6.0`, `github.com/mattn/go-sqlite3 v1.14.22`/CGO. Локально на Darwin/arm64 измерен SQLite `3.45.1`; полные compile options доступны через `Store.Diagnostics`. Целевая Linux-среда проверяется отдельно.
- Goose CLI/library: `github.com/pressly/goose/v3 v3.27.0`, library runner с embedded SQL и `DialectSQLite3`.
- Driver parity: GORM и Goose используют один underlying CGO `sql.DB`; версия/compile options проверяются на нескольких физических соединениях. Отдельный CLI с другим движком не используется.
- Migration path: `migrations/sqlite/`, последовательная нумерация с `00001_initial.sql`. Первая схема не поддерживает разрушительный Down; допустимые будущие откаты определяются отдельно.
- Execution owner/timing: `storage.Open` получает lifetime-блокировку, выполняет миграции и прерывает незавершённые состояния прежнего владельца до возврата. БД закрывается после пользователей; startup API выполняется позднее в composition root.
- Политика текущего этапа разработки: по решению владельца схема изменяется в `00001_initial.sql`, без добавления миграций обновления и переноса прежних БД, созданных в ходе разработки. Существующие пользовательские БД автоматически не пересоздаются. Проверяются создание новой БД, повторное открытие заполненной БД текущей схемы и восстановление протокола управления. Правило неизменяемости применённых миграций для поставленных версий остаётся общей политикой.
- Production backup/recovery: резервная копия перед изменениями, проверяемое восстановление с учётом WAL; разрушительные операции и реальные пользовательские БД требуют явного разрешения.
- No competing schema owner: GORM `AutoMigrate`/`Migrator` не изменяет runtime/production-схему и не подменяет Goose в тестах.
- DB location/DSN owner: `storage.Open` получает путь файла и явные `storage.Options`; будущая конфигурация сервиса задаст рабочие данные в `/var/lib/gost-restreamer/`, вне Git. Cooperative sidecar lock использует канонический путь; hardlink DB не поддерживается.
- Foreign keys: `foreign_keys=ON` на каждом соединении, включая миграционное, с проверкой на нескольких соединениях.
- Busy timeout/pool: конечные бюджеты соединений, lifetime/idle time, busy timeout и retry передаются явно и валидируются; молчаливых численных defaults нет. Повторяется лишь полностью откатившаяся операция БД, с ограничением попыток/срока/контекста; `MaxOpenConns(1)` не навязывается.
- Journal: WAL, synchronous FULL, private cache; короткие IMMEDIATE write transactions, согласованные DEFERRED reads и один одновременный writer. Эксплуатационные checkpoint/backup проверяются при поставке.
- Process policy: один lifetime-владелец storage; второй сотрудничающий opener отклоняется до миграций/recovery. Удержания выданных внешних попыток сохраняются до подтверждённого cleanup; file lock сам по себе не доказывает остановку процессов.
- Integration DB: только временная файловая SQLite; реальные миграции, FK/unique/check, rollback, locks, история версий и сохранение заполненных данных при upgrade.

## Project Boundaries

Пути и обязанности взяты из архитектуры. Реализованы домен, storage и компоненты медиатракта T03–T05: ingest, FFmpeg/UDS, поколения fallback, группы/Switcher и независимые Publishers. `internal/session`, `internal/processowner` и `cmd/gost-process-helper` приняты локально в T06. Сборщики, API и composition root остаются следующими задачами:

- `internal/api/`: Huma/Chi, DTO, SSE; управляющий API без авторизации и токенов доступа.
- `internal/core/`: домен, сервисы, repository interfaces; без импортов Huma/GORM/FFmpeg.
- `internal/storage/`: GORM/SQLite и транзакции.
- `internal/media/`, `internal/fallback/`, `internal/monitoring/`: медиатракт, варианты заглушек и наблюдение по своим обязанностям.
- `cmd/gost-restreamer/main.go`: сборка приложения и lifecycle.

Сразу реализуются рабочие пакеты Go backend; отдельный прототип и последующий перенос или переписывание его кода не предусматриваются. Проверки медиаконтрактов, обмена Go ↔ FFmpeg, переключения и изоляции отказов входят в реализацию соответствующих компонентов.

Аппаратная и live-приёмка выполняются отдельно в разрешённой целевой среде; отсутствие доступа к RTX A1000 не блокирует локальную реализацию и её проверки. В T03 владелец выбрал raw-адаптер `gortmplib v1.0.1` и, после контрпримера NUT, fMP4 через UDS. [Границы AV1, fMP4 и конкретного FFmpeg CLI-порта](docs/media-contracts.md#контейнер-и-конкретная-граница-ffmpeg) применяются вместе с общими signed Packet. Не считать Enhanced RTMP, UDS/fMP4 или непрерывный AV1 fallback реализованными и проверенными без соответствующих интеграционных результатов.

## Build And Validation Commands

Command source of truth: этот профиль задаёт обязательные проверки, `Makefile` реализует их; `.github/workflows/go.yml` использует те же цели. Версия Go задаётся в `go.mod`, версия линтера — в `Makefile`. Явная команда `make tools` загружает и собирает закреплённый линтер в `bin/`; обычные проверки не устанавливают инструменты автоматически.

Для подготовки инструмента используется явная команда `make tools`; установленный закреплённый линтер повторно устанавливать не требуется. `make bootstrap-check` проверяет версию Go, `gofmt` без правки файлов, `go mod tidy -diff`, `go mod verify` и `golangci-lint config verify`. Полная цель `make check` дополнительно проверяет существующие Go-пакеты, включая общие контракты. При отсутствии пакетов CI явно пропускает их анализ, тесты и сборку, а `make check` завершается ошибкой с пояснением. Прохождение bootstrap не является проверкой приложения.

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
- Coverage: `make coverage`; полный запуск тестов, свежий профиль, `go tool cover -func=<profile>` и точная проверка минимума 85% через `tools/coveragecheck`.
- Race tests: `go test -race ./...` при поддерживаемой платформе/CGO.
- Build: `make build` → `go build -o /dev/null ./...`; проверка компиляции не оставляет бинарники в корне репозитория. Выпускаемый бинарник создаётся через `make binary`.
- Module consistency: `go mod tidy -diff` при поддержке закреплённым Go и разрешении на возможные cache/network effects. Обычный `go mod tidy` изменяет модули и требует разрешённой задачи на их изменение.
- Make: `make fmt`, `make fmt-check`, `make tidy`, `make tidy-check`, `make mod-verify`, `make test`, `make coverage`, `make test-race`, `make build` реализуют соответствующие команды. `make check` объединяет bootstrap, анализ, полный запуск тестов с порогом покрытия 85%, race и сборку; CI использует ту же цель `coverage`. `make binary` после появления точки входа создаёт `bin/gost-restreamer` для текущей платформы с CGO и `-trimpath`; Linux CI дополнительно создаёт архив и SHA-256 в `dist/`.
- Benchmarks: `go test -run '^$' -bench '<проверенная цель>' -benchmem -count=10 <package>` только для выбранной задачи и изолированной среды; `benchstat` — если уже доступен и разрешён.
- Delve diagnostics: `dlv version` и `dlv help test`, затем согласованная локальная цель `dlv debug`/`dlv test`/`dlv exec` с интерактивным stdin и временными артефактами. Отладчик запускает настоящий код; наблюдения не заменяют обычные тесты.
- Конкретные цели Media E2E, fuzz и benchmarks, а также команды Goose runner определяются вместе с реальными пакетами и изолированной средой.

Не запускать разрешённую команду, если её фактические побочные действия выходят за область задачи. Не представлять отсутствующие или непройденные проверки как успешные.

## Dependency Policy

- Existing dependency source of truth: `go.mod` и `go.sum`; закреплены GORM `v1.31.2`, SQLite dialect `v1.6.0`, CGO driver `v1.14.22`, Goose `v3.27.0`, выбранный владельцем `gortmplib v1.0.1` и необходимые codec/box API его графа `mediacommon/v2 v2.9.3`, `go-mp4 v1.7.1`. Выбранные семейства остальных слоёв перечислены в этом профиле и архитектуре. Dev dependency `golangci-lint` `2.15.0` закреплён отдельно в `Makefile` и не входит в runtime-граф приложения.
- Одобрены Go, Huma v2/Chi, локальный Otter v2 и GORM/SQLite/Goose. Первые совместимые версии ещё не подключённых слоёв фиксируются при реализации использующего их кода, с проверкой официальных требований.
- Остальные новые runtime/dev dependencies и повышение toolchain требуют явного разрешения. Не устанавливать библиотеки, CLI, linters, контейнеры или hooks в рамках настройки инструкций.
- Наличие active profile не запускает сервисы, миграции, публикацию, трансляции или установку зависимостей.

## Material Provenance

- Source: [GregoryGost/agent-projects-tools](https://github.com/GregoryGost/agent-projects-tools).
- Source version: `v1.16.1`.
- Source commit: `b3fa6c24e3f12ca2337100d21f74081862daeb12`.
- Local adaptations: root `AGENTS.md` targets this project and preserves the user's evidence/review and platform-specific console instructions; this profile and the configuration example are project-owned.
- Обновлять переносимые материалы согласованно: bootstrap, routing, dependencies, coordination, роли, skills и этот профиль. Не заменять профиль полным шаблоном с чужими стеками.
