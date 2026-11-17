package codec_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// contractMastering задаёт RGB-сведения с одной сеткой координат и отдельными сетками яркости.
// rawXY, maximum и minimum являются буквальными кодовыми словами выбранного источника.
// xyGrid, maxGrid и minGrid задают их сетки, source сохраняет происхождение сведений.
func contractMastering(rawXY, maximum, minimum uint64, xyGrid, maxGrid, minGrid media.HDRGrid, source media.HDRSource) media.HDRMetadata {
	// xy является известной парой одинаковых координат для всех полей: тест проверяет сетки, без оценки цветового охвата.
	xy := media.HDRChromaticity{X: media.HDRValue{Raw: rawXY, Grid: xyGrid, Known: true}, Y: media.HDRValue{Raw: rawXY, Grid: xyGrid, Known: true}}
	return media.HDRMetadata{MDCV: media.HDRMasteringDisplay{
		Present: true, Source: source, Primaries: [3]media.HDRChromaticity{xy, xy, xy}, WhitePoint: xy,
		MaxLuminance: media.HDRValue{Raw: maximum, Grid: maxGrid, Known: true},
		MinLuminance: media.HDRValue{Raw: minimum, Grid: minGrid, Known: true},
	}}
}

// TestFreshHDRChecksBothProjectionsAgainstSource проверяет две независимые проекции source:
// Максимальная яркость SEI 10.0001 сохраняется в контейнерном SEI 100001,
// но округляется в AV1 UQ24.8 до 2560 и затем обратно в SEI 100000.
// Эти буквальные результаты следуют сеткам 1/10000 и 1/256 и округлению к ближайшему.
// Свежий собственный результат может согласовать дескрипторы; чужой конфликт и неверный source недопустимы.
func TestFreshHDRChecksBothProjectionsAgainstSource(t *testing.T) {
	// source сохраняет допустимые SEI-коды координат 0.3127, максимума 10.0001 и минимума 0.005.
	source := contractMastering(15635, 100001, 50, media.HDRGridSEIChromaticity, media.HDRGridSEILuminance, media.HDRGridSEILuminance, media.HDRSourceH264SEI)
	// container является прямой проекцией source в сетку SEI внешнего дескриптора MP4.
	container := contractMastering(15635, 100001, 50, media.HDRGridSEIChromaticity, media.HDRGridSEILuminance, media.HDRGridSEILuminance, media.HDRSourceContainer)
	// inband содержит независимую прямую проекцию source в OBU метаданных AV1.
	inband := contractMastering(20493, 2560, 82, media.HDRGridBinaryChromaticity, media.HDRGridBinaryMaxLuminance, media.HDRGridBinaryMinLuminance, media.HDRSourceAV1OBU)
	// beforeSource, beforeContainer и beforeInband защищают каждый вход от изменения при согласовании.
	beforeSource, beforeContainer, beforeInband := source, container, inband
	if codec.MatchHDRProjection(source, container) != nil || codec.MatchHDRProjection(source, inband) != nil {
		t.Fatal("Корректная независимая проекция source отклонена")
	}
	if codec.MatchHDRProjection(inband, container) == nil {
		t.Fatal("Промежуточная сетка скрыла различие яркостей SEI100001 и SEI100000")
	}
	// reconciled и err сохраняют авторитетные сведения собственного результата либо причину отказа.
	reconciled, err := codec.ReconcileFreshHDR(source, container, inband)
	if err != nil || reconciled != inband || source != beforeSource || container != beforeContainer || inband != beforeInband {
		t.Fatalf("Свежий собственный результат потерял авторитетные сведения: %+v, %v", reconciled, err)
	}
	// corrupt совпадает с повторной проекцией OBU, но противоречит прямой проекции source.
	corrupt := container
	corrupt.MDCV.MaxLuminance.Raw = 100000
	if _, err = codec.ReconcileFreshHDR(source, corrupt, inband); err == nil {
		t.Fatal("Ошибочный container принят по промежуточной сетке вместо source")
	}
}

// TestAV1ConfigMetadataSurvivesNormalization проверяет отдельный новый нормализатор:
// полные CLL OBU и заполнение из av1C остаются точными, прежний нормализатор сохраняет только заголовок последовательности.
func TestAV1ConfigMetadataSurvivesNormalization(t *testing.T) {
	// sequence содержит буквальный OBU последовательности SDR с размерами 1920×804 без собственного FPS.
	sequence := []byte{0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
	// cll сохраняет CLL 1000/400 с metadata_type и trailing_bits; padding содержит OBU заполнения конфигурации.
	cll := []byte{0x2a, 6, 1, 3, 232, 1, 144, 128}
	padding := []byte{0x7a, 1, 0}
	// base содержит av1C префикс и sequence; template дополнительно содержит дополнительные OBU конфигурации.
	base := append([]byte{0x81, 8, 0x0c, 0}, sequence...)
	template := append(append(bytes.Clone(base), cll...), padding...)
	// tu содержит тот же sequence, повтор CLL и показанный независимый KEY_FRAME.
	tu := append(append(bytes.Clone(sequence), cll...), 0x1a, 1, 0x10)
	// templateBefore и tuBefore защищают входные буферы от изменения обоими нормализаторами.
	templateBefore, tuBefore := bytes.Clone(template), bytes.Clone(tu)
	// legacy и actual показывают прежний результат с одним заголовком последовательности; err фиксирует ошибку разбора входа.
	legacy, actual, err := codec.AV1ConfigFromTU(template, tu, core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil || !bytes.Equal(legacy, base) || actual.Width != 1920 || actual.Height != 804 {
		t.Fatalf("Прежняя нормализация изменилась: %x, %+v, %v", legacy, actual, err)
	}
	// normalized использует новый перенос OBU конфигурации без дублирования CLL из первого TU.
	normalized, _, err := codec.AV1ConfigFromTUWithMetadata(template, tu, actual.FrameRate)
	if err != nil || !bytes.Equal(normalized, templateBefore) || !bytes.Equal(template, templateBefore) || !bytes.Equal(tu, tuBefore) {
		t.Fatalf("Потеряны OBU конфигурации или изменены исходные байты: %x, %v", normalized, err)
	}
	// state применяет CLL конфигурации к первому и следующему кадрам, где повторных метаданных уже нет.
	var state codec.HDRState
	// first содержит снимок первого независимого TU, second — снимок следующего TU без метаданных.
	first, err := state.Observe(core.CodecAV1, normalized, tu, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.Observe(core.CodecAV1, normalized, []byte{0x1a, 1, 0x30}, false)
	if err != nil || !first.CLL.Present || first.CLL.MaxCLL.Raw != 1000 || first.CLL.MaxFALL.Raw != 400 || second != first {
		t.Fatalf("Метаданные конфигурации не пережили следующий TU: %+v, %+v, %v", first, second, err)
	}
}

// contractLightAU формирует H264 SEI CLL с четырёхбайтовой длиной.
// maximum и average являются независимыми кодами uint16 верхних границ яркости.
func contractLightAU(maximum, average uint16) []byte {
	// nal содержит SEI H264 до записи двух 16-битных CLL.
	nal := []byte{6, 144, 4, 0, 0, 0, 0, 128}
	binary.BigEndian.PutUint16(nal[3:5], maximum)
	binary.BigEndian.PutUint16(nal[5:7], average)
	// framed выделяет собственный AU: заголовок длины не входит в NAL.
	framed := make([]byte, 4, len(nal)+4)
	binary.BigEndian.PutUint32(framed, uint32(len(nal)))
	return append(framed, nal...)
}

// TestSEIPersistenceRejectsChangeWithoutDamagingState проверяет постоянство CLL в CVS,
// атомарность отказа, сохранение неопределённого нуля и законную смену при новом независимом старте.
func TestSEIPersistenceRejectsChangeWithoutDamagingState(t *testing.T) {
	// state накапливает только метаданные кодека H264, независимо от внешнего colorInfo.
	var state codec.HDRState
	// firstAU содержит CLL первого IDR с неизвестным MaxCLL и известным MaxFALL 400.
	firstAU := contractLightAU(0, 400)
	// first и err сохраняют первый принятый снимок и отказ его разбора.
	first, err := state.Observe(core.CodecH264, nil, firstAU, true)
	if err != nil || first.CLL.MaxCLL.Known || !first.CLL.MaxFALL.Known {
		t.Fatalf("Неопределённый CLL изменён: %+v, %v", first, err)
	}
	// changedAU нарушает постоянство текущего CVS; его отказ не должен изменить state.
	changedAU := contractLightAU(1000, 400)
	if _, err = state.Observe(core.CodecH264, nil, changedAU, false); !errors.Is(err, codec.ErrCodec) {
		t.Fatalf("Смена CLL внутри CVS принята: %v", err)
	}
	// noMetadata содержит только зависимый срез; разбор геометрии остаётся обязанностью Inspector.
	noMetadata := []byte{0, 0, 0, 2, 0x41, 0x80}
	// afterFailure подтверждает, что неуспешное Observe не оставило частичный снимок.
	afterFailure, err := state.Observe(core.CodecH264, nil, noMetadata, false)
	if err != nil || afterFailure != first {
		t.Fatalf("Отказ повредил состояние: %+v, %v", afterFailure, err)
	}
	// next разрешает новое авторское CLL только на следующем независимом CVS.
	next, err := state.Observe(core.CodecH264, nil, changedAU, true)
	if err != nil || next.CLL.MaxCLL.Raw != 1000 || !next.CLL.MaxCLL.Known {
		t.Fatalf("Новый CVS не принял новый снимок: %+v, %v", next, err)
	}
	state.Reset()
	if _, err = state.Observe(core.CodecH264, nil, noMetadata, true); err != nil {
		t.Fatal(err)
	}
	if _, err = state.Observe(core.CodecH264, nil, changedAU, false); !errors.Is(err, codec.ErrCodec) {
		t.Fatalf("Поздний H264 CLL принят: %v", err)
	}
}

// TestVP9RecordRequiresActualIndependentFrame проверяет отличие record от vpcC FullBox,
// отсутствие FPS в record, зависимый кадр с опорным состоянием и отказ show_existing_frame без фиктивной независимости.
func TestVP9RecordRequiresActualIndependentFrame(t *testing.T) {
	// record задаёт profile0, 8 бит, limited с неопределёнными primaries/transfer и известной матрицей BT.709.
	record := []byte{0, 10, 0x82, 2, 2, 1, 0, 0}
	// keyAU и interAU содержат два последовательных синтетических кадра 64×64 из локальной FFmpeg-фикстуры.
	// err сообщает об ошибке чтения их буквальной шестнадцатеричной записи.
	keyAU, err := hex.DecodeString("824983424003f003f60038241c184a00003060000078b9ffff57b0ffffff7c17fffffffc78c1cfd2e880")
	if err != nil {
		t.Fatal(err)
	}
	interAU, err := hex.DecodeString("860040929c00500000036000005470")
	if err != nil {
		t.Fatal(err)
	}
	// fps является частотой, проверенной вызывающим компонентом: сам record не может дать это значение.
	fps := core.FrameRate{Numerator: 30, Denominator: 1}
	if _, err = codec.ParseVP9Config(append([]byte{1, 0, 0, 0}, record...)); !errors.Is(err, codec.ErrCodec) {
		t.Fatalf("FullBox принят вместо record: %v", err)
	}
	// description хранит опорные размеры только после успешного независимого кадра.
	description, err := codec.ParseVP9Config(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = description.Inspect(interAU, fps); !errors.Is(err, codec.ErrUnready) {
		t.Fatalf("Зависимый кадр получил выдуманные опорные размеры: %v", err)
	}
	if _, _, err = description.Inspect(keyAU, core.FrameRate{}); !errors.Is(err, codec.ErrUnready) {
		t.Fatalf("Неполный фактический формат получил выдуманный FPS: %v", err)
	}
	// actual, independent и err описывают первый кадр после подтверждения FPS.
	actual, independent, err := description.Inspect(keyAU, fps)
	if err != nil || !independent || actual.Width != 64 || actual.Height != 64 || actual.CodecProfile != "profile0" || actual.BitDepth != 8 || actual.FrameRate != fps || actual.Color.Primaries != "unspecified" || actual.Color.Transfer != "unspecified" || actual.Color.Matrix != "bt709" {
		t.Fatalf("Record или общее цветовое пространство подменили фактический формат: %+v, key=%v, %v", actual, independent, err)
	}
	if _, independent, err = description.Inspect([]byte{0x88}, fps); !errors.Is(err, codec.ErrUnsupported) || independent {
		t.Fatalf("show_existing_frame принят как независимый кадр: key=%v, %v", independent, err)
	}
	// conflicting меняет кодированный диапазон при неизменном limited record.
	conflicting := bytes.Clone(keyAU)
	conflicting[4] |= 0x10
	if _, _, err = description.Inspect(conflicting, fps); !errors.Is(err, codec.ErrCodec) {
		t.Fatalf("Противоречие record и кодированного диапазона принято: %v", err)
	}
	// afterRejected подтверждает сохранение опорного состояния после двух отклонённых форм.
	afterRejected, independent, err := description.Inspect(interAU, fps)
	if err != nil || independent || afterRejected != actual {
		t.Fatalf("Отказ повредил состояние зависимых кадров: %+v, key=%v, %v", afterRejected, independent, err)
	}
}
