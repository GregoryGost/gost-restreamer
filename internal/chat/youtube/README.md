# Протокол чтения чата YouTube

`stream_list.proto` воспроизводит [образец Google от 12 ноября 2026 года](https://developers.google.com/youtube/data/live/streaming-live-chat#stream_listproto). SHA-256 извлечённого образца до локальных правок — `1e0588932437ecd6b4c99b3da9a2fa1c9351db3665be22cb28dfe31a62672e98`. Применяется Apache License 2.0 из файла [LICENSE](LICENSE). В исходник добавлены сведения о происхождении и импорт `google/protobuf/duration.proto` для уже объявленного поля `gift_duration`. Имена, номера и типы полей сохраняются.

Для обновления `stream_list.protoset` из локального исходника используется `protoc 36.2` со стандартными схемами из того же архива. Укажите каталог распакованного архива в переменной `GOST_PROTOC_DIR`. Команда запускается из каталога с исходником. [Релиз protoc 36.2](https://github.com/protocolbuffers/protobuf/releases/tag/v36.2)

```sh
"$GOST_PROTOC_DIR/bin/protoc" --proto_path=. --proto_path="$GOST_PROTOC_DIR/include" \
  --include_imports --descriptor_set_out=stream_list.protoset stream_list.proto
```

SHA-256 дескриптора — `e5c6c33c8d9ca8c9ba1bb6bc92b03bbff8c9b105ffea866933f280b84a920970`. Он включается в Go-бинарник и используется с `dynamicpb`. Обычная сборка и тесты используют готовый дескриптор.
