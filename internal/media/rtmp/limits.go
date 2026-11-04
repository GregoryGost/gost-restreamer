// Package rtmp принимает ограниченный поток Enhanced RTMP AV1/AAC.
package rtmp

import (
	"errors"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"time"
)

var (
	ErrLimit   = codec.ErrLimit
	ErrFraming = errors.New("некорректное обрамление RTMP")
	ErrCodec   = codec.ErrCodec
	ErrUnready = codec.ErrUnready
)

// Limits задаёт положительные бюджеты на одно соединение, включая память читателя библиотеки.
// MaxRetainedBytes учитывает верхнюю границу удерживаемых полезных данных и структурных слотов,
// копии сборки/декодирования и буфер bufio; это не предел RSS процесса.
type Limits struct {
	MaxConnections    uint32
	MaxChunkStreams   uint32
	MaxMessageBytes   uint32
	MaxChunkBytes     uint32
	MaxFragments      uint32
	MaxRetainedBytes  uint64
	MaxControlBytes   uint32
	MaxConfigBytes    uint32
	MaxAMFBytes       uint32
	MaxAMFDepth       uint32
	MaxAMFElements    uint32
	MaxReadAheadBytes uint32
	HandshakeTimeout  time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	ShutdownTimeout   time.Duration
}

// Validate исключает неограниченные и несовместимые с gortmplib значения.
func (l Limits) Validate() error {
	if l.MaxConnections == 0 || l.MaxChunkStreams == 0 || l.MaxChunkStreams > 62 || l.MaxMessageBytes == 0 || l.MaxMessageBytes > 10<<20 || l.MaxChunkBytes < 128 || l.MaxChunkBytes > 10<<20 || l.MaxFragments == 0 || l.MaxFragments > 1<<20 || l.MaxControlBytes < 6 || l.MaxControlBytes > l.MaxMessageBytes || l.MaxConfigBytes < 4 || l.MaxConfigBytes > l.MaxMessageBytes || l.MaxAMFBytes == 0 || l.MaxAMFBytes > l.MaxMessageBytes || l.MaxAMFDepth == 0 || l.MaxAMFDepth > 64 || l.MaxAMFElements == 0 || l.MaxReadAheadBytes < 18 || l.MaxReadAheadBytes > 10<<20 || l.MaxRetainedBytes < 8192 || l.HandshakeTimeout <= 0 || l.ReadTimeout <= 0 || l.WriteTimeout <= 0 || l.ShutdownTimeout <= 0 {
		return ErrLimit
	}
	return nil
}

func (l Limits) baseRetained() uint64 {
	// Два ограниченных графа AMF покрывают сохранённую команду публикации и
	// следующее типизированное сообщение, включая внутренние массивы и заголовки элементов.
	return uint64(8192) + 8*uint64(l.MaxMessageBytes) + 4*uint64(l.MaxChunkBytes) + uint64(l.MaxReadAheadBytes) + 192*uint64(l.MaxAMFElements) + 128*uint64(l.MaxAMFDepth)
}
