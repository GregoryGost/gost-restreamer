# Строковые значения REST и SSE

Реестр описывает строки с ограниченной семантикой. Все маршруты имеют префикс `/api/v1`; `[]` обозначает элемент списка. Наборы чувствительны к регистру. Поля передаются JSON-строками; объект Base64 не является представлением настройки. Числа, даты, идентификаторы и непрозрачные курсоры сохраняют [собственный контракт](numeric-json-contracts.md).

DTO, схемы и единая доменная проверка описывают конечные наборы. Отсутствующий/null encoding допускается только как немедленно материализуемое сокращение текущего полного профиля; обхода проверки строк и сочетаний нет.

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

На входе profile заменяется целиком; PATCH без него сохраняет значение. Обязательные объекты и настройки нельзя заменить null. Выходной профиль всегда содержит явный live_audio_mode и полный объект encoding. Поля level/tier обязательны: пустая строка означает отсутствие ограничения, отсутствие и null запрещены. Полей версии возможностей нет.

## Настройки

| Поле относительно P | Доменный тип или владелец проверки | Допустимые значения и сочетания |
| --- | --- | --- |
| `mode` | `core.GroupMode` | `copy`, `transcode`; обязательное непустое значение. |
| `format.video.codec` | `core.Codec` | `av1` при copy, `h264` при transcode, `hevc`/`vp9` при copy либо transcode. |
| `format.audio.codec` | `core.Codec` | `aac`, `opus`, `flac`, `mp3`, `ac3` с точными зависимостями аудиопривязок. |
| `format.video.codec_profile` | `core.VideoFormat`, Проверка желаемого формата по кодеку и кодировщику | AV1 `main/high/professional`, H.264 `baseline/constrained-baseline/main/high/high10/high422/high444`, HEVC `main/main10`, VP9 `profile0/profile2` по матрице encoder/pixel/depth. Baseline/constrained-baseline требуют B0. |
| `format.video.level` | Та же проверка | H.264: `""`, `auto`, `1`, `1.0`, `1b`, `1.0b`, `1.1`, `1.2`, `1.3`, `2`, `2.0`, `2.1`, `2.2`, `3`, `3.0`, `3.1`, `3.2`, `4`, `4.0`, `4.1`, `4.2`, `5`, `5.0`, `5.1`, `5.2`, `6.0`, `6.1`, `6.2`; последние три только с применимым CBR/VBR. AV1: `""`, `auto`, `2.0`…`7.3` с последней цифрой 0…3, high tier по отдельной таблице; AOM требует пустой уровень/auto. HEVC/VP9: только `""`/`auto`. |
| `format.video.tier` | Та же проверка | H.264/VP9: только `""`; AV1: `""`, `main`, либо `high` только с SVT CBR и явным применимым уровнем; HEVC: `""`/`main`. |
| `format.video.pixel_format` | Та же проверка | `yuv420p`, `yuv420p10le`, `yuv422p`, `yuv444p`, `yuv422p10le`, `yuv444p10le`, `gray` с точной глубиной 8/10 и применимым encoder/profile. |
| `format.video.color.range` | `core.ColorInfo`, общая проверка desired | `limited`, `full`, `tv`, `pc`; допустимые псевдонимы сохраняются точно. |
| `format.video.color.primaries` | Та же проверка | `bt709`, `bt470m`, `bt470bg`, `smpte170m`, `smpte240m`, `film`, `bt2020`. |
| `format.video.color.transfer` | Та же проверка | `bt709`, `gamma22`, `gamma28`, `smpte170m`, `smpte240m`, `linear`, `iec61966-2-1`, `bt2020-10`, `bt2020-12`; `smpte2084`/`arib-std-b67` только с применимой HDR-парой, BT.2020/BT.2020nc, известным диапазоном и 10 битами. |
| `format.video.color.matrix` | Та же проверка | `bt709`, `fcc`, `bt470bg`, `smpte170m`, `smpte240m`, `bt2020nc`, `bt2020c`; последний требует цветной формат и transfer bt709/smpte170m/bt2020-10/bt2020-12. |
| `format.audio.codec_profile` | `core.AudioFormat`, Проверка желаемого формата по кодеку | AAC `LC`, Opus `celt`, FLAC `s16`, MP3 `mpeg1-layer3`, AC-3 `ac3`. Чужой профиль не подходит другому кодеку. |
| `format.audio.channel_layout` | Та же проверка | AAC: `mono`/1, `stereo`/2, `2.1`/3, `3.0`/3, `4.0`/4, `5.0`/5, `5.1`/6, `7.1`/8; остальные конечные привязки — только `stereo`/2. |
| `live_audio_mode` | `core.LiveAudioMode` | `copy`, `transcode`; аудио transcode требует видео transcode. Создание при отсутствии выбирает copy; полная замена явного профиля требует селектора. Null и пустая строка запрещены. |
| `encoding.video_encoder` | `core.EncodingSettings.VideoEncoder` | H.264 transcode: `h264_nvenc`/`libx264`; AV1 copy: `libsvtav1`/`libaom-av1`; HEVC copy/transcode: `libx265`/`hevc_nvenc`; VP9 copy/transcode: `libvpx-vp9`. |
| `encoding.audio_encoder` | `core.EncodingSettings.AudioEncoder` | `aac` по умолчанию; явные `libopus`, `flac`, `libmp3lame`, `ac3` с соответствующим codec/profile. |
| `encoding.rate_control` | `core.EncodingSettings.RateControl` | CBR/VBR по ограничениям кодировщика; SVT/x264/x265 также CQP/CRF, H.264/HEVC NVENC — CQP, AOM/VP9 — CRF. Диапазон качества и применимость отдельны для каждого кодировщика. |
| `encoding.nvenc.preset` | `core.NVENCSettings` | `p1`…`p7`. |
| `encoding.nvenc.tuning` | Тот же тип | `hq`, `ll`, `ull`; `lossless` только с P4/high444/4448/CQP0/B0/CAVLC и без AQ/lookahead/multipass/custom overrides. |
| `encoding.nvenc.multipass` | Тот же тип | `disabled`, `qres`, `fullres`. |
| `encoding.svt_av1.tuning` | `core.SVTAV1Settings` | `psnr`, `vq`; CBR/CQP допускают только psnr. CRF также допускает `ssim`, `ms_ssim`, `vmaf` со штатным tune 2/4/5. |

Кроме level/tier и разрешённых пустых custom-строк пустые значения недопустимы. Необязательные настройки encoding получают [единые defaults](api-contracts.md#полный-профиль-кодирования) при материализации входа; null внутри блока запрещён. Неприменимый блок запрещён даже пустым. Сохранённый профиль полон, без старого нулевого рецепта.

Таблица описывает единую матрицу, а не свободный выбор любых сочетаний. [SDR-матрица](api-contracts.md#sdr-форматы-цвет-и-кодировщики) ограничивает кодировщик/профиль/пиксели/глубина, RC, level/tier и преобразования. Одного появления строки в enum недостаточно для приёма профиля.

[HEVC/VP9/HDR-матрица](api-contracts.md#hevc-vp9-и-hdr) дополняет SDR-пары. HEVC/VP9 SDR-цели имеют BT.709, HDR — 10 бит, BT.2020, PQ/HLG и BT.2020nc. Profile0/profile2 соответствуют native 0/2; числовые строки не являются псевдонимами. Фиксированные рецепты кодировщиков требуют пустого video_custom; nvenc применим только к h264_nvenc, svt_av1 — к libsvtav1. Неприменимый объект, включая {}, отклоняется. Пользовательских MDCV/CLL-полей нет.

[Аудиоматрица](api-contracts.md#opus-flac-mp3-и-ac-3) определяет 48 kHz, 2/stereo, собственные профиль и кодировщик, пустой audio_custom и target: Opus/MP3 — 128000, AC-3 — 192000, FLAC — 0 как неприменимое значение. FLAC требует явных video/аудио transcode; остальные допускают copy при фактической совместимости. Новые кодеки не выбираются автоматически по одному имени codec.

`encoding.video_custom` и `encoding.audio_custom` не являются одним перечислением. Их грамматика — `name=value name=value`: NVENC `nonref_p=0|1`, `strict_gop=0|1`; SVT `lp=0..6`; AAC `aac_coder=twoloop|fast`. Пустая строка допустима; действуют прежние пределы, нормализация, запрет дубликатов и конфликтов. Неизвестное имя или недопустимое значение отклоняется.

Новые x264/AOM используют фиксированный рецепт и допускают только пустой `video_custom`, без собственных блоков настройки. NVENC CQP/lossless также требует пустого video custom. Это не разрешает передавать внутренние параметры FFmpeg под произвольным именем.

Недопустимые значение, регистр, сочетание, пустота, `null`, JSON-тип, UTF-8 и Base64-объект дают безопасный `422`. Ответ не повторяет присланные строки. Наличие блока кодировщика не создаёт альтернативного пути обхода общей проверки.

## Фактически проверенный формат варианта

`GET /fallback-assets/{id}/variants` возвращает `items[].format` отдельно от `items[].profile.format`. Это наблюдение `core.MediaFormat`, а не настройки. Объект присутствует также до проверки и у FAILED; его числа могут быть нулевыми, а строки пустыми. Прежнее измерение FAILED сохраняется.

Каждый набор ниже дополнительно содержит `""` и `UNKNOWN`. Пустота означает отсутствие распознанного значения; UNKNOWN — непустое внутреннее обозначение вне публичного каталога. Нераспознаваемые строки и невалидный UTF-8 не возвращаются клиенту. Числа и FPS сохраняются точно. Публичная проекция не переписывает исходный `VerifiedFormat` и сама по себе не аннулирует файл.

| Поле относительно format | Распознаваемые непустые обозначения |
| --- | --- |
| `video.codec`, `audio.codec` | `av1`, `h264`, `hevc`, `vp9`; соответственно `aac`, `opus`, `flac`, `mp3`, `ac3`. |
| `video.codec_profile` | H.264: `baseline`, `constrained-baseline`, `main`, `extended`, `high`, `high10`, `high422`, `high444`; AV1: `main`, `high`, `professional`; HEVC: `main`, `main10`; VP9: `profile0`, `profile1`, `profile2`, `profile3`. |
| `video.level` | H.264: все 256 строк вида `n/10.n%10` для целого n=0…255, которые формирует действующий парсер, и прежние алиасы `1`, `1b`, `1.0b`, `2`, `3`, `4`, `5`. AV1: `2.0`…`7.3` с последней цифрой 0…3, `unspecified`. HEVC: `1.0`, `2.0`, `2.1`, `3.0`, `3.1`, `4.0`, `4.1`, `5.0`, `5.1`, `5.2`, `6.0`, `6.1`, `6.2`, `6.3`, `7.0`, `7.1`, `7.2`, `8.5`. VP9: тот же набор от `1.0` до `6.2` без `6.3` и выше, а также `1.1` и `unspecified` для record Undefined. |
| `video.tier` | AV1/HEVC: `main`, `high`; для H.264/VP9 непустая категория не распознаётся. |
| `video.pixel_format` | `gray`, `yuv420p`, `yuv422p`, `yuv444p`; также каждая база с суффиксом `9le`…`16le`. |
| `video.color.range` | `limited`, `full`, `tv`, `pc`, `unspecified`. |
| `video.color.primaries` | `bt709`, `unspecified`, `bt470m`, `bt470bg`, `smpte170m`, `smpte240m`, `film`, `bt2020`. |
| `video.color.transfer` | `bt709`, `unspecified`, `gamma22`, `gamma28`, `smpte170m`, `smpte240m`, `linear`, `iec61966-2-1`, `bt2020-10`, `bt2020-12`, `smpte2084`, `arib-std-b67`. При разборе нового результата transfer-псевдонимы ffprobe `bt470m/bt470bg` приводятся к `gamma22/gamma28`; сохранённая история этим не переписывается. |
| `video.color.matrix` | `gbr`, `bt709`, `unspecified`, `fcc`, `bt470bg`, `smpte170m`, `smpte240m`, `bt2020nc`, `bt2020c`. |
| `audio.codec_profile`, `audio.channel_layout` | По фактическому кодеку: AAC `LC`, Opus `celt`, FLAC `s16`, MP3 `mpeg1-layer3`, AC-3 `ac3`; профиль другого кодека проецируется в `UNKNOWN`. Раскладки: `mono`, `stereo`, `2.1`, `3.0`, `4.0`, `5.0`, `5.1`, `7.1`, `7.1(wide-back)`. Последняя распознаётся как несовместимая с desired `7.1`. |

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
| `M.format.video_codec`, `audio_codec` | `core.ObservedFormat`, `core.Codec` | `""`, `av1`, `h264`, `hevc`, `vp9`, `UNKNOWN`; соответственно `""`, `aac`, `opus`, `flac`, `mp3`, `ac3`, `UNKNOWN`. Пустота — до распознавания; UNKNOWN — общая защитная проекция непустого нераспознанного обозначения. |
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

## Чаты

| Поле | Доменный тип или владелец | Значения |
| --- | --- | --- |
| `provider` источника, аккаунта и подключения | `core.ChatProvider` | `youtube`, `twitch`, `vk_video_live`. |
| `provider` сообщения | `core.ChatMessage` | `youtube`, `twitch`, `vk_video_live`, `test`. Значение `test` обозначает сообщение серверного генератора. |
| `test_provider` сообщения | `core.ChatMessage` | `youtube`, `twitch`, `vk_video_live`. Поле задаёт значок имитируемой площадки при `provider: "test"` и отсутствует у реальных сообщений. |
| `authorization_state` аккаунта | API | `CONFIGURED` подтверждает сохранённую конфигурацию. |
| `state` попытки авторизации | `core.ChatAuthorizationState` | `PENDING`, `SUCCEEDED`, `FAILED`, `CANCELLED`, `EXPIRED`. |
| `code` попытки авторизации | API и `core.ChatReason` | Безопасная причина из каталога чатов или `credential_commit_unknown` для неподтверждённой записи. Пустой код опускается. |
| `state` статуса источника | `core.ChatSourceState` | `DISABLED`, `WAITING_OUTPUT`, `DISCOVERING`, `WAITING_CHAT`, `CONNECTING`, `CONNECTED`, `RETRYING`, `AUTH_REQUIRED`, `FAILED`, `STOPPED`. |
| `reason` статуса и события чата | `core.ChatReason` | `""`, `no_broadcast`, `selection_required`, `chat_unavailable`, `chat_ended`, `chat_disabled`, `credentials_required`, `network`, `quota`, `configuration`, `limit`, `upstream_gap`. |
| `kind` сообщения, `parts[].kind` | `core.ChatMessageKind`, `core.ChatPartKind` | Для сообщения `message`, `supported_system_event`. Для фрагмента `text`, `emote`. |
| `kind` события SSE чата | `core.ChatEventKind` | `chat.message`, `chat.source_status`, `chat.gap`, `chat.cleared`, `resync_required`, `chat.session_closed`. |

Идентификаторы, имена, текст сообщений, cursor, заголовки эфиров и штатные публичные параметры подтверждения Twitch остаются строками. Необязательные URL визуальных элементов проверяются на схему HTTPS. Токены и исходная диагностика платформы не попадают в эти представления.

## Возможности, здоровье и ошибки

| Маршрут/поле | Владелец набора | Значения |
| --- | --- | --- |
| `GET /system/capabilities`: каждый компонент `.status` | API capability provider | `available`, `unavailable`, `unverified`. |
| Те же компоненты `.reason` | Тот же поставщик, проверка HTTP-границы | `not_configured`, `executable_unavailable`, `executable_resolved`, `hardware_not_verified`, `unsupported_platform`. Неизвестный результат поставщика не сериализуется как допустимая возможность. |
| `GET /health/live`, `/health/ready`: `status` | API | `live`, `ready`; неуспешная readiness возвращает Problem Details с 503. |
| Те же успешные ответы: `reason` | API | `""`. |
| `failure.code` Session и квитанций | Сохранённая типизированная причина и безопасная HTTP-проекция | Предметные коды из [каталога ошибок](api-error-contracts.md); для старого результата без причины — `session_failed`, `session_interrupted`, `command_failed`, `command_interrupted`. При отсутствии failure весь объект null. |
| `failure.code` подготовки и наблюдений | Безопасная HTTP-проекция | `preparation_failed`, `preparation_interrupted`, `observation_failed`. Исходная диагностическая строка не превращается в новый код. |
| Problem Details `.code` любого маршрута | Каталог публичных ошибок API | Подготовленные коды разбора, состояния, доступности и предметных причин из [каталога ошибок](api-error-contracts.md); неизвестный внутренний отказ — `internal_error`. |
| Ошибка полного профиля `.code` | `core.ProfileError.Code()` и безопасная HTTP-проекция | `invalid_profile`, `invalid_custom`, `duplicate_custom`, `custom_conflict`, `unknown_custom`. |

`message`, `title`, `detail`, `action`, `field`, `type` и `instance` ошибок сохраняют своё назначение пояснения, пути либо URI. `detail` и `failure.message` объясняют причину, отдельное необязательное `action` подсказывает достоверно известный следующий шаг. `field` содержит только проверенное публичное имя, а не произвольный присланный ключ. Необязательный `context` состоит из известных строковых ID фиксированных видов; он не является словарём произвольной диагностики. Внешние ошибки и отклонённые значения в ответ не переносятся. `name`, ID, публикационные ключи, stream keys, endpoint, platform, cursor и даты также не являются enum.

## Граница формата хранилища

Сохранённые профили используют единственную полную форму. Маркер storage_format=5, история Goose и набор таблиц проверяются до потенциально пишущих вызовов Goose и Recover. Формат рассчитан на новую установку без переноса прежних БД. Отсутствие маркера не означает пустую БД; прежний формат, неизвестная или неполная схема и несогласованные метаданные отклоняются через ErrIncompatibleFormat.

Единственный скрипт `migrations/sqlite/00001_initial.sql` создаёт схему, маркер и ограничения одной транзакцией со своей записью Goose 1. Создание Goose 0 предшествует ей отдельной транзакцией; весь Open не является атомарным. MetadataVersion=1 оболочек, receipts/cursors и физический контракт генератора имеют самостоятельное назначение.

Правила инициализации, повторного открытия и проверки схемы описаны в [контракте хранения](domain-storage-contracts.md#sqlite-и-миграции).
