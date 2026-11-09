# Подключение управляющего клиента OBS

OBS-плагин разрабатывается отдельно. Он управляет backend через REST/SSE и включает один штатный выход OBS AV1/AAC. Серверные группы самостоятельно формируют выходные профили и отправляют их назначениям.

## Схема API и первичная настройка

Префикс всех маршрутов — `/api/v1`. Управление в локальной сети владельца не требует Authorization или токенов доступа. Структуры запросов и все обязательные поля доступны по адресам:

- `GET /api/v1/openapi.json` и `/api/v1/openapi.yaml`;
- `/api/v1/docs`;
- schemas под `/api/v1/schemas`.

Примеры ниже используют адрес `http://127.0.0.1:8080`. На клиентском компьютере укажите адрес серверной VM. Значения `ASSET_ID`, `STREAM_ID`, `PROFILE_ID`, `GROUP_ID`, `DESTINATION_ID`, `SESSION_ID` и `COMMAND_ID` нужно заменить идентификаторами из ответов сервера; это не фиксированные значения продукта. Примеры ключей синтетические.

Создание именованной заглушки возвращает `201` и её `id` с `revision`:

```sh
curl --fail-with-body -H 'Content-Type: application/json' \
  -d '{"name":"Пауза эфира"}' \
  http://127.0.0.1:8080/api/v1/fallback-assets
```

Создайте Stream с постоянным ключом для OBS и политикой этой заглушки:

```json
{
  "name": "Основной эфир",
  "publication_key": "example-publication-key",
  "fallback": {
    "asset_id": "ASSET_ID",
    "watchdog": {
      "video_timeout_ns": "3000000000",
      "audio_timeout_ns": "3000000000",
      "recovery_window_ns": "1000000000",
      "keyframe_timeout_ns": "5000000000"
    }
  }
}
```

Этот JSON отправляется в `POST /streams`. Числа watchdog — пример настройки, не результат измерения производительности. Ключ передаётся при создании и не возвращается в GET: клиент сохраняет исходное выбранное значение для настройки OBS.

Затем создайте полные профили через `POST /transcode-profiles` с телом `{"name":"…","profile":{…}}` по OpenAPI. Входной сценарий архитектуры использует следующие три профиля:

| Режим | Выходное видео | Общие характеристики |
| --- | --- | --- |
| `copy` | AV1 2560×1440, 60 FPS | SDR BT.709, AAC LC 48 kHz stereo |
| `transcode` | H.264 2560×1440, 60 FPS | SDR BT.709, AAC LC 48 kHz stereo |
| `transcode` | H.264 1920×1080, 60 FPS | SDR BT.709, AAC LC 48 kHz stereo |

Полный профиль включает `format.video`, `format.audio`, `video_encoding`, `audio_encoding`, независимый `live_audio_mode` и блок `encoding` версии 2; битрейты, GOP, профиль кодека, формат пикселей и цвет задаются явно. Новый профиль по умолчанию копирует LIVE-аудио OBS даже при преобразовании видео. Для явного AAC-перекодирования укажите `live_audio_mode: "transcode"` в H.264-группе. Параметры не выбираются автоматически по названию площадки. [Поля, значения по умолчанию и ограничения](api-contracts.md#полный-профиль-кодирования) описаны в контракте API.

### Профили кодирования в Postman

Для каждого примера используйте `POST http://127.0.0.1:8080/api/v1/transcode-profiles`, `Body → raw → JSON`, `Content-Type: application/json`. Это примеры синтаксиса, а не подтверждённые рекомендации битрейта или качества для конкретной площадки. Сервер должен иметь указанные кодировщики; NVIDIA и фактическое воспроизведение проверяются отдельно.

Первый профиль задаёт H.264 1080p60 CBR и показывает полный блок NVENC:

```json
{
  "name": "H264 1080p60 CBR",
  "profile": {
    "mode": "transcode",
    "live_audio_mode": "copy",
    "format": {
      "video": {
        "codec": "h264", "width": 1920, "height": 1080,
        "frame_rate": {"numerator": 60, "denominator": 1},
        "codec_profile": "high", "level": "auto", "tier": "",
        "pixel_format": "yuv420p", "bit_depth": 8,
        "color": {"range": "limited", "primaries": "bt709", "transfer": "bt709", "matrix": "bt709"}
      },
      "audio": {"codec": "aac", "codec_profile": "LC", "sample_rate_hz": 48000, "channels": 2, "channel_layout": "stereo"}
    },
    "video_encoding": {
      "target_bitrate_bps": "6000000", "max_bitrate_bps": "6000000", "buffer_bits": "12000000",
      "gop_frames": 120, "max_b_frames": 2, "closed_gop": true
    },
    "audio_encoding": {"bitrate_bps": "192000"},
    "encoding": {
      "version": 2, "video_encoder": "h264_nvenc", "audio_encoder": "aac",
      "rate_control": "cbr", "rescale_output": true,
      "nvenc": {
        "preset": "p4", "tuning": "hq", "multipass": "disabled", "lookahead_frames": 0,
        "enable_spatial_aq": false, "enable_temporal_aq": false, "aq_strength": 8
      },
      "video_custom": "strict_gop=1", "audio_custom": "aac_coder=twoloop"
    }
  }
}
```

Второй независимый профиль задаёт H.264 1440p60 VBR. Пропущенные новые optional-поля сервер материализует при создании; GET вернёт их полные значения:

```json
{
  "name": "H264 1440p60 VBR",
  "profile": {
    "mode": "transcode",
    "live_audio_mode": "copy",
    "format": {
      "video": {
        "codec": "h264", "width": 2560, "height": 1440,
        "frame_rate": {"numerator": 60, "denominator": 1},
        "codec_profile": "high", "level": "auto", "tier": "",
        "pixel_format": "yuv420p", "bit_depth": 8,
        "color": {"range": "limited", "primaries": "bt709", "transfer": "bt709", "matrix": "bt709"}
      },
      "audio": {"codec": "aac", "codec_profile": "LC", "sample_rate_hz": 48000, "channels": 2, "channel_layout": "stereo"}
    },
    "video_encoding": {
      "target_bitrate_bps": "12000000", "max_bitrate_bps": "16000000", "buffer_bits": "24000000",
      "gop_frames": 120, "max_b_frames": 2, "closed_gop": true
    },
    "audio_encoding": {"bitrate_bps": "192000"},
    "encoding": {
      "version": 2, "video_encoder": "h264_nvenc", "rate_control": "vbr",
      "nvenc": {"preset": "p5", "multipass": "qres", "lookahead_frames": 16, "enable_spatial_aq": true},
      "video_custom": "nonref_p=1 strict_gop=1"
    }
  }
}
```

Третий профиль описывает ожидаемый AV1 1440p60 VBR из OBS. Его параметры нужно согласовать с фактическим выходом OBS до подготовки заглушки. `libsvtav1` и `svt_av1` относятся к серверному кодированию заглушки; основной LIVE пересылается без перекодирования:

```json
{
  "name": "AV1 copy 1440p60 VBR",
  "profile": {
    "mode": "copy",
    "live_audio_mode": "copy",
    "format": {
      "video": {
        "codec": "av1", "width": 2560, "height": 1440,
        "frame_rate": {"numerator": 60, "denominator": 1},
        "codec_profile": "main", "level": "auto", "tier": "main",
        "pixel_format": "yuv420p", "bit_depth": 8,
        "color": {"range": "limited", "primaries": "bt709", "transfer": "bt709", "matrix": "bt709"}
      },
      "audio": {"codec": "aac", "codec_profile": "LC", "sample_rate_hz": 48000, "channels": 2, "channel_layout": "stereo"}
    },
    "video_encoding": {
      "target_bitrate_bps": "18000000", "max_bitrate_bps": "0", "buffer_bits": "0",
      "gop_frames": 120, "max_b_frames": 0, "closed_gop": true
    },
    "audio_encoding": {"bitrate_bps": "192000"},
    "encoding": {
      "version": 2, "video_encoder": "libsvtav1", "audio_encoder": "aac",
      "rate_control": "vbr", "rescale_output": false,
      "svt_av1": {"preset": 8, "tuning": "psnr", "aq_mode": 2, "lookahead_frames": -1},
      "video_custom": "lp=2", "audio_custom": ""
    }
  }
}
```

Для AV1 CBR измените в полном третьем профиле `rate_control` на `cbr`, `max_bitrate_bps` на `"18000000"`, `buffer_bits` на `"36000000"` (2000 мс) и SVT `lookahead_frames` на 0. Ненулевой VBR max/buffer не принимается: библиотека не предоставляет это ограничение в выбранном режиме. Native настройки других кодировщиков OBS нельзя вставить в SVT-блок без сопоставления.

Во всех трёх примерах LIVE AAC сохраняется исходным. Указанные аудиобитрейт и `audio_custom` управляют подготовкой беззвучной заглушки. Для перекодирования звука в одном из H.264-профилей явно замените корневой `live_audio_mode` на `"transcode"`: тогда те же аудиопараметры применятся и к LIVE. При копировании фактический профиль AAC, частота, число и расположение каналов OBS должны совпадать с `format.audio`; неподходящий LIVE оставляет группу на заглушке с причиной в журнале.

Редактирование выполняется через `PATCH /transcode-profiles/PROFILE_ID`: передайте текущий `expected_revision` и **полный** объект `profile`, например скопированный из GET с нужными изменениями. Не отправляйте частичный `profile` только с `rate_control` или `live_audio_mode`. Чтобы включить копирование звука у прежнего профиля, добавьте `live_audio_mode: "copy"` в полный профиль; существующий `encoding: null` можно сохранить, не меняя видеорецепт. Для расширенных видеонастроек явно добавьте блок `encoding` версии 2. У уже явного профиля полная замена требует `live_audio_mode`; имя без `profile` сохраняет прежний режим. Текущий эфир использует свой снимок; после изменения потребуется подготовить заглушки до нового запуска.

Проверка понятного конфликта: добавление `rc=vbr` в `video_custom` первого профиля должно дать `422` с `code`, `field` и подготовленным объяснением. Значение typed-поля не переопределяется, профиль не сохраняется.

### Привязка профилей к назначениям

Создание группы для Stream и профиля:

```json
{"stream_id":"STREAM_ID","profile_id":"PROFILE_ID","name":"Выходная группа"}
```

Отправьте его в `POST /transcode-groups`. Создание назначения через `POST /destinations`:

```json
{
  "stream_id": "STREAM_ID",
  "group_id": "GROUP_ID",
  "name": "Назначение",
  "platform": "custom",
  "endpoint": "rtmps://ingest.example.invalid/live",
  "stream_key": "example-destination-key",
  "enabled": true
}
```

Укажите фактические URL и ключ выбранной площадки. Пример URL намеренно не является рабочим назначением. Несколько назначений одной группы получают один результат обработки. Разные H.264-профили используют независимые группы.

## Подготовка заглушки до эфира

Сначала создаются все группы Streams, использующих заглушку: из них сервер выводит полный набор вариантов. Затем передаётся оригинал. `expected_revision` берётся из последнего GET Asset:

```sh
curl --fail-with-body -X PUT -H 'Content-Type: application/octet-stream' \
  --data-binary @pause.png \
  'http://127.0.0.1:8080/api/v1/fallback-assets/ASSET_ID/content?kind=image&expected_revision=1'
```

Для видео используйте `kind=video`. `202` означает завершение передачи оригинала; подготовка вариантов продолжается в фоне. В ответе находятся `asset_id`, `generation_id` и `Location` точного результата:

```text
GET /api/v1/fallback-assets/ASSET_ID/generations/GENERATION_ID
```

Читайте этот ресурс до `status=ready` либо `status=error`. Состояния `queued` и `preparing` промежуточные. Прежняя рабочая заглушка сохраняется при неудачной замене. После изменения полных профилей потребуется новая подготовка; upload и rebuild выполняются до эфира, включая запрет во время ручной паузы.

Заглушки всегда подготавливаются с AAC-тишиной, в том числе из видео со звуком. После обновления со старого генератора выполните `POST /fallback-assets/ASSET_ID/rebuild` с `{"expected_revision":"ТЕКУЩАЯ_РЕВИЗИЯ"}`. Сохранённый оригинал используется повторно, передавать его заново не нужно. Ответ `202` и результат поколения проверяются так же, как при upload. Старый READY не даёт допуск воспроизведения; прежние снимки сохраняются, новый вариант не подставляется в уже прикреплённый Output автоматически. Ошибка rebuild сохраняет прежний текущий комплект, но его устаревшая версия остаётся непригодной до успешной подготовки.

Подготовленный файл и его повторы полностью беззвучны. Только непосредственно при LIVE → fallback допускается короткий остаток прежнего звука декодера получателя; он не является звуком исходного файла заглушки. Универсальный предел длительности для всех плееров не заявляется.

## Запуск и асинхронные команды

```sh
curl --fail-with-body -H 'Content-Type: application/json' \
  -d '{"request_id":"start-example-1","destination_ids":["DESTINATION_ID"]}' \
  http://127.0.0.1:8080/api/v1/streams/STREAM_ID/start
```

`202 Accepted` подтверждает долговечное принятие команды. Квитанция содержит `id`, `request_id`, `session_id` и `status`; `Location` указывает `GET /api/v1/commands/COMMAND_ID`. Проверяйте результат этим GET независимо от SSE. Промежуточные статусы — `PENDING`, `CLAIMED`, `ISSUED`; терминальные — `SUCCEEDED`, `FAILED`, `INTERRUPTED`.

При сетевом повторе используйте прежний `request_id` для того же намерения. Новое действие получает новый идентификатор. Повтор не должен создавать новый эфир или повторять уже выполненный внешний шаг. При конфликте содержимого запроса сервер возвращает `409`.

После успешного запуска получите `GET /sessions/SESSION_ID` и проверьте `RUNNING`. Готовность Session включает фактическую готовность выбранных локальных групп; сетевое подключение каждого назначения независимо. До OBS сервер уже подаёт заглушку.

Настройка одного штатного выхода OBS:

```text
Server:     rtmp://<адрес-VM>:1935/STREAM_ID
Stream key: выбранный постоянный publication_key Stream
Video:      AV1, 2560×1440, 60 FPS, SDR
Audio:      AAC
```

Публикация OBS сама не создаёт Session. До `RUNNING` и после принятия stop она отклоняется. Новая авторизованная публикация заменяет прежний вход; исправные группы и исходящие соединения сохраняются.

Pause, resume и stop — отдельные асинхронные команды:

```text
POST /api/v1/sessions/SESSION_ID/pause
POST /api/v1/sessions/SESSION_ID/resume
POST /api/v1/sessions/SESSION_ID/stop
```

Тело каждой команды: `{"request_id":"уникальный-идентификатор-действия"}`. Для каждой читайте собственную квитанцию. Pause включает заглушку, продолжая приём OBS; resume разрешает возврат LIVE после стабилизации. Успех команды не является подтверждением изображения у зрителя.

Независимые выходы управляются так:

| Действие | Запрос |
| --- | --- |
| Добавить назначение | `POST /sessions/SESSION_ID/outputs` с `request_id` и `destination_id` |
| Временно остановить выход | `POST /sessions/SESSION_ID/outputs/OUTPUT_ID/stop` с `request_id` |
| Запустить прикреплённый выход снова | Тот же путь с `/start` |
| Отсоединить выход | Тот же путь с `/detach` |

Stop сохраняет возможность запуска с прежним снимком. После detach старый Output повторно не запускается; для нового подключения используется POST коллекции. Команды не пересоздают исправные соседние выходы.

## Метрики, SSE и потеря событий

Текущие измерения: `GET /api/v1/sessions/SESSION_ID/metrics`. История запусков: `GET /api/v1/streams/STREAM_ID/sessions` и `GET /api/v1/sessions/SESSION_ID`. История не содержит старых оперативных счётчиков. У известной завершённой Session метрики недоступны: `availability=unavailable`, `snapshot=null`; неизвестный ID даёт `404`.

Клиентская последовательность подписки:

1. Получить `GET /api/v1/events/snapshot`, отобразить его данные и сохранить `cursor` как непрозрачную строку.
2. Открыть `GET /api/v1/events?cursor=<cursor>` с корректным URL-кодированием.
3. Применять события по порядку и сохранять последнее поле SSE `id`.
4. При обычном переподключении передать последнее `id` в `Last-Event-ID` либо `cursor`.
5. При событии `resync_required` закрыть старую подписку, заново получить snapshot и открыть подписку с новым cursor. Неполный диапазон не применяется как непрерывная история.

Для ручной проверки, где `event_cursor` содержит cursor ответа snapshot:

```sh
curl --fail-with-body http://127.0.0.1:8080/api/v1/events/snapshot
curl -N --get --data-urlencode "cursor=$event_cursor" \
  http://127.0.0.1:8080/api/v1/events
```

Heartbeat и комментарий `: connected` не являются изменением состояния. При передаче header и query одновременно их значения должны совпадать. После перезапуска сервера прежний epoch недействителен: клиент начинает с нового snapshot и отдельно читает сохранённые Session/квитанции.

Все 64-битные значения и наносекундные длительности API — десятичные строки. Не пропускайте их через тип числа с потерей точности. PATCH различает отсутствие, `null`, `false` и пустую строку; изменения и удаления требуют текущего `expected_revision`. Подробный контракт ошибок, nullable и профилей — в [описании HTTP API](api-contracts.md).

Показатели означают серверные наблюдения: фактические локальные записи RTMP, полученные кадры и отброшенные внутри backend пакеты. Они не доказывают воспроизведение на площадке. Недоступные/устаревшие измерения показываются явно; GPU проверяется на целевом сервере. Основной журнал приложения использует режим `full`, а GET/SSE сохраняют отдельный контракт без ключей и Endpoint.
