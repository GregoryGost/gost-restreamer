package fmp4

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// TestOpusSourceSkipOwnsExactlyOneWholeAU проверяет единственный source decoder start на положительном origin.
// t задаёт три различные полные CELT120 AU; удержанный prefix и повторная транспортная оболочка
// должны сохранить ровно последние две AU с исходными временами, без нового удаления.
func TestOpusSourceSkipOwnsExactlyOneWholeAU(t *testing.T) {
	// headers задаёт нормативный Head0, из которого строится standalone init.
	headers := []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0}
	// init сохраняет реальную пару track configs со шкалой audio48000.
	init := audioBindingInit(t, core.CodecOpus, headers)
	// wire содержит только init перед добавлением независимо заданных fragments.
	var wire bytes.Buffer
	// err сообщает об отказе сериализации исходной допустимой конфигурации.
	if _, err := NewMuxer(&wire, init, testLimits()); err != nil {
		t.Fatal(err)
	}
	// raw не имеет UUID: standard dOps остаётся единственным источником audio start semantics.
	raw := withoutConfigUUID(t, wire.Bytes())
	// offset находит единственный дочерний dOps в созданном ограниченном init.
	offset := bytes.Index(raw, []byte("dOps"))
	if offset < 0 || bytes.Count(raw, []byte("dOps")) != 1 || len(raw) < offset+15 {
		t.Fatal("фикстура не содержит единственный полный dOps")
	}
	// Нормативный dOps хранит preSkip в BE после version0/channels2.
	raw[offset+6], raw[offset+7] = 0, 120
	// payloads различают начальную и обе retained AU без совпадений по тестовому содержимому.
	payloads := [][]byte{{0xe4, 0x21, 0x31}, {0xe4, 0x42, 0x52}, {0xe4, 0x63, 0x73}}
	// index и payload задают номер фрагмента и отдельную полную CELT120 AU.
	for index, payload := range payloads {
		raw = append(raw, rawAudioBindingFragment(uint32(index+1), 480000+uint64(index)*120, 120, payload)...)
	}
	// demux и err проверяют bounded source completion до публикации Init.
	demux, err := NewDemuxer(bytes.NewReader(raw), DemuxOptions{Limits: testLimits(), SourceEpoch: 11, VideoRevision: 2, AudioRevision: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(demux.Init().Audio.Headers(), headers) {
		t.Fatalf("source adapter не завершил Head0: %x", demux.Init().Audio.Headers())
	}
	// retained содержит строго две исходные AU после единственного initial skip120.
	var retained []media.Packet
	// index выбирает ожидаемый retained payload и точное исходное время.
	for index := 1; index < 3; index++ {
		// packet и readErr должны выдать исходную AU без перемещения timestamps.
		packet, readErr := demux.Next()
		if readErr != nil || !bytes.Equal(packet.Payload(), payloads[index]) || packet.Info().PTS != 480000+int64(index)*120 || packet.Info().DTS != 480000+int64(index)*120 || packet.Info().DurationTicks != 120 {
			t.Fatalf("retained%d=%+v bytes=%x err=%v", index, packet.Info(), packet.Payload(), readErr)
		}
		retained = append(retained, packet)
	}
	if _, err = demux.Next(); !errors.Is(err, io.EOF) {
		t.Fatal("source owner удалил не ровно одну AU", err)
	}
	// normalized представляет следующий физический decoder input с уже обработанным Head0.
	var normalized bytes.Buffer
	// mux переносит retained AU как есть; новый Demuxer не должен считать новый transport новым audio start120.
	mux, err := NewMuxer(&normalized, demux.Init(), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	// packet сохраняет исходные payload/PTS/DTS/duration при смене оболочки.
	for _, packet := range retained {
		if err = mux.WritePacket(packet); err != nil {
			t.Fatal(err)
		}
	}
	demux, err = NewDemuxer(bytes.NewReader(normalized.Bytes()), DemuxOptions{Limits: testLimits()})
	if err != nil {
		t.Fatal(err)
	}
	// expected задаёт каждую retained AU, включая первую после нормализации.
	for _, expected := range retained {
		// actual и readErr доказывают отсутствие повторного skip, alias и retiming.
		actual, readErr := demux.Next()
		if readErr != nil || actual.Info() != expected.Info() || !bytes.Equal(actual.Payload(), expected.Payload()) {
			t.Fatal("повторная оболочка вновь применила source skip", actual.Info(), readErr)
		}
	}
	if _, err = demux.Next(); !errors.Is(err, io.EOF) {
		t.Fatal("повторная оболочка изменила число retained AU", err)
	}
}
