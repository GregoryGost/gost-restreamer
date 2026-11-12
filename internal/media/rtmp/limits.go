// Package rtmp принимает ограниченный поток Enhanced RTMP AV1/AAC и передаёт AV1/H.264/AAC через RTMP/RTMPS.
package rtmp

import (
	"errors"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"time"
)

var (
	// ErrLimit сохраняет общую категорию превышения бюджета кодека или RTMP.
	ErrLimit = codec.ErrLimit
	// ErrFraming обозначает повреждённую структуру сообщений или блоков RTMP.
	ErrFraming = errors.New("некорректное обрамление RTMP")
	// ErrCodec сохраняет общую категорию неподдержанного сжатого потока.
	ErrCodec = codec.ErrCodec
	// ErrUnready сохраняет общую категорию недостаточно определённого формата.
	ErrUnready = codec.ErrUnready
)

// Limits задаёт положительные бюджеты на одно соединение, включая память читателя библиотеки.
// MaxRetainedBytes учитывает верхнюю границу удерживаемых полезных данных и структурных слотов,
// копии сборки/декодирования и буфер bufio; это не предел объёма резидентной памяти процесса.
type Limits struct {
	// MaxConnections ограничивает число одновременно зарегистрированных входящих соединений сервера.
	MaxConnections uint32
	// MaxChunkStreams ограничивает число используемых потоков блоков одного соединения.
	MaxChunkStreams uint32
	// MaxMessageBytes ограничивает объявленную длину тела сообщения в байтах.
	MaxMessageBytes uint32
	// MaxChunkBytes ограничивает допустимый размер блока в байтах.
	MaxChunkBytes uint32
	// MaxFragments ограничивает число фрагментов одного сообщения.
	MaxFragments uint32
	// MaxRetainedBytes ограничивает логический резерв буферов и удержанных сообщений в байтах.
	MaxRetainedBytes uint64
	// MaxControlBytes ограничивает тело управляющего сообщения в байтах.
	MaxControlBytes uint32
	// MaxConfigBytes ограничивает тело сообщения конфигурации кодека в байтах.
	MaxConfigBytes uint32
	// MaxAMFBytes ограничивает тело проверяемого AMF-сообщения в байтах.
	MaxAMFBytes uint32
	// MaxAMFDepth ограничивает глубину вложенности AMF до построения графа объектов.
	MaxAMFDepth uint32
	// MaxAMFElements ограничивает общее число значений AMF.
	MaxAMFElements uint32
	// MaxReadAheadBytes ограничивает объём одного читаемого вперёд блока вместе с заголовком, в
	// байтах.
	MaxReadAheadBytes uint32
	// HandshakeTimeout ограничивает согласование до publish. При строгом Feed без Admit
	// этот же срок покрывает допуск; автозапуск получает отдельный AdmissionTimeout.
	HandshakeTimeout time.Duration
	// ReadTimeout задаёт срок очередного чтения блока после согласования.
	ReadTimeout time.Duration
	// WriteTimeout задаёт срок очередной транспортной записи.
	WriteTimeout time.Duration
	// ShutdownTimeout ограничивает отдельное ожидание остановки соединений и отзыва допуска.
	ShutdownTimeout time.Duration
}

// Validate исключает неограниченные и несовместимые с gortmplib значения.
func (l Limits) Validate() error {
	if l.MaxConnections == 0 || l.MaxChunkStreams == 0 || l.MaxChunkStreams > 62 || l.MaxMessageBytes == 0 || l.MaxMessageBytes > 10<<20 || l.MaxChunkBytes < 128 || l.MaxChunkBytes > 10<<20 || l.MaxFragments == 0 || l.MaxFragments > 1<<20 || l.MaxControlBytes < 6 || l.MaxControlBytes > l.MaxMessageBytes || l.MaxConfigBytes < 4 || l.MaxConfigBytes > l.MaxMessageBytes || l.MaxAMFBytes == 0 || l.MaxAMFBytes > l.MaxMessageBytes || l.MaxAMFDepth == 0 || l.MaxAMFDepth > 64 || l.MaxAMFElements == 0 || l.MaxReadAheadBytes < 18 || l.MaxReadAheadBytes > 10<<20 || l.MaxRetainedBytes < 8192 || l.HandshakeTimeout <= 0 || l.ReadTimeout <= 0 || l.WriteTimeout <= 0 || l.ShutdownTimeout <= 0 {
		return ErrLimit
	}
	return nil
}

// ValidateIngress проверяет полный резерв входного сервера до открытия ресурсов.
func (l Limits) ValidateIngress() error {
	// err сообщает о некорректных общих ограничениях до проверки полного входного резерва.
	if err := l.Validate(); err != nil {
		return err
	}
	if l.baseRetained() > l.MaxRetainedBytes {
		return ErrLimit
	}
	return nil
}

// Вычисляет базовый логический резерв соединения для буферов, копий сообщений и двух ограниченных
// графов AMF; текущие фрагменты учитываются отдельно.
func (l Limits) baseRetained() uint64 {
	// Два ограниченных графа AMF покрывают сохранённую команду публикации и
	// следующее типизированное сообщение, включая внутренние массивы и заголовки элементов.
	return uint64(8192) + 8*uint64(l.MaxMessageBytes) + 4*uint64(l.MaxChunkBytes) + uint64(l.MaxReadAheadBytes) + 192*uint64(l.MaxAMFElements) + 128*uint64(l.MaxAMFDepth)
}
