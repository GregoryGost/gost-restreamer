package chatwire

import "github.com/GregoryGost/gost-restreamer/internal/core"

const (
	// frameID предваряет непрозрачную позицию кадра.
	frameID = "id: "
	// frameEvent начинает строку SSE с именем события.
	frameEvent = "\nevent: "
	// frameData начинает строку с компактным JSON.
	frameData = "\ndata: "
	// frameEnd завершает последнюю строку и отделяет кадр пустой строкой.
	frameEnd = "\n\n"
)

// FrameSize учитывает обе позиции cursor, имя события, JSON и разделители SSE.
func FrameSize(event core.ChatEvent, data []byte) uint64 {
	return uint64(len(frameID)+len(frameEvent)+len(frameData)+len(frameEnd)) +
		uint64(len(event.Cursor)) + uint64(len(event.Kind)) + uint64(len(data))
}

// Frame оформляет полное SSE-событие с позицией. Кадр, не помещающийся в limit,
// отклоняется до выделения общего буфера. Cursor создаётся лентой сервера.
func Frame(event core.ChatEvent, limit uint64) ([]byte, error) {
	// data содержит JSON в том же формате, который учитывает журнал Hub, err сообщает об отказе кодирования.
	data, err := EventJSON(event, nil)
	if err != nil {
		return nil, err
	}
	// size включает служебные строки и проверяется до преобразования в int.
	size := FrameSize(event, data)
	if size > limit || size > uint64(^uint(0)>>1) {
		return nil, core.ErrObservationLimit
	}
	// frame объединяет все строки для единственной ограниченной записи.
	frame := make([]byte, 0, int(size))
	frame = append(frame, frameID...)
	frame = append(frame, event.Cursor...)
	frame = append(frame, frameEvent...)
	frame = append(frame, event.Kind...)
	frame = append(frame, frameData...)
	frame = append(frame, data...)
	return append(frame, frameEnd...), nil
}
