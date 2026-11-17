package codec

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// TestHDRMetadataSyntaxAndPersistence проверяет буквальные метаданные AV1, неизвестные значения SEI и границы IDR.
func TestHDRMetadataSyntaxAndPersistence(t *testing.T) {
	// raw содержит полный реальный CLL/MDCV OBU из SVT, включая metadata_type и завершающие биты.
	// err проверяет корректность буквального hex эталона OBU.
	raw, err := hex.DecodeString("2a060103e80190802a1a02ae1451ec43d7b0a426660f5c500d54390003e8000000000280")
	if err != nil {
		t.Fatal(err)
	}
	// metadata содержит физические значения с двоичными сетками AV1 без переписывания кодовых слов.
	metadata, err := ParseAV1HDR(raw)
	if err != nil || !metadata.MDCV.FullyKnown() || !metadata.CLL.FullyKnown() {
		t.Fatal(metadata, err)
	}
	if metadata.MDCV.Primaries[0].X.Raw != 44564 || metadata.MDCV.MinLuminance.Raw != 2 {
		t.Fatal(metadata)
	}
	if _, err = ParseAV1HDR(raw[:len(raw)-1]); err == nil {
		t.Fatal("Усечён trailing bit")
	}
	// sei содержит H.264 CLL со значением неизвестным нулём и известным MaxFALL=400.
	sei := []byte{6, 144, 4, 0, 0, 1, 144, 128}
	// block проверяет сохранение нуля без назначения типовых сведений.
	// err сообщает об усечённом SEI либо ошибке восстановления CLL.
	block, err := ParseHDRSEI(sei, false)
	if err != nil || block.CLL.MaxCLL.Known || !block.CLL.MaxFALL.Known {
		t.Fatal(block, err)
	}
	// state проверяет первый IDR H.264 и разрешённую смену в новой независимой CVS.
	var state HDRState
	// au содержит один полный SEI с четырёхбайтовой длиной.
	au := append([]byte{0, 0, 0, 8}, sei...)
	if _, err = state.Observe(core.CodecH264, nil, au, true); err != nil {
		t.Fatal(err)
	}
	sei[4] = 10
	au = append([]byte{0, 0, 0, 8}, sei...)
	if _, err = state.Observe(core.CodecH264, nil, au, false); err == nil {
		t.Fatal("Принят меняющийся CLL в CVS")
	}
	if _, err = state.Observe(core.CodecH264, nil, au, true); err != nil {
		t.Fatal("Смена на IDR", err)
	}
	state.Reset()
	// emptyAU содержит только байты среза; состояние HDR доверяет независимости, установленной отдельным разборщиком.
	emptyAU := []byte{0, 0, 0, 2, 0x65, 0x80}
	if _, err = state.Observe(core.CodecH264, nil, emptyAU, true); err != nil {
		t.Fatal(err)
	}
	if _, err = state.Observe(core.CodecH264, nil, au, false); err == nil {
		t.Fatal("Принят поздний H264 CLL")
	}
	state.Reset()
	// hevcNAL содержит префиксный SEI HEVC с тем же поздним CLL; правило H264 сюда не переносится.
	hevcNAL := append([]byte{78, 1}, sei[1:]...)
	// hevcAU сохраняет четырёхбайтовое обрамление полного HEVC NAL.
	hevcAU := append([]byte{0, 0, 0, byte(len(hevcNAL))}, hevcNAL...)
	state.started = true
	if _, err = state.Observe(core.CodecHEVC, nil, hevcAU, false); err != nil {
		t.Fatal("Поздний HEVC CLL", err)
	}
	if _, err = ParseHDRSEI([]byte{6, 137, 24, 0}, false); err == nil {
		t.Fatal("Усечён MDCV")
	}
}

// TestHDRFreshProjection проверяет направленное сравнение проекций и заполнение отсутствующих блоков.
func TestHDRFreshProjection(t *testing.T) {
	// source сохраняет точные исходные целочисленные сведения CLL из SEI.
	source := media.HDRMetadata{CLL: media.HDRContentLight{Present: true, Source: media.HDRSourceH264SEI, MaxCLL: media.HDRValue{Raw: 1000, Grid: media.HDRGridInteger, Known: true}, MaxFALL: media.HDRValue{Raw: 400, Grid: media.HDRGridInteger, Known: true}}}
	// output содержит тот же физический CLL другого установленного происхождения.
	output := source
	output.CLL.Source = media.HDRSourceAV1OBU
	// err сообщает о повреждённом MDCV вместо ложного успеха.
	if err := MatchHDRProjection(source, output); err != nil {
		t.Fatal(err)
	}
	// restored заполняет только действительно отсутствующие блоки.
	restored, err := RestoreSourceHDR(media.HDRMetadata{}, source)
	if err != nil || restored != source {
		t.Fatal(restored, err)
	}
	output.CLL.MaxCLL.Raw = 999
	if _, err = ReconcileFreshHDR(source, media.HDRMetadata{}, output); err == nil {
		t.Fatal("Подменён actual CLL")
	}
	// unknown сохраняет исходное кодовое слово; его невозможно направленно превратить в иную сетку.
	unknown := source
	unknown.CLL.MaxCLL.Known = false
	output = unknown
	output.CLL.MaxCLL.Grid = media.HDRGridSEILuminance
	if err = MatchHDRProjection(unknown, output); err == nil {
		t.Fatal("Потерян unknown codeword")
	}
	// malformed проверяет missing SEI trailing bits без изменения входного raw AU.
	malformed := []byte{6, 144, 4, 3, 232, 1, 144}
	// before сохраняет исходные байты для проверки отсутствия мутаций разборщиком.
	before := bytes.Clone(malformed)
	if _, err = ParseHDRSEI(malformed, false); err == nil || !bytes.Equal(malformed, before) {
		t.Fatal(err)
	}
}
