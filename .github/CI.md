# Workflow Go и установочные пакеты

Исполняемый workflow — [workflows/go.yml](workflows/go.yml). Проверки, планировщик версий и публикация тегов сохранены; упаковка CI выполняется GoReleaser Community `2.19.0` со встроенным nFPM. Используются поддерживаемые возможности [draft и extra_files](https://goreleaser.com/customization/publish/scm/), [архивов](https://goreleaser.com/customization/package/archive/) и [nFPM](https://goreleaser.com/customization/package/nfpm/); конфигурации проверяются самим закреплённым executable.

| Параметр | Значение |
| --- | --- |
| Платформа | Только Debian 13 (`trixie`), `linux/amd64`, `GOAMD64=v1`. |
| Окружение | `ubuntu-24.04`, контейнер `golang:1.27.3-trixie`, `CGO_ENABLED=1`. |
| Go / линтер | Go `1.27.3` из `go.mod`; `golangci-lint` `2.15.0` из `Makefile`. |
| Запуск | Только `pull_request: closed` в `develop` или `main`, с `merged == true`. |
| Версии | Закреплённый `GregoryGost/version-tagger v1.2.0`, локальный план и повторное использование версии commit. |
| Конкурентность | Общая группа двух веток, `cancel-in-progress: false`, `queue: max`. |
| Результат develop | Dev-тег и Actions Artifact: `.deb`, `.tar.gz`, `SHA256SUMS`; GitHub Release отсутствует. |
| Результат main | Stable-тег и Draft GitHub Release с тремя файлами; `draft: true`, `prerelease: false`. |

Пуши в ветки и теги, открытие и обновление PR не запускают workflow. Закрытие PR без merge пропускает задание. Тег через `GITHUB_TOKEN` не запускает новый workflow: draft создаётся в том же задании после публикации тега. Docker-образы приложения не создаются.

## Проверки и порядок выпуска

1. Checkout точного `pull_request.merge_commit_sha`, полная история и теги, без сохранения Git credentials. Actions закреплены по SHA; общий доступ — `contents: read`, задание выпуска получает `contents: write`.
2. `tools/releaseplan` проверяет HEAD и теги. Неоднозначные версии, неподдерживаемые version-like теги и metadata, не совпадающие с commit, останавливают выпуск. Для существующего тега того же commit и канала используется прежний номер. Новый номер должен совпасть с `version-tagger` в режиме `dryrun: true`.
3. `make bootstrap-check` проверяет Go, форматирование, `go mod tidy -diff`, `go mod verify` и конфигурацию линтера. `make vet lint coverage test-race build` сохраняет scope `./...` и точный порог покрытия 85% без исключений. Пропуски при отсутствии Go-пакетов или точки входа явно отражаются в сводке.
4. GoReleaser Action устанавливает Community `2.19.0`. `make release-check` проверяет обе конфигурации и запускает тесты выпуска через Node.js `24` из `actions/github-script`; npm-зависимостей нет.
5. Назначенный тег создаётся локально до компиляции и проверяется по commit. `make package-debian VERSION=…` использует общую [.goreleaser.yaml](../.goreleaser.yaml): для dev — `--snapshot --skip=publish`, для stable — `--skip=publish`. В конфигурации упаковки дополнительно стоит `release.disable: true`. Бинарники собираются с CGO, `-trimpath`, `-buildvcs=true` и одной полной `VERSION` в `internal/buildinfo`.
6. [verify-packages.sh](scripts/verify-packages.sh) проверяет SHA256, состав, архитектуру и версию `.deb`, права, conffiles, maintainer scripts, равенство бинарников в двух форматах, `--version` и встроенные Go/VCS-метаданные. `dpkg --compare-versions` проверяет порядок dev/stable и переход следующего цикла. Время всех записей архива фиксируется датой commit; [package-reproducibility.sh](scripts/package-reproducibility.sh) меняет только даты YAML/unit, повторно упаковывает тот же checkout и требует прежние SHA256 всех трёх файлов, затем восстанавливает даты.
7. [package-lifecycle.sh](scripts/package-lifecycle.sh) выполняет установку, обновление, remove и purge **в одноразовом Debian-контейнере задания**. Для обновления используется копия пакета с увеличенной тестовой версией и изменённым примером YAML. Проверяются сохранение изменённых конфигурации/unit и синтетических SQLite/медиа/других данных, права и бинарники. `--force-depends` относится только к этой проверке: она не устанавливает systemd/FFmpeg и не запускает службу. Живой systemd этим не проверяется.
8. Перед push исходный набор ссылок тегов сравнивается с удалённым. Изменение набора останавливает выпуск; конфликт ссылки не допускает force push или смену версии собранных файлов. Публикуется точный проверенный тег, затем проверяется удалённая ссылка. Credentials передаются окружением шага и не сохраняются в checkout.
9. `develop` загружает только три итоговых файла в Actions Artifacts. Имя содержит версию, run ID и attempt; перезаписи прежних попыток нет. Ветка не выполняет шаги GitHub Release и не получает токен публикации GoReleaser.
10. `main` проверяет релизы через [release.cjs](scripts/release.cjs). При отсутствии релиза GoReleaser с [.goreleaser-publish.yaml](../.goreleaser-publish.yaml) создаёт draft и прикрепляет проверенные файлы через `release.extra_files`. Сборка пропускается настройкой `builds.skip`, архивы/nFPM — `--skip=archive,nfpm`, checksum — `checksum.disable`. Каталог `dist/publish` отделён от готовых файлов. Pro-функции `continue` и `before_publish` не нужны.
11. Финальная проверка main требует точный тег/commit, Draft, `prerelease=false` и полный набор файлов с совпадающими размерами/SHA256. Владелец открывает Releases → Draft → Publish release **вручную после успешного workflow**.

## Повторные запуски и ручная публикация

Полная пагинация учитывает draft и опубликованные релизы. Более одного релиза для одного тега, неизвестный файл, неверный размер/состояние/SHA256 или изменившийся тег останавливают обработку. Для существующего draft сначала сверяются все имеющиеся файлы; затем дозагружаются отсутствующие, контрольные суммы — последними. Загруженные файлы не заменяются и не удаляются. При отличающихся байтах повторной сборки CI останавливается, сохраняя прежний draft для разбора владельцем.

Опубликованный релиз пропускается без изменения описания, состояния и файлов. Ни один шаг не публикует draft автоматически. Перед каждой дозагрузкой проверяется, что релиз ещё draft. GitHub API не предоставляет атомарного условия Draft вместе с upload, поэтому не публикуйте релиз вручную одновременно с работающим или повторно запущенным workflow.

## Скачивание и хранение

Dev: GitHub → Actions → успешный запуск нужного merge → Artifacts → `gost-restreamer-<версия>-linux-amd64-<run_id>-<attempt>`. Распакуйте ZIP GitHub; внутри находятся `.deb`, `.tar.gz` и `SHA256SUMS`. Срок хранения — 14 дней с учётом политики репозитория/организации; артефакт также исчезает при удалении запуска. Истечение хранения не удаляет Git-тег и не создаёт GitHub Release. Правила доступа и скачивания описаны в [GitHub Docs](https://docs.github.com/en/actions/managing-workflow-runs/downloading-workflow-artifacts).

Stable: до публикации файлы доступны владельцу в Draft Release; после ручной публикации — в обычном GitHub Release. Отдельного хранилища пакетов нет. Установка/обновление и служба описаны в [deployment.md](../docs/deployment.md), преобразование версий — в [versioning.md](../docs/versioning.md).

`queue: max` допускает до 100 ожидающих запусков; порядок не гарантирует порядок merge. Ограничения заданы [GitHub Actions](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency). После checkout точный `$GITHUB_WORKSPACE` добавляется в `safe.directory` временного контейнера, без wildcard-доверия ко всем путям.

Обязательные Go-тесты по-прежнему не требуют FFmpeg, GPU или внешних площадок. Локальные тесты с подставным GitHub API и проверка конфигурации не подтверждают удалённый запуск, разрешения `GITHUB_TOKEN`, выдачу Artifacts или создание draft. Debian/CGO, dpkg lifecycle и настоящий systemd отмечаются проверенными только после выполнения соответствующих сред. Прежний успешный T10 на Debian 12 остаётся историческим результатом; новый workflow требует собственной приёмки.
