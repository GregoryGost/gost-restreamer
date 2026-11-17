# Синтетические исходники для проверки подготовки аудио

Файлы созданы локальным FFmpeg 7.1.5 (Homebrew `7.1.5_3`) из чёрного изображения `color` и цифровой тишины `anullsrc`. Пользовательских записей и сетевых источников нет. Это свежий ненормализованный выход кодировщика: обычные Go-тесты читают настоящие fMP4, заголовки и сжатые кадры, выполняют нормализацию, проверку формата и периода. Запуск FFmpeg в этих тестах не нужен; подставляется только ответ внешнего ffprobe.

В подставленном ответе параметры дорожек измерены, а `Format.Duration` задана синтетически по целевому полному периоду для общей проверки положительности и бюджета. Это не измеренная длительность исходного контейнера: она равна 0,102500/2,400000/0,623021/0,805333 с для Opus/FLAC/MP3/AC-3. Точный период готового файла проверяется независимо по фактическим AU и временным меткам.

Видео всех файлов: H.264 High level1.1, 128×128, 30FPS, yuv420p, ограниченный BT.709, без B-кадров. Аудио: 48kHz, stereo. Число аудиокадров до и после нормализации указано раздельно; целый период относится к подготовленному результату.

| Файл `.mp4` | Байт | Видеокадров | Аудиокадров до → после | Сэмплов в полной AU | Период, с |
| --- | ---: | ---: | ---: | ---: | ---: |
| `opus-celt-silent` | 7792 | 3 | 41 → 40 | 120 | 0,1 |
| `flac-s16-silent` | 15573 | 72 | 25 → 25 | 4608 | 2,4 |
| `mp3-mpeg1-silent` | 17524 | 18 | 26 → 25 | 1152 | 0,6 |
| `ac3-stereo-silent` | 27274 | 24 | 25 → 25 | 1536 | 0,8 |

Opus содержит начальный `preSkip=120` и лишнюю первую AU; подготовка потребляет её один раз и сохраняет Head0. MP3 содержит 26 полных сжатых AU, отрицательный первый DTS −1105 и контейнерную длительность последнего образца1105; нормализатор собственной тишины удаляет первую полную AU и создаёт25кадров на сетке1152. У AC-3 первый DTS −256; исходный `anullsrc` после кодирования ещё содержит шум декодерного dithering. Только доказав нулевые коэффициенты всех шести блоков, рабочий нормализатор очищает соответствующие флаги и пересчитывает обе CRC. Исходный AC-3-файл не служит оракулом нулевого PCM.

При создании независимое декодирование исходных Opus, FLAC и MP3 в `pcm_f32le` дало точные цифровые нули. Отдельный FFmpeg 7.1.5 декодировал нормализованные результаты с `-stream_loop 2`: соответственно 14400, 345600, 86400 и 115200 сэмплов на канал, ровно три периода каждого файла; все выходные байты равны нулю. Обычный Go-тест фиксирует измеренные сжатые кадры контрольными суммами и проверяет формат и время. Простой разбор Go не выдаётся за независимое декодирование PCM.

## Воспроизведение

Ниже приведены полные команды создания исходных файлов. Они пишут только указанные новые файлы в текущем каталоге; перед запуском следует выбрать отдельный временный каталог. Точные байты могут зависеть от сборок библиотек кодировщиков, поэтому контрольные суммы фиксируют использованные тестовые входы, а не обещают одинаковый результат произвольной сборки FFmpeg.

```sh
ffmpeg -hide_banner -nostdin -v error -xerror -f lavfi -i color=c=black:s=128x128:r=30 -f lavfi -i anullsrc=r=48000:cl=stereo -map 0:v:0 -map 1:a:0 -vf setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709 -c:v libx264 -preset medium -profile:v high -level:v 1.1 -pix_fmt yuv420p -bf:v 0 -g:v 15 -keyint_min:v 15 -sc_threshold:v 0 -b:v 100000 -maxrate:v 200000 -bufsize:v 200000 -color_range:v tv -color_primaries:v bt709 -color_trc:v bt709 -colorspace:v bt709 -ar:a 48000 -ac:a 2 -c:a libopus -b:a 128000 -application:a lowdelay -frame_duration:a 2.5 -vbr:a on -mapping_family:a 0 -t 0.1 -f mp4 -movflags delay_moov+default_base_moof+frag_every_frame -min_frag_duration 1 -use_editlist 1 -movie_timescale 48000 -video_track_timescale 48000 -avoid_negative_ts disabled pipe:1 > opus-celt-silent.mp4
```

```sh
ffmpeg -hide_banner -nostdin -v error -xerror -f lavfi -i color=c=black:s=128x128:r=30 -f lavfi -i anullsrc=r=48000:cl=stereo -map 0:v:0 -map 1:a:0 -vf setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709 -c:v libx264 -preset medium -profile:v high -level:v 1.1 -pix_fmt yuv420p -bf:v 0 -g:v 15 -keyint_min:v 15 -sc_threshold:v 0 -b:v 100000 -maxrate:v 200000 -bufsize:v 200000 -color_range:v tv -color_primaries:v bt709 -color_trc:v bt709 -colorspace:v bt709 -ar:a 48000 -ac:a 2 -c:a flac -sample_fmt:a s16 -strict -2 -t 2.4 -f mp4 -movflags delay_moov+default_base_moof+frag_every_frame -min_frag_duration 1 -use_editlist 1 -movie_timescale 48000 -video_track_timescale 48000 -avoid_negative_ts disabled pipe:1 > flac-s16-silent.mp4
```

```sh
ffmpeg -hide_banner -nostdin -v error -xerror -f lavfi -i color=c=black:s=128x128:r=30 -f lavfi -i anullsrc=r=48000:cl=stereo -map 0:v:0 -map 1:a:0 -vf setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709 -c:v libx264 -preset medium -profile:v high -level:v 1.1 -pix_fmt yuv420p -bf:v 0 -g:v 15 -keyint_min:v 15 -sc_threshold:v 0 -b:v 100000 -maxrate:v 200000 -bufsize:v 200000 -color_range:v tv -color_primaries:v bt709 -color_trc:v bt709 -colorspace:v bt709 -ar:a 48000 -ac:a 2 -c:a libmp3lame -b:a 128000 -reservoir:a 0 -abr:a 0 -t 0.6 -f mp4 -movflags delay_moov+default_base_moof+frag_every_frame -min_frag_duration 1 -use_editlist 1 -movie_timescale 48000 -video_track_timescale 48000 -avoid_negative_ts disabled pipe:1 > mp3-mpeg1-silent.mp4
```

```sh
ffmpeg -hide_banner -nostdin -v error -xerror -f lavfi -i color=c=black:s=128x128:r=30 -f lavfi -i anullsrc=r=48000:cl=stereo -map 0:v:0 -map 1:a:0 -vf setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709 -c:v libx264 -preset medium -profile:v high -level:v 1.1 -pix_fmt yuv420p -bf:v 0 -g:v 15 -keyint_min:v 15 -sc_threshold:v 0 -b:v 100000 -maxrate:v 200000 -bufsize:v 200000 -color_range:v tv -color_primaries:v bt709 -color_trc:v bt709 -colorspace:v bt709 -ar:a 48000 -ac:a 2 -c:a ac3 -b:a 192000 -channel_coupling:a 0 -stereo_rematrixing:a 0 -t 0.8 -f mp4 -movflags delay_moov+default_base_moof+frag_every_frame -min_frag_duration 1 -use_editlist 1 -movie_timescale 48000 -video_track_timescale 48000 -avoid_negative_ts disabled pipe:1 > ac3-stereo-silent.mp4
```

## Контрольные суммы SHA256

```text
74608e9900647f67ae07122c0e9d2f5807ddfdb559da0d602cbdfe5c660fa395  opus-celt-silent.mp4
90c5f7ff176c32ba06d4666cbd4cb07f4cf5fd1e810f869d7f8667c0155158c7  flac-s16-silent.mp4
7c64faba6559ca7b2abcaae99a2d4691fb5da49d2e8f0c04763a962354f3771e  mp3-mpeg1-silent.mp4
cf55203965e7309ca8dabd487bb68531a276aa45be92a38d164b4708510f9e47  ac3-stereo-silent.mp4
```
