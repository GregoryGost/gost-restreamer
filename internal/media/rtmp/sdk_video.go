package rtmp

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/abema/go-mp4"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// vp9SDKMessage восстанавливает descriptor только для текущего завершённого VP9 record.
// Вызывающий уже проверил единственность завершения и отсутствие причинной ошибки транспорта.
// Повторный вызов mp4.Unmarshal проверяет точную ошибку pinned SDK, а добавленные version/flags
// существуют только в локальном descriptor: исходные сетевые байты остаются без FullBox.
func (c *wireConn) vp9SDKMessage(cause error) (message.Message, bool) {
	// raw содержит исходное полное SequenceStart, record — восемь байтов нормативного record.
	raw := c.lastConfig
	if len(raw) != 13 || raw[0]&0x8f != 0x80 || string(raw[1:5]) != "vp09" {
		return nil, false
	}
	record := raw[5:]
	if record[6] != 0 || record[7] != 0 {
		return nil, false
	}
	// recordErr отделяет повреждённый record от корректного неподдержанного codec tuple;
	// последний получит точную processing-категорию в publication вместо ошибки FullBox SDK.
	if _, recordErr := codec.ParseVP9Config(record); recordErr != nil && !errors.Is(recordErr, codec.ErrUnready) && !errors.Is(recordErr, codec.ErrUnsupported) {
		return nil, false
	}
	// original и parseErr воспроизводят только разбор FullBox на тех же исходных байтах.
	original := mp4.VpcC{}
	_, parseErr := mp4.Unmarshal(bytes.NewReader(record), uint64(len(record)), &original, mp4.Context{})
	if parseErr == nil || cause.Error() != fmt.Sprintf("invalid VP9 configuration: %v", parseErr) {
		return nil, false
	}
	// unwrapped подтверждает одноуровневую ошибку mp4, исключая произвольные обёртки.
	unwrapped := errors.Unwrap(cause)
	if unwrapped == nil || unwrapped.Error() != parseErr.Error() || errors.Unwrap(unwrapped) != nil {
		return nil, false
	}
	// fullBox добавляет обязательные для SDK version1/flags0 перед неизменённым record.
	fullBox := append([]byte{1, 0, 0, 0}, record...)
	// descriptor содержит фактические поля record, включая исходные CICP и глубину.
	descriptor := &mp4.VpcC{}
	if _, parseErr = mp4.Unmarshal(bytes.NewReader(fullBox), uint64(len(fullBox)), descriptor, mp4.Context{}); parseErr != nil {
		return nil, false
	}
	return &message.VideoExSequenceStart{ChunkStreamID: c.lastConfigChunkStream, MessageStreamID: c.lastConfigMessageStream, FourCC: message.FourCCVP9, VP9Config: descriptor}, true
}
