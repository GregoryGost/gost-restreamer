# Строковые значения REST и SSE

Реестр описывает строки с ограниченной семантикой. Все маршруты имеют префикс `/api/v1`; `[]` обозначает элемент списка. Наборы чувствительны к регистру. Поля передаются JSON-строками; объект Base64 не является представлением настройки. Числа, даты, идентификаторы и непрозрачные курсоры сохраняют [собственный контракт](numeric-json-contracts.md).

Обычный тип `string` сам по себе не означает отсутствие проверки. До закрытия общего контракта режимы, кодеки, блок кодировщика и query-фильтры уже проверялись; прежде открытые текстовые параметры старого профиля проверялись только структурно. Выходные состояния и причины ограничивались владельцами наблюдений, даже если схема не перечисляла их явно. Теперь DTO и схемы описывают те же конечные наборы; валидация профиля выполняется также без `encoding` и с `encoding: null`.

## Полный профиль: места использования

Пути следующей таблицы являются префиксами `P`. Каждая строка раздела «Настройки» применяется к `P.<поле>`, включая все вложенные представления. Это одна семантика полного `core.GroupProfile`, а не отдельные правила текущей конфигурации и истории.

| Маршрут и направление | Префикс P |
| --- | --- |
| `POST /transcode-profiles`, `PATCH /transcode-profiles/{id}`, запрос | `profile` |
| Создание, изменение и `GET /transcode-profiles/{id}`, ответ | `profile` |
| `GET /transcode-profiles`, ответ | `items[].profile` |
| `GET /sessions/{id}`, ответ | `groups[].profile.profile` |
| `GET /sessions`, `GET /streams/{id}/sessions`, ответ | `items[].groups[].profile.profile` |
| `GET /sessions/{id}/outputs/{outputId}`, ответ | `profile.profile` |
| `GET /sessions/{id}/outputs`, ответ | `items[].profile.profile` |
| `GET /fallback-assets/{id}/variants`, ответ | `items[].profile` |

На входе целый `profile` заменяется целиком; PATCH без него сохраняет прежний профиль. Обязательные объекты и настройки нельзя заменять `null`. В выходных полных профилях перечисленные строковые поля присутствуют, кроме отсутствующего у прежнего профиля `live_audio_mode`. `encoding: null` обозначает прежний видеорецепт и не отключает проверку общих параметров. Поля `level`/`tier` обязательны в полном HTTP-профиле: передайте пустую строку, если ограничение не задано; отсутствие и явный `null` не допускаются.

## Настройки

| Поле относительно P | Доменный тип или владелец проверки | Допустимые значения и сочетания |
| --- | --- | --- |
| `mode` | `core.GroupMode` | `copy`, `transcode`; обязательное непустое значение. |
| `format.video.codec` | `core.Codec` | `av1` при `copy`, `h264` при `transcode`. |
| `format.audio.codec` | `core.Codec` | `aac`. |
| `format.video.codec_profile` | `core.VideoFormat`, общая проверка desired | AV1: `main`; H.264: `baseline`, `main`, `high`. H.264 baseline требует нулевые B-кадры. |
| `format.video.level` | Та же проверка | H.264: `""`, `auto`, `1`, `1.0`, `1b`, `1.0b`, `1.1`, `1.2`, `1.3`, `2`, `2.0`, `2.1`, `2.2`, `3`, `3.0`, `3.1`, `3.2`, `4`, `4.0`, `4.1`, `4.2`, `5`, `5.0`, `5.1`, `5.2`. AV1: `""`, `auto`, а также `2.0`…`7.3`, последняя цифра от 0 до 3. |
| `format.video.tier` | Та же проверка | H.264: только `""`; AV1: `""` или `main`. |
| `format.video.pixel_format` | Та же проверка | `yuv420p`, глубина `bit_depth: 8`. |
| `format.video.color.range` | `core.ColorInfo`, общая проверка desired | `limited`, `full`, `tv`, `pc`; допустимые псевдонимы сохраняются точно. |
| `format.video.color.primaries` | Та же проверка | `bt709`. |
| `format.video.color.transfer` | Та же проверка | `bt709`. |
| `format.video.color.matrix` | Та же проверка | `bt709`. |
| `format.audio.codec_profile` | `core.AudioFormat`, общая проверка desired | `LC`. |
| `format.audio.channel_layout` | Та же проверка | `mono` при `channels: 1`, `stereo` при `channels: 2`. |
| `live_audio_mode` | `core.LiveAudioMode` | `copy`, `transcode`; transcode требует видео transcode. При создании отсутствие материализуется в copy. У прежнего сохранённого профиля отсутствие сохраняет прежнюю обработку. Явные `null` и `""` недопустимы. |
| `encoding.video_encoder` | `core.EncodingSettings.VideoEncoder` | `h264_nvenc` для H.264 transcode; `libsvtav1` для AV1 copy. |
| `encoding.audio_encoder` | `core.EncodingSettings.AudioEncoder` | `aac`; при пропуске в новом блоке назначается один раз. |
| `encoding.rate_control` | `core.EncodingSettings.RateControl` | `cbr`, `vbr`. |
| `encoding.nvenc.preset` | `core.NVENCSettings` | `p1`…`p7`. |
| `encoding.nvenc.tuning` | Тот же тип | `hq`, `ll`, `ull`. |
| `encoding.nvenc.multipass` | Тот же тип | `disabled`, `qres`, `fullres`. |
| `encoding.svt_av1.tuning` | `core.SVTAV1Settings` | `psnr`, `vq`; CBR допускает только psnr. |

Кроме level/tier и описанного отсутствия LIVE-селектора пустые значения недопустимы. Необязательные поля нового блока кодировщика получают [явно описанные defaults](api-contracts.md#полный-профиль-кодирования); явный `null` внутри блока запрещён. Неприменимый блок кодировщика запрещён, даже пустой. Числовые зависимости v2 сохраняются отдельно: закрытие строк не переносит их на прежний видеорецепт.

`encoding.video_custom` и `encoding.audio_custom` не являются одним перечислением. Их грамматика — `name=value name=value`: NVENC `nonref_p=0|1`, `strict_gop=0|1`; SVT `lp=0..6`; AAC `aac_coder=twoloop|fast`. Пустая строка допустима; действуют прежние пределы, нормализация, запрет дубликатов и конфликтов. Неизвестное имя или недопустимое значение отклоняется.

Недопустимые значение, регистр, сочетание, пустота, `null`, JSON-тип, UTF-8 и Base64-объект дают безопасный `422`. Ответ не повторяет присланные строки. Наличие блока кодировщика не создаёт альтернативного пути обхода общей проверки.

## Фактически проверенный формат варианта

`GET /fallback-assets/{id}/variants` возвращает `items[].format` отдельно от `items[].profile.format`. Это наблюдение `core.MediaFormat`, а не настройки. Объект присутствует также до проверки и у FAILED; его числа могут быть нулевыми, а строки пустыми. Прежнее измерение FAILED сохраняется.

Каждый набор ниже дополнительно содержит `""` и `UNKNOWN`. Пустота означает отсутствие распознанного значения; UNKNOWN — непустое внутреннее обозначение вне публичного каталога. Нераспознаваемые строки и невалидный UTF-8 не возвращаются клиенту. Числа и FPS сохраняются точно. Публичная проекция не переписывает исходный `VerifiedFormat` и сама по себе не аннулирует файл.

| Поле относительно format | Распознаваемые непустые обозначения |
| --- | --- |
| `video.codec`, `audio.codec` | `av1`, `h264`; соответственно `aac`. |
| `video.codec_profile` | H.264: `baseline`, `constrained-baseline`, `main`, `extended`, `high`, `high10`, `high422`, `high444`; AV1: `main`. |
| `video.level` | H.264: все 256 строк вида `n/10.n%10` для целого n=0…255, которые формирует действующий парсер, и прежние алиасы `1`, `1b`, `1.0b`, `2`, `3`, `4`, `5`. AV1: `2.0`…`7.3` с последней цифрой 0…3, `unspecified`. |
| `video.tier` | AV1: `main`, `high`; для H.264 непустая категория не распознаётся. |
| `video.pixel_format` | `gray`, `yuv420p`, `yuv422p`, `yuv444p`; также каждая база с суффиксом `9le`…`16le`. |
| `video.color.range` | `limited`, `full`, `tv`, `pc`, `unspecified`. |
| `video.color.primaries` | `bt709`, `unspecified`, `bt470bg`, `smpte170m`, `bt2020`. |
| `video.color.transfer` | `bt709`, `unspecified`, `smpte170m`, `linear`, `iec61966-2-1`, `bt2020-10`, `bt2020-12`, `smpte2084`, `arib-std-b67`. |
| `video.color.matrix` | `gbr`, `bt709`, `unspecified`, `bt470bg`, `smpte170m`, `bt2020nc`, `bt2020c`. |
| `audio.codec_profile`, `audio.channel_layout` | `LC`; соответственно `mono`, `stereo`. |

Эти обозначения описывают уже распознаваемые данные. Они не расширяют допустимые настройки и не подтверждают возможность их обработки. Динамические `iso-N`, `cicp:N`, `h264-profile-idc-N` и прочие значения вне таблицы проецируются в UNKNOWN.

## Состояния, виды и query

Все перечисленные выходные поля присутствуют и не равны `null`. Исключения отсутствия относятся к целому nullable-объекту, а не к строке внутри него. Поля списков имеют те же наборы, что одиночные ответы.

| Маршрут/направление и поле | Доменный тип | Набор; отсутствие и пустота |
| --- | --- | --- |
| `GET /sessions`, `/streams/{id}/sessions`, `/sessions/{id}`: `state` | `core.SessionState` | `STARTING`, `RUNNING`, `STOPPING`, `STOPPED`, `FAILED`, `INTERRUPTED`. Необязательный query state имеет тот же набор; пустой query означает отсутствие фильтра. |
| `GET /commands/{commandId}` и ответы принятых команд: `kind` | `core.CommandKind` | `start_session`, `stop_session`, `pause_session`, `resume_session`, `add_output`, `start_output`, `stop_output`, `detach_output`. |
| Те же ответы: `status` | `core.CommandStatus` | `PENDING`, `CLAIMED`, `ISSUED`, `SUCCEEDED`, `FAILED`, `INTERRUPTED`. |
| CRUD `/fallback-assets`: `kind` | `core.AssetKind` | `image`, `video`, а до загрузки — `""`. |
| Те же ответы: `state`; query state списка | `core.AssetState` | `PREPARING`, `READY`, `FAILED`; query необязателен. |
| `PUT /fallback-assets/{id}/content`: query `kind` | `core.AssetKind` | Обязательный `image` либо `video`; пустота недопустима. |
| `GET /fallback-assets/{id}/generations/{generationId}`: `kind` | `core.AssetKind` | `image`, `video`. |
| Тот же ответ: `status` | Проекция `core.PreparationState` | `queued`, `preparing`, `ready`, `error`. |
| `GET /fallback-assets/{id}/variants`: `items[].state`; query state | `core.VariantState` | `PREPARING`, `READY`, `FAILED`; query необязателен. |

Фильтры состояний проверялись до закрытия строковых схем; произвольный фильтр не означает молчаливый выбор всех записей. Сохраняемый список Outputs не принимает query state: его наблюдаемое состояние находится в RAM.

## Оперативные REST и SSE

Одинаковые DTO используются в `GET /sessions/{id}/metrics`, `GET /events/snapshot` и payload `GET /events`. Метрики выхода доступны также в ответах его чтения через текущую runtime-проекцию. Ниже R — путь к RuntimeSessionDTO: `snapshot` для метрик Session и `sessions[]` для общего снимка. В ответе Output отдельный текущий объект находится в `runtime`. В SSE объекты `input`, `group`, `output`, `ffmpeg`, `gpu` nullable и зависят от вида события; `observation` остаётся объектом. Отсутствующий объект не заменяется набором фиктивных состояний.

Для полей скоростей и компактного формата M обозначает каждый MediaMetricsDTO: `R.input.metrics.media`, `R.groups[].metrics.live_input`, `.fallback_input`, `.output`, `R.outputs[].metrics.media` и `Output.runtime.metrics.media`. В SSE те же объекты находятся в `input.metrics.media`, `group.live_input`, `group.fallback_input`, `group.output`, `output.media`. Каждый путь `M.rates` содержит отдельные `fps`, `payload_bitrate`, `written_bitrate`.

| Поле/контекст | Доменный тип или владелец | Набор и особые значения |
| --- | --- | --- |
| `SessionMetrics.availability` | API, доступность RAM | `available`, `unavailable`; при unavailable snapshot=null. |
| `SessionMetrics.reason` | API | `""`, `not_observed`, `session_terminal`. |
| `Output.runtime_availability`, `runtime_reason` | API | `available`, `unavailable`; причина `""`, `not_observed`, `execution_inactive`; при unavailable runtime=null. |
| `R.observation.state`, `R.groups[].state`, `R.ffmpeg[].state`; соответствующее SSE `observation.state` | `core.ObservationState` | `UNKNOWN`, `STARTING`, `READY`, `RETRYING`, `FAILED`, `STOPPED`; до первого значения REST публикует UNKNOWN. |
| `R.outputs[].state`, `Output.runtime.state`; соответствующее SSE `observation.state` | `core.OutputState` | `UNKNOWN`, `CONNECTING`, `PUBLISHING`, `RETRYING`, `FAILED`, `STOPPED`. |
| `R.groups[].source`, SSE `source` | `core.ObservationSource` | `NONE`, `LIVE`, `FALLBACK`; до первого наблюдения NONE. |
| `R.ffmpeg[].role`, SSE `role` | Роль FFmpeg владельца наблюдений | `""`, `live_transcode`, `fallback_player`; общий worker может не иметь роли. |
| Неприменимые SSE `observation.state`, `source`, `role` | Общая observation-проекция | `""`; применимые значения берутся из наборов соответствующего владельца. |
| `M.format.video_codec`, `audio_codec` | `core.ObservedFormat`, `core.Codec` | `""`, `av1`, `h264`, `UNKNOWN`; соответственно `""`, `aac`, `UNKNOWN`. Пустота — до распознавания; UNKNOWN — общая защитная проекция непустого нераспознанного обозначения. |
| `M.rates.fps`, `M.rates.payload_bitrate`, `M.rates.written_bitrate`: `status` | `core.MetricStatus` | `available`, `warming_up`, `unavailable`, `stale`, `saturated`, `counter_reset`, `unsupported`. |
| Те же объекты: `reason` | `core.MetricReason` | Набор причин метрик ниже. |
| `R.ffmpeg[].metrics.status/reason`, SSE `ffmpeg.status/reason` | Те же типы | Те же наборы. Нулевые Rate/FFmpeg проецируются unavailable/source_absent. |
| `gpu.status/reason` и отдельные GPU-метрики | Те же типы | Те же наборы; пустого status нет. Недоступное числовое значение — null. |
| SSE `kind` | `core.RuntimeEventKind` | `state`, `metrics`, `input`, `gpu`, `removed`, `attempt`. |
| SSE `owner` | `core.RuntimeOwnerKind` | `session`, `group`, `output`, `ffmpeg`, `system`. |
| SSE `reason` | `core.MetricReason` | Набор причин метрик ниже. |
| Событие `resync_required`: `reason` | API, причина пересинхронизации | `gap`, `reset`, `frame_limit`, `source_unavailable`. |

Закрытый набор `MetricReason`: `""`, `unsupported_platform`, `library_unavailable`, `driver_unavailable`, `not_supported`, `not_found`, `collection_failed`, `source_absent`, `disconnected`, `invalid_sample`, `counter_regression`, `counter_saturated`, `observation_failed`, `cgo_disabled`, `permission_denied`, `device_lost`, `provider_error`, `budget_exceeded`, `busy`, `stale`, `warming_up`.

Строковые поля observation обязательны внутри существующего объекта. Пустые неприменимые значения перечислены в общей SSE-схеме; их нельзя подменять состоянием другого владельца. `connected` и `heartbeat` остаются транспортными комментариями SSE, не новыми значениями RuntimeEvent.kind. Объекты метрик и progress могут отсутствовать через null по прежнему контракту.

## Возможности, здоровье и ошибки

| Маршрут/поле | Владелец набора | Значения |
| --- | --- | --- |
| `GET /system/capabilities`: каждый компонент `.status` | API capability provider | `available`, `unavailable`, `unverified`. |
| Те же компоненты `.reason` | Тот же поставщик, проверка HTTP-границы | `not_configured`, `executable_unavailable`, `executable_resolved`, `hardware_not_verified`, `unsupported_platform`. Неизвестный результат поставщика не сериализуется как допустимая возможность. |
| `GET /health/live`, `/health/ready`: `status` | API | `live`, `ready`; неуспешная readiness возвращает Problem Details с 503. |
| Те же успешные ответы: `reason` | API | `""`. |
| Любой `failure.code` истории/квитанций/наблюдений | Безопасная HTTP-проекция | `session_failed`, `session_interrupted`, `command_failed`, `command_interrupted`, `preparation_failed`, `preparation_interrupted`, `observation_failed`. При отсутствии failure весь объект null. |
| Ошибка полного профиля `.code` | `core.ProfileError.Code()` и безопасная HTTP-проекция | `invalid_profile`, `invalid_custom`, `duplicate_custom`, `custom_conflict`, `unknown_custom`. |

`message`, `title`, `detail`, `field`, `type` и `instance` ошибок сохраняют своё назначение пояснения, пути либо URI, а не превращаются в перечисления. Внешняя диагностика и отклонённые значения в ответ не переносятся. `name`, ID, публикационные ключи, stream keys, endpoint, platform, cursor и даты также не являются enum.

## Одноразовый переход хранилища

При открытии под lifetime-блокировкой Goose выполняет миграцию версии 2 до строгого чтения и Recover. В одной транзакции обновляются данные, ограничение уникальности и маркер `storage_format` с 2 на 3; версия Goose записывается вместе с ними. Первая SQL-миграция не редактируется. Повторное открытие не повторяет замену. При ошибке переход откатывается целиком.

Миграция преобразует прежние допустимые текстовые настройки, впервые закрываемые строгим контрактом, включая невалидный UTF-8. Корректные значения и псевдонимы сохраняются побайтно. Исправный блок v2 не переписывается; прежний профиль не превращается в v2 и не получает новый LIVE-селектор.

| Прежняя неподдерживаемая настройка или сочетание | Замена |
| --- | --- |
| Известный codec не соответствует корректному mode | copy → av1; transcode → h264. |
| Неподдерживаемый video profile | AV1 main; H.264 high. |
| H.264 baseline при B>0 | high, число B сохраняется. |
| Неподдерживаемый level/tier выбранного кодека | Пустая строка. |
| Неподдерживаемая пара pixel/depth | yuv420p / 8. |
| Неподдерживаемый range | limited. |
| Неподдерживаемые primaries/transfer/matrix | bt709. |
| Неподдерживаемый audio profile | LC. |
| Неверный layout при одном/двух каналах | mono/stereo соответственно. |
| Другое ранее допустимое положительное число каналов | 2 / stereo. |

Независимые битрейты, sample rate, размеры, FPS, GOP, B-кадры и остальные числа не исправляются. Неизвестная версия, повреждённая структура, исходные несогласованные ключи/SQL-проекции и нарушение уже действовавшего контракта приводят к откату. В частности, прежде запрещённые Mode/codec/live_audio_mode/encoder/RC/custom не получают выдуманные defaults: миграция настроек не является общим ремонтом повреждённой БД.

Каждая копия desired в текущем профиле, RequiredProfiles, варианте, Session snapshot и Output snapshot/ArtifactManifest преобразуется из собственных сохранённых данных. Пересчитываются связанные canonical/key. SourceFormat и raw VerifiedFormat не меняются. Идентификаторы, история команд и их идентичности, ревизии/времена, пути, holds, владение и cleanup сохраняются.

При совпадении профилей все прежние требования и VariantID остаются. Изменённые требования помечены внутренним RequiresRebuild, изменённые варианты становятся FAILED; неизменённый READY-сосед остаётся пригодным. Миграционное исключение не разрешает обычным операциям создавать дубли. PUBLISHED и текущий оригинал сохраняются для штатного rebuild.

Изменённый ArtifactManifest сохраняет историю и измерение, но его прежнее доказательство соответствия аннулировано. Проверки использования запрещают такой manifest до открытия файла/процесса, включая повторный запуск выхода и повторное открытие проигрывателя. Выполните существующий `POST /fallback-assets/{id}/rebuild` до эфира; исходник повторно загружать не нужно. Старый OutputSnapshot автоматически не получает новый файл. Миграция не запускает FFmpeg, не перекодирует и не удаляет медиаресурсы. Исполнение перехода на рабочей БД относится к обновлению сервиса, а локальные проверки используют изолированные временные БД.
