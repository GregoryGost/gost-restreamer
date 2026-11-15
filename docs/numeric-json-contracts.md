# Числа в REST и SSE

Все значения с числовым назначением передаются JSON-числами. Это относится к запросам, ответам, вложенным снимкам, массивам, значениям словарей и JSON после `data:` в SSE. Число в кавычках не принимается вместо числа. Ограничения внешнего языка или клиента не уменьшают диапазон API.

Целые описываются в схемах как `integer`, вещественные величины — как `number`. Разбор, проверка и публикация целых на границе JSON сохраняют точность без промежуточного `float64`. Например, `9007199254740993` и `18446744073709551615` остаются разными точными целыми. Настоящие скорости и дробные показатели используют существующий `float64`.

Математически целые формы `1`, `1.0`, `1e0` и `100e-2` имеют одно целое значение. Ненулевая дробная часть, переполнение, неправильный JSON-тип и отрицательная запись для беззнакового поля отклоняются. Минус при нормализации не удаляется; беззнаковые поля не принимают также `-0`. Для знакового целого `-0` имеет значение 0. Большая экспонента не служит причиной округлять число, разворачивать огромную степень десяти или ограничивать допустимые значения диапазоном JavaScript.

Точные границы основных типов:

| Обозначение | Диапазон |
| --- | --- |
| `U64` | `0…18446744073709551615` (`uint64`, в API также `Decimal`) |
| `I64` | `-9223372036854775808…9223372036854775807` (`int64`) |
| `U32` | `0…4294967295` (`uint32`) |
| `U16` | `0…65535` (`uint16`) |
| `U8` | `0…255` (`uint8`) |
| `D` | `0…9223372036854775807` наносекунд (`time.Duration`) |
| `F` | Конечный неотрицательный `float64`; JSON не допускает `NaN` и бесконечности. |

Физический диапазон не отменяет ограничений конкретного поля, состояния и кодировщика. Например, watchdog положителен, GPU utilization не превышает 100, а расширенный профиль NVENC ограничивает битрейт сильнее `U64`. У прежнего профиля без `encoding` сохраняются его собственные допустимые диапазоны.

## Присутствие, null и ноль

В таблицах ниже поле без пометки `nullable` присутствует, если присутствует содержащий его объект; его значение не заменяется `null`. Nullable-измерение возвращается как явный `null`, когда значения нет, и как число 0, когда измерен ноль. Сам объект наблюдения или `progress` также может быть `null`; дочерних полей в этом случае нет.

В полном входном профиле числовые поля формата, общих параметров кодирования и `encoding.version` обязательны. Отсутствие поля и `null` не означают явный ноль. Необязательные поля `encoding.nvenc` и `encoding.svt_av1` используют прежний механизм присутствия: пропуск выбирает документированное значение по умолчанию, явный 0 или `-1` сохраняется, `null` отклоняется. Неприменимый блок кодировщика отсутствует в ответе. Прежний `encoding: null` остаётся допустимым представлением старого видеорецепта.

`expected_revision` обязателен в соответствующем JSON-запросе, положителен и не допускает `null`. Несовпадение ревизии остаётся конфликтом `409`. Ошибка типа или значения не изменяет сохраняемую конфигурацию. PATCH сохраняет прежние правила пропуска полей, очистки nullable-политики и применения `false`, пустой строки и нуля там, где они допустимы.

## Реестр REST-маршрутов

Пути указаны без общего `/api/v1`. Таблица перечисляет все поверхности; обозначения объектов раскрыты по полям ниже. `items[]` означает применение той же карты к каждому элементу страницы. Идентификаторы, даты и строковые настройки в этой таблице не считаются числовыми полями.

| Маршрут и направление | Числовые пути |
| --- | --- |
| `POST /streams`, запрос | `fallback.watchdog.*_ns`, если передана политика. |
| `PATCH /streams/{id}`, запрос | `expected_revision`; `fallback.watchdog.*_ns`, если политика заменяется. |
| `GET/POST/PATCH /streams…`, ответ ресурса; `GET /streams`, элементы | `revision`; `fallback.watchdog.*_ns`, если политика есть. |
| `POST /transcode-profiles`, запрос | `profile` по полной карте профиля. |
| `PATCH /transcode-profiles/{id}`, запрос | `expected_revision`; полный `profile`, если заменяется. |
| `GET/POST/PATCH /transcode-profiles…`, ответ; `GET` коллекции, элементы | `revision`; полный `profile`. |
| `POST /transcode-groups` и `/destinations`, запрос | Числовых полей нет. |
| `PATCH /transcode-groups/{id}` и `/destinations/{id}`, запрос | `expected_revision`. |
| `GET/POST/PATCH /transcode-groups…` и `/destinations…`, ответ; коллекции, элементы | `revision`. |
| `GET /streams/{id}/destinations`, элементы | `revision` назначения. |
| `GET /streams/{id}/fallback-policy`, ответ | `revision`; `fallback.watchdog.*_ns`, если политика есть. |
| `PUT /streams/{id}/fallback-policy`, запрос/ответ | Запрос: `expected_revision`, сроки переданной политики; ответ: `revision`, сроки сохранённой политики. Явный `fallback: null` очищает политику. |
| `GET /sessions`, `/streams/{id}/sessions`, элементы; `GET /sessions/{id}`, ответ | `revision`; `stream.revision`, `stream.fallback.watchdog.*_ns`; `groups[].group.revision`, `groups[].profile.revision`, `groups[].profile.profile` по карте профиля. |
| `GET /sessions/{id}/outputs`, элементы | `revision`, `destination.revision`, `group.revision`, `profile.revision`; `profile.profile` по карте профиля; `fallback.watchdog.*_ns`. |
| `GET /sessions/{id}/outputs/{outputId}`, ответ | Та же карта сохранённого Output, дополнительно `runtime.revision` и `runtime.metrics` по карте метрик выхода при наличии наблюдения. |
| `POST /streams/{id}/start`, команды `/sessions/{id}/stop`, `/pause`, `/resume`; `POST /sessions/{id}/outputs`; команды выхода `/start`, `/stop`, `/detach` | Запросы содержат строки и массивы идентификаторов, числовых полей нет. Ответ квитанции содержит `revision`. |
| `GET /commands/{commandId}`, ответ | `revision` квитанции. |
| `POST /fallback-assets`, запрос/ответ | В запросе чисел нет; ответ содержит `revision`, `size_bytes`. |
| `GET /fallback-assets`, элементы; `GET /fallback-assets/{id}`, ответ | `revision`, `size_bytes`. |
| `PUT /fallback-assets/{id}/content` | Тело бинарное, не JSON; ответ принятия содержит только идентификаторы. `expected_revision` находится в текстовом query. |
| `POST /fallback-assets/{id}/rebuild`, запрос/ответ | Запрос: `expected_revision`; ответ принятия содержит только идентификаторы. |
| `GET /fallback-assets/{id}/generations/{generationId}`, ответ | `revision`, `size_bytes`. |
| `GET /fallback-assets/{id}/variants`, элементы | `revision`, `size_bytes`, `profile` по карте полного профиля, `format` по карте медиаформата. |
| `DELETE /streams/{id}`, `/transcode-profiles/{id}`, `/transcode-groups/{id}`, `/destinations/{id}`, `/fallback-assets/{id}` | JSON-запроса и успешного JSON-ответа нет; `expected_revision` находится в query. |
| `GET /sessions/{id}/metrics`, ответ | `snapshot` по карте оперативной Session, когда доступен. |
| `GET /events/snapshot`, ответ | `sessions[]` по карте оперативной Session; `gpu` по карте GPU. |
| `GET /system/capabilities`, `/health/live`, `/health/ready`, успешные ответы | Числовых доменных полей нет; времена проверок остаются строками или `null`. |
| Ошибка любого маршрута | `status` в Problem Details — число HTTP-статуса (`int`). |
| `GET /openapi.json`, `/schemas/{schema}.json` и спецификация YAML | Числовые ограничения/перечисления/значения по умолчанию и примеры публикуются согласно типам описываемых полей; точные границы не округляются. |

## Метаданные и политика

| Путь/назначение | Тип Go и JSON | Единицы/диапазон и ноль |
| --- | --- | --- |
| `revision` любого сохраняемого ресурса, вложенной конфигурации или квитанции; `PolicyResult.revision` | `Decimal` / `integer` | Номер ревизии `1…U64.max`; переполнение следующей ревизии даёт конфликт. |
| JSON `expected_revision` | `Decimal` / `integer` | Ожидаемая ревизия `1…U64.max`; отсутствие, `null`, 0 и строка недопустимы. |
| `watchdog.video_timeout_ns` | `Decimal` → `time.Duration` / `integer` | Допустимое отсутствие видео, `1…D.max` нс. |
| `watchdog.audio_timeout_ns` | `Decimal` → `time.Duration` / `integer` | Допустимое отсутствие аудио, `1…D.max` нс. |
| `watchdog.recovery_window_ns` | `Decimal` → `time.Duration` / `integer` | Окно восстановления LIVE, `1…D.max` нс. |
| `watchdog.keyframe_timeout_ns` | `Decimal` → `time.Duration` / `integer` | Ожидание ключевого кадра, `1…D.max` нс. |
| `size_bytes` Asset/Generation/Variant | `Decimal` / `integer` | Размер файла в байтах, `U64`; исходный 0 допустим по состоянию, готовый Variant требует положительный размер. |
| Problem Details `status` | `int` / `integer` | Числовой код HTTP-ошибки; остальные поля ошибки строковые. |

## Полный профиль и медиаформат

Карта ниже применяется к `profile.format`, а также к отдельному `VariantDTO.format`. Вложенные пути профиля в Session/Output раскрыты в таблице маршрутов. Сохранённый профиль возвращает те же числа, независимо от переименования или удаления текущей конфигурации.

| Путь относительно `format` | Go / JSON | Назначение и диапазон |
| --- | --- | --- |
| `video.width` | `uint32` / `integer` | Ширина в пикселях, физически `U32`. |
| `video.height` | `uint32` / `integer` | Высота в пикселях, физически `U32`. |
| `video.frame_rate.numerator` | `uint32` / `integer` | Числитель точной частоты кадров, физически `U32`. |
| `video.frame_rate.denominator` | `uint32` / `integer` | Знаменатель исходной дроби, физически `U32`; дробь не сокращается в публичном представлении. |
| `video.bit_depth` | `uint8` / `integer` | Глубина компонента в битах, физически `U8`. |
| `audio.sample_rate_hz` | `uint32` / `integer` | Частота дискретизации в герцах, физически `U32`. |
| `audio.channels` | `uint16` / `integer` | Число аудиоканалов, физически `U16`. |

При вводе профиля все эти значения положительны. Для ещё не готового варианта `VerifiedFormat` может содержать нули: готовность и проверка формата остаются отдельными условиями. Поэтому общая схема медиаформата не объявляет все ответы строго положительными. Нельзя переносить ограничения расширенного NVENC/SVT-профиля на прежние профили или на ещё не проверенный формат.

| Путь относительно `profile` | Go / JSON | Назначение и диапазон |
| --- | --- | --- |
| `video_encoding.target_bitrate_bps` | `Decimal` / `integer` | Целевой видеобитрейт, бит/с, физически `U64`. |
| `video_encoding.max_bitrate_bps` | `Decimal` / `integer` | Максимальный видеобитрейт, бит/с, физически `U64`; 0 допустим для SVT VBR. |
| `video_encoding.buffer_bits` | `Decimal` / `integer` | Буфер управления битрейтом, биты, физически `U64`; 0 допустим для SVT VBR. |
| `video_encoding.gop_frames` | `uint32` / `integer` | Длина GOP в кадрах, положительный `U32`; дополнительные границы зависят от версии/кодировщика. |
| `video_encoding.max_b_frames` | `uint32` / `integer` | Максимальное число B-кадров, `U32`; 0 допустим. |
| `audio_encoding.bitrate_bps` | `Decimal` / `integer` | Битрейт AAC, бит/с, положительный `U64` с применимыми ограничениями профиля. |
| `encoding.version` | `int` / `integer` | Версия блока кодировщиков, 2. |
| `encoding.nvenc.lookahead_frames` | `Field[int]` на входе, `int` в ответе / `integer` | 0…31 кадров; положительный lookahead вместе с B-кадрами ≤31; по умолчанию 0. |
| `encoding.nvenc.aq_strength` | `Field[int]` / `int` / `integer` | Интенсивность AQ 1…15, без spatial AQ только 8; по умолчанию 8. |
| `encoding.svt_av1.preset` | `Field[int]` / `int` / `integer` | Предустановка 0…13, по умолчанию 8; явный 0 сохраняется. |
| `encoding.svt_av1.aq_mode` | `Field[int]` / `int` / `integer` | Поддержанный режим AQ, 2; по умолчанию 2. |
| `encoding.svt_av1.lookahead_frames` | `Field[int]` / `int` / `integer` | CBR: 0; VBR: −1 либо 6…120 кадров. Defaults: CBR 0, VBR −1. |

Прежний профиль без блока `encoding` сохраняет положительные `target`, `max`, `buffer`, AAC bitrate и GOP, соотношение `target ≤ max` и полный применимый диапазон `uint64`. Расширенный профиль сохраняет отдельные условия:

- H.264/NVENC: `1 ≤ target ≤ max ≤ 4294967295`, `buffer_bits` 1…2147483647; CBR требует равных битрейтов. Закрытый GOP 1…2147483647, B-кадры 0…4 и меньше GOP; baseline требует 0. Размеры чётные, не больше 65534.
- AV1/SVT: target 1…100000000; VBR требует `max = 0`, `buffer = 0`; CBR — `max = target` и буфер, точно соответствующий целым 20…10000 мс. GOP 2…2147483647, B-кадры 0; ширина 64…16384, высота 64…8704, FPS ≤240.
- Расширенный формат использует прежние yuv420p/8-bit SDR и проверки представимости FPS. AAC LC mono/stereo использует поддержанный набор частот и битрейт не выше `6 × sample_rate_hz × channels`. Полные зависимости полей описаны в [контракте API](api-contracts.md#полный-профиль-кодирования).

## Оперативная Session и общие метрики

Оперативная Session содержит следующие числовые поддеревья:

| Путь относительно runtime Session | Содержимое |
| --- | --- |
| `observation.revision` | Ревизия наблюдения, `Decimal` / `integer`, `U64`, включая 0 до первого значения. |
| `input.metrics.media` | Общие метрики медиа по таблице ниже. |
| `input.metrics.source_epoch` | `Decimal` / `integer`, `U64`, идентичность сегмента LIVE; 0 не подменяется отсутствием. |
| `groups[].revision` | Ревизия наблюдения группы, `Decimal` / `integer`, `U64`. |
| `groups[].source_revision` | Ревизия выбора источника, `Decimal` / `integer`, `U64`, 0 до первого выбора. |
| `groups[].metrics.live_input`, `.fallback_input`, `.output` | Три самостоятельных набора общих метрик медиа. |
| `outputs[].revision`, `outputs[].metrics` | Ревизия наблюдения и метрики выхода. |
| `ffmpeg[].revision`, `ffmpeg[].metrics` | Ревизия наблюдения и метрики работника FFmpeg. |

Эта же карта используется в `SessionMetricsDTO.snapshot`, `AnchorDTO.sessions[]` и вложенном текущем Output. Отсутствующее поддерево наблюдения остаётся `null`, без выдуманных нулевых метрик.

| Путь относительно общих `MediaMetricsDTO` | Go / JSON | Единицы/диапазон |
| --- | --- | --- |
| `video_frames` | `Decimal` / `integer` | Число подтверждённых видеоединиц, `U64`. |
| `payload_bytes` | `Decimal` / `integer` | Полезные медиабайты, `U64`. |
| `written_bytes` | `Decimal` / `integer` | Подтверждённые локальным транспортом байты, `U64`. |
| `audio_units` | `Decimal` / `integer` | Число аудиоединиц доступа, `U64`. |
| `rates.fps.value` | `*float64` / nullable `number` | Измеренные кадры/с, `F`; недоступное значение `null`. |
| `rates.payload_bitrate.value` | `*float64` / nullable `number` | Полезный битрейт, бит/с, `F`. |
| `rates.written_bitrate.value` | `*float64` / nullable `number` | Битрейт реально записанных байтов, бит/с, `F`. |
| `rates.fps.duration_ns` | `Decimal` из `time.Duration` / `integer` | Фактическое окно измерения, `D` нс. |
| `rates.payload_bitrate.duration_ns` | `Decimal` из `time.Duration` / `integer` | Собственное фактическое окно, `D` нс. |
| `rates.written_bitrate.duration_ns` | `Decimal` из `time.Duration` / `integer` | Собственное фактическое окно, `D` нс. |
| `format.width`, `format.height` | `uint32` / `integer` | Распознанные пиксели, `U32`; 0 до распознавания. |
| `format.sample_rate_hz` | `uint32` / `integer` | Распознанные герцы, `U32`; 0 до распознавания. |
| `format.channels` | `uint16` / `integer` | Распознанное число каналов, `U16`; 0 до распознавания. |

Каждый счётчик может быть измеренным нулём. Насыщение сохраняет `U64.max` и соответствующий признак, а не сворачивает число. Доступная скорость требует положительного окна; при недоступности/устаревании её `value` равно `null`, а известная длительность окна может сохраняться. Семантика границ измерения описана в [контракте мониторинга](monitoring-contracts.md).

## Метрики выхода

`OutputMetricsDTO.media` использует полную карту общих метрик. Все значения трёх словарей имеют Go-тип `Decimal`, JSON-тип `integer`, диапазон `U64`; ключи словарей остаются строками. Значение 0 явно присутствует и не обозначает отсутствие словаря.

| Путь относительно `metrics` | Назначение/единицы |
| --- | --- |
| `counters.queued_packets` | Текущее число пакетов основной очереди. |
| `counters.packet_bytes` | Логический вес пакетов и ожидающего аудио с конфигурациями, байты. |
| `counters.access_units` | Число полностью отправленных медиасообщений. |
| `counters.pending_packets` | Число аудиопакетов до выбора ключевого кадра. |
| `counters.generation` | Поколение сброса подписки. |
| `counters.in_flight_packet_bytes` | Логический вес пакета на отправке, байты. |
| `counters.in_flight_config_bytes` | Вес его конфигураций, байты. |
| `counters.wire_buffer_bytes` | Логический резерв RTMP-транспорта, байты. |
| `counters.config_bytes` | Вес текущих конфигураций подписки, байты. |
| `counters.config_byte_capacity` | Предел веса конфигураций, байты. |
| `counters.attempts` | Число попыток подключения. |
| `counters.connections` | Число установленных соединений. |
| `counters.reconnects` | Число повторных подключений в текущем запуске выхода. |
| `counters.queue_packets` | Текущее число пакетов очереди. |
| `counters.queue_capacity` | Предел числа пакетов. |
| `counters.queue_bytes` | Текущее логическое заполнение очереди, байты. |
| `counters.queue_byte_capacity` | Предел заполнения, байты. |
| `queue_drops.overflow` | Потери при переполнении очереди. |
| `queue_drops.oversized` | Слишком большие пакеты. |
| `queue_drops.config_rejected` | Отклонённые конфигурации. |
| `queue_drops.awaiting_keyframe` | Пакеты до ключевого кадра. |
| `queue_drops.audio_before_cut` | Аудиопакеты до границы переключения. |
| `queue_drops.source_reset` | Потери при сбросе источника. |
| `queue_drops.config_reset` | Потери при сбросе конфигурации. |
| `queue_drops.closed` | Потери при закрытии очереди. |
| `delivery_drops.stale` | Устаревшие выданные пакеты. |
| `delivery_drops.transport` | Отказы транспортной доставки. |
| `delivery_drops.cancelled` | Отменённая доставка. |
| `delivery_drops.terminal` | Потери при окончательном завершении выхода. |
| `ack_ahead_events_total` | Число новых достоверно измеренных опережающих ACK. |
| `ack_ahead_bytes_last` | Последний измеренный размер опережения, RTMP-байты. |
| `ack_ahead_bytes_max` | Максимальный размер опережения текущего запуска, RTMP-байты. |

Потери выражаются числом соответствующих локальных событий/отброшенных единиц. Сохраняются ограничения `queue_packets ≤ queue_capacity` и `queue_bytes ≤ queue_byte_capacity`. ACK-поля находятся непосредственно в `metrics`, не дублируются в `counters` и сохраняют прежнюю семантику reconnect/насыщения. Времена `ack_last_anomaly_at`, `last_write`, `last_media` — строки UTC либо `null`.

## FFmpeg

| Путь относительно метрик FFmpeg | Go / JSON | Назначение, единицы и диапазон |
| --- | --- | --- |
| `progress_losses` | `Decimal` / `integer` | Потерянные целые отчёты progress, `U64`. |
| `diagnostic_losses` | `Decimal` / `integer` | Потерянные диагностические записи, `U64`. |
| `progress.frame` | `*Decimal` / nullable `integer` | Обработанные кадры, `U64`. |
| `progress.total_size_bytes` | `*Decimal` / nullable `integer` | Объём выходных данных, байты `U64`. |
| `progress.dup_frames` | `*Decimal` / nullable `integer` | Дублированные кадры, `U64`. |
| `progress.drop_frames` | `*Decimal` / nullable `integer` | Отброшенные кадры, `U64`. |
| `progress.fps` | `*float64` / nullable `number` | Частота FFmpeg, кадры/с, `F`. |
| `progress.bitrate_bps` | `*float64` / nullable `number` | Битрейт FFmpeg, бит/с, `F`. |
| `progress.speed` | `*float64` / nullable `number` | Множитель скорости обработки, `F`. |
| `progress.out_time_us` | `*int64` / nullable `integer` | Выходное медиавремя, микросекунды `I64`; отрицательные значения сохраняются. |

Весь `progress` равен `null`, пока отсутствует пригодный текущий отчёт. В пригодном отчёте каждое отсутствующее числовое поле остаётся `null`, присутствующий ноль — числом 0. Неизвестное время не подменяется нулём, а микросекунды не превращаются в миллисекунды. Внутренний текстовый протокол CLI FFmpeg остаётся самостоятельным форматом.

## GPU

Каждая строка таблицы описывает отдельный `GPUMetricDTO`. Его `value` имеет тип `*Decimal`, JSON nullable `integer`; `sampling_period_us` — `Decimal`, обязательный `integer` в `U64`, микросекунды. Не заданный аппаратный период может быть 0 и не подменяется периодом опроса. Все времена — строки UTC либо `null`.

| Путь относительно `gpu.devices[]` | Единицы и диапазон `value` |
| --- | --- |
| `utilization.value` | Загрузка GPU, 0…100%. |
| `memory_utilization.value` | Активность памяти, 0…100%. |
| `encoder_utilization.value` | Загрузка NVENC, 0…100%. |
| `decoder_utilization.value` | Загрузка NVDEC, 0…100%. |
| `memory_used_bytes.value` | Занятая память, байты `U64`. |
| `memory_total_bytes.value` | Полная память, байты `U64`. |
| `memory_free_bytes.value` | Свободная память, байты `U64`. |

`sampling_period_us` имеется у каждого из семи перечисленных объектов. `gpu.stale_after_ns` имеет тип `Decimal` из `time.Duration`, JSON `integer`, диапазон `1…D.max` наносекунд. При недоступности или устаревании отдельного измерения его `value = null`; измеренный ноль остаётся числом. Отсутствующий системный снимок — `gpu: null`.

## JSON-данные SSE

События `state`, `metrics`, `input`, `gpu`, `removed` и `attempt` используют `RuntimeEventDTO`. В каждом payload числовые поля верхнего уровня — `observation.revision` и `source_revision`, оба `Decimal` / `integer`, `U64`, включая допустимый 0 для неприменимого/ещё не наблюдённого значения. Вложенные `input`, `group`, `output`, `ffmpeg`, `gpu` используют те же таблицы и единицы, что REST:

- `input.metrics.media` и `input.metrics.source_epoch`;
- `group.live_input`, `group.fallback_input`, `group.output`;
- `output.media`, три словаря и три ACK-поля;
- `ffmpeg.progress_losses`, `diagnostic_losses`, nullable `progress`;
- `gpu.stale_after_ns` и все показатели `devices[]`.

Неприменимое поддерево остаётся `null`. Управляющее событие `resync_required` содержит строки `reason` и `snapshot_url` и не содержит чисел. Комментарии `: connected` и heartbeat, а также служебные `event:`/`id:` не являются JSON-данными. Описание события в OpenAPI связывает JSON payload с опубликованной схемой; транспорт всего ответа остаётся `text/event-stream`.

## Строки, схемы и сохранённые данные

Идентификаторы, составные `instance_id`, ключи, URL, непрозрачные cursor, RFC3339-времена и обозначения кодеков/уровней остаются строками даже при сходстве с числом. Строки custom-параметров, например `lp=2`, сохраняют свою грамматику. Допустимые строковые настройки и отдельный каталог измеренного формата описаны в [реестре строковых полей](string-json-contracts.md); публичная Base64-обёртка профиля не используется.

Query `limit` и `expected_revision`, path/header и `Last-Event-ID` передаются текстом по правилам HTTP. JSON внутри непрозрачного SSE cursor v1 сохраняет прежнее строковое `sequence`, а наружу весь cursor выдаётся строкой. Содержимое курсора не становится самостоятельным числовым полем API.

В OpenAPI/JSON Schema числовыми остаются `minimum`, `maximum`, исключающие границы, `multipleOf`, размеры строк/массивов/объектов и соответствующие numeric `enum`, `const`, `default`, `examples`. Они сохраняют точность при выдаче JSON, YAML и отдельных схем. Тип `integer` для полного unsigned32/64 не сопровождается противоречащим signed форматом, который сокращает диапазон. Nullable и более узкие применимые ограничения описываются отдельно от общей физической ширины.

Публичное представление отделено от хранения. SQL `uint64` остаётся точным десятичным TEXT; числовой JSON сам по себе не меняет профильные кодеки v1/v2/v3, версии метаданных, канонические байты/ключи, самостоятельные снимки или идентичность команд. Отдельная согласованная миграция закрытых строк описана в реестре строковых полей; она сохраняет числовую точность и независимые числовые настройки.
