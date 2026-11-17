package codec

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// TestOpusLiteralHeadersAndSingleCELT проверяет нормативный порядок байтов RFC и отдельную границу dOps.
// t проверяет полный пакет CELT на 120 отсчётов, законное дополнение кода 3 и неподдержанные формы.
func TestOpusLiteralHeadersAndSingleCELT(t *testing.T) {
	// raw содержит нормативный OpusHead с начальным исключением 120 и справочной частотой 44100 Гц.
	raw := []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 120, 0, 0x44, 0xac, 0, 0, 0, 0, 0}
	// head содержит прочитанные поля с младшим байтом первым; err сообщает об отказе нормативного образца.
	head, err := ParseOpusHead(raw)
	if err != nil || head.PreSkip != 120 || head.InputSampleRate != 44100 {
		t.Fatalf("head=%+v err=%v", head, err)
	}
	// dops хранит нормативное тело версии 0 со старшим байтом первым, независимо от закреплённого SDK.
	dops, err := OpusHeadToDOps(raw)
	if err != nil || !bytes.Equal(dops, []byte{0, 2, 0, 120, 0, 0, 0xac, 0x44, 0, 0, 0}) {
		t.Fatalf("dOps=%x err=%v", dops, err)
	}
	// back содержит независимый OpusHead, восстановленный из dOps.
	back, err := OpusHeadFromDOps(dops)
	if err != nil || !bytes.Equal(back, raw) {
		t.Fatalf("Head roundtrip=%x err=%v", back, err)
	}
	// zero содержит заголовок после уже потреблённой владельцем начальной AU.
	zero, err := NormalizeOpusHead(raw)
	if err != nil || binary.LittleEndian.Uint16(zero[10:12]) != 0 || raw[10] != 120 {
		t.Fatal("skip normalization изменила исходный Head", err)
	}
	// packet включает код 0, пустой сжатый кадр, код 3 с CBR/VBR и законное произвольное дополнение.
	for _, packet := range [][]byte{{0xe4, 0xff, 0xfe}, {0xe4}, {0xe7, 1, 0xff, 0xfe}, {0xe7, 0x81, 0xff, 0xfe}, {0xe7, 0x41, 2, 0xff, 0xfe, 0xa5, 0x5a}} {
		// samples подтверждает 120 отсчётов независимо от справочной частоты; sampleErr содержит отказ AU.
		samples, sampleErr := AudioSamples(core.CodecOpus, zero, packet)
		if sampleErr != nil || samples != 120 {
			t.Fatalf("packet=%x samples=%d err=%v", packet, samples, sampleErr)
		}
		if sampleErr = ValidateAudioDuration(core.CodecOpus, zero, packet, media.PacketInfo{DurationTicks: 120}, media.TimeBase{Numerator: 1, Denominator: 48000}); sampleErr != nil {
			t.Fatal(sampleErr)
		}
	}
	// packet задаёт гибридный режим, неверную длительность, моно, два кадра либо усечённое дополнение.
	for _, packet := range [][]byte{nil, {0x64}, {0xec}, {0xe0}, {0xe5, 0, 0}, {0xe6, 0, 0}, {0xe7}, {0xe7, 0}, {0xe7, 2}, {0xe7, 0x41, 255}, {0xe7, 0x41, 2, 0}} {
		if _, err = AudioSamples(core.CodecOpus, zero, packet); err == nil {
			t.Fatalf("неподдержанный packet=%x принят", packet)
		}
	}
	// badHeader проверяет запрет произвольного PreSkip без изменения исходного образца.
	badHeader := bytes.Clone(raw)
	badHeader[10] = 121
	if _, err = ParseOpusHead(badHeader); err == nil {
		t.Fatal("частичный initial skip принят")
	}
	if err = ValidateAudioDuration(core.CodecOpus, zero, []byte{0xe4}, media.PacketInfo{DurationTicks: 3}, media.TimeBase{Numerator: 1, Denominator: 1000}); err == nil {
		t.Fatal("округлённая duration3ms принята вместо2.5ms")
	}
	if err = ValidateAudioDuration(core.CodecAAC, nil, nil, media.PacketInfo{}, media.TimeBase{}); err != nil {
		t.Fatal("прежний AAC duration contract изменён", err)
	}
}
