package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

// chatTestStartInput содержит адрес потока и необязательный интервал тестовой ленты.
type chatTestStartInput struct {
	// StreamID выбирает существующий поток, для которого запускается тестовая лента.
	StreamID string `path:"streamId"`
	// Body содержит необязательный интервал в миллисекундах; отсутствие тела или поля означает значение по умолчанию.
	Body *chatTestStartBody
}

// chatTestStreamInput адресует действие ленты только по идентификатору потока.
type chatTestStreamInput struct {
	// StreamID выбирает поток, к текущей ленте которого относится операция.
	StreamID string `path:"streamId"`
}

// chatTestStartBody описывает единственный параметр запуска тестовой ленты.
type chatTestStartBody struct {
	// IntervalMS задаёт период генерации в диапазоне от 100 до 60000 миллисекунд.
	IntervalMS Field[Decimal] `json:"interval_ms,omitempty" minimum:"100" maximum:"60000"`
}

// Schema публикует точный числовой интервал и его серверное значение по умолчанию.
// registry предоставляет базовую схему Decimal для ограничения диапазона поля.
func (chatTestStartBody) Schema(registry huma.Registry) *huma.Schema {
	// intervalSchema сохраняет точную схему Decimal с диапазоном генератора и значением по умолчанию.
	intervalSchema := huma.SchemaFromType(registry, reflect.TypeFor[Decimal]())
	intervalSchema.Extensions["minimum"] = json.Number("100")
	intervalSchema.Extensions["maximum"] = json.Number("60000")
	intervalSchema.Default = json.Number("1000")
	return &huma.Schema{
		Type:                 huma.TypeObject,
		AdditionalProperties: false,
		Properties:           map[string]*huma.Schema{"interval_ms": intervalSchema},
	}
}

// chatTestStartResult сообщает фактический интервал работающего генератора.
type chatTestStartResult struct {
	// IntervalMS содержит период, который Manager применил для тестовой ленты.
	IntervalMS Decimal `json:"interval_ms" minimum:"100" maximum:"60000"`
}

// registerChatTestFeed публикует запуск, остановку и очистку тестовой ленты выбранного потока.
func (a *API) registerChatTestFeed() {
	// start описывает успешный ответ с интервалом, оставленным работающим генератором.
	start := a.chatControlOperation(http.MethodPost, "/chat/streams/{streamId}/test/start", "start-chat-test-feed", "Запустить тестовую ленту чата.")
	start.RequestBody = &huma.RequestBody{Required: false}
	register(a, start, a.startChatTestFeed)

	// stop возвращает пустой ответ после остановки генератора без очистки истории.
	stop := a.chatControlOperation(http.MethodPost, "/chat/streams/{streamId}/test/stop", "stop-chat-test-feed", "Остановить генератор тестовой ленты.")
	stop.DefaultStatus = http.StatusNoContent
	register(a, stop, a.stopChatTestFeed)

	// clear возвращает пустой ответ после очистки выбранной текущей ленты.
	clear := a.chatControlOperation(http.MethodPost, "/chat/streams/{streamId}/clear", "clear-chat-feed", "Очистить текущую ленту чата.")
	clear.DefaultStatus = http.StatusNoContent
	register(a, clear, a.clearChatFeed)
}

// startChatTestFeed запускает генератор либо сохраняет прежний интервал уже работающей ленты.
// ctx ограничивает операцию, а input задаёт поток и необязательный интервал в миллисекундах.
func (a *API) startChatTestFeed(ctx context.Context, input *chatTestStartInput) (*Result[chatTestStartResult], error) {
	// intervalMS выбирает переданный период либо штатное значение при отсутствии поля.
	intervalMS := uint64(1000)
	if input.Body != nil && input.Body.IntervalMS.Set {
		if input.Body.IntervalMS.Null {
			return nil, reasonError(core.ErrInvalid, "invalid_field", "interval_ms")
		}
		intervalMS = uint64(input.Body.IntervalMS.Value)
	}
	if intervalMS < 100 || intervalMS > 60000 {
		return nil, reasonError(core.ErrInvalid, "invalid_field", "interval_ms")
	}
	// applied содержит период фактически запущенного генератора; err сохраняет причину отказа Manager.
	applied, err := a.config.Chat.StartTest(ctx, core.ID(input.StreamID), time.Duration(intervalMS)*time.Millisecond)
	if err != nil {
		return nil, chatTestFeedError(err)
	}
	// result передаёт миллисекунды, возвращённые Manager, без округления через число с плавающей точкой.
	result := chatTestStartResult{IntervalMS: Decimal(applied / time.Millisecond)}
	return &Result[chatTestStartResult]{Body: result}, nil
}

// stopChatTestFeed останавливает генератор выбранного потока и оставляет его историю нетронутой.
// ctx ограничивает операцию, а input задаёт поток, генератор которого нужно остановить.
func (a *API) stopChatTestFeed(ctx context.Context, input *chatTestStreamInput) (*struct{}, error) {
	// err сообщает о неизвестном потоке, временной недоступности или отказе остановки.
	if err := a.config.Chat.StopTest(ctx, core.ID(input.StreamID)); err != nil {
		return nil, chatTestFeedError(err)
	}
	return &struct{}{}, nil
}

// clearChatFeed удаляет историю выбранного потока и публикует границу очистки для его подписчиков.
// ctx ограничивает операцию, а input задаёт поток, текущую ленту которого нужно очистить.
func (a *API) clearChatFeed(ctx context.Context, input *chatTestStreamInput) (*struct{}, error) {
	// err сообщает об отказе очистки или публикации границы Manager.
	if err := a.config.Chat.Clear(ctx, core.ID(input.StreamID)); err != nil {
		return nil, chatTestFeedError(err)
	}
	return &struct{}{}, nil
}

// chatTestFeedError переводит временную недоступность чтения в 503, сохраняя остальные доменные причины.
// err содержит исходную причину операции Manager.
func chatTestFeedError(err error) error {
	if errors.Is(err, core.ErrNotReady) || errors.Is(err, core.ErrStale) {
		return unavailable()
	}
	return err
}
