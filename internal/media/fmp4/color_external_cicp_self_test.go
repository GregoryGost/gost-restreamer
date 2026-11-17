package fmp4

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// TestHEVCExternalCICPContainerRoundTrip проходит мультиплексор, восстановители с UUID и без него, nclx и настоящий Inspector.
// t проверяет сохранение исходных hvcC/AU при разрешённом уточнении двух unspecified CICP при известной матрице.
func TestHEVCExternalCICPContainerRoundTrip(t *testing.T) {
	// headers содержит реальный hvcC с двумя unspecified CICP, payload — независимый IDR того же SPS.
	// err сообщает о повреждении буквальной шестнадцатеричной записи.
	headers, err := hex.DecodeString("0101600000009000000000001ef000fcfdf8f800000f03200001001840010c01ffff01600000030090000003000003001eba0240210001002b42010101600000030090000003000003001ea020810596ea4932bc05a810100820000003002000000303c122000100074401c172b42240")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := hex.DecodeString("0000001840010c01ffff01600000030090000003000003001eba02400000002b42010101600000030090000003000003001ea020810596ea4932bc05a810100820000003002000000303c1000000074401c172b42240000000142801af0860f9eead5ffea9b7f2614b9762feb980")
	if err != nil {
		t.Fatal(err)
	}
	// coded извлекает все фактические поля SPS, исключая отсутствующие цветовые индексы.
	coded, err := codec.HEVCFormat(codec.VideoDescription{Headers: headers})
	if err != nil || coded.Color.Primaries != "unspecified" || coded.Color.Transfer != "unspecified" || coded.Color.Matrix != "bt709" {
		t.Fatal("Неверная исходная фикстура", coded, err)
	}
	// init предоставляет неизменённую AAC-дорожку; effective содержит фактический внешний BT.709.
	init := testInit(t)
	init.Origin = OriginMapping{}
	effective := coded
	effective.Color = codec.CICPColor(1, 1, 1, false)
	// info связывает проверенный colorConfig с неизменяемым видео без дублирования значений в признаке происхождения.
	info := media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: init.Audio.Info().SourceEpoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: effective, HDR: media.HDRMetadata{ColorConfig: media.ColorConfigProvenance{Source: media.HDRSourceERTMP, Primaries: true, Transfer: true, Matrix: true}}}
	init.Video, err = media.NewCodecConfig(info, headers)
	if err != nil {
		t.Fatal(err)
	}
	if codec.ValidateHEVCConfig(headers, effective) == nil {
		t.Fatal("Прежняя строгая проверка ослаблена")
	}
	// wire принимает init и неизменённый сжатый IDR без внешнего процесса или сокета.
	var wire bytes.Buffer
	// mux проверяет исходный SPS с внешним цветом до первой записи контейнера.
	mux, err := NewMuxer(&wire, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	// original сохраняет точное время и исходную нагрузку до записи и повторного чтения.
	original := packet(t, init.Video, media.PacketInfo{DurationTicks: 1, Keyframe: true}, payload)
	if err = mux.WritePacket(original); err != nil {
		t.Fatal(err)
	}
	// withUUID выбирает оба реальных пути восстановления фактического формата: сохранённый UUID и исходный SPS/nclx.
	for _, withUUID := range []bool{true, false} {
		// raw содержит тот же контейнер, из которого только во втором сценарии удаляется UUID.
		raw := wire.Bytes()
		if !withUUID {
			raw = withoutConfigUUID(t, raw)
		}
		// demux восстанавливает происхождение из nclx, а не из JSON; readErr сохраняет отказ контейнерного чтения.
		demux, readErr := NewDemuxer(bytes.NewReader(raw), DemuxOptions{Limits: testLimits(), SourceEpoch: info.SourceEpoch, VideoRevision: 1, AudioRevision: 1})
		if readErr != nil {
			t.Fatal(withUUID, readErr)
		}
		if demux.Init().Video.Info().Video != effective || demux.Init().Video.Info().HDR.ColorConfig.Source != media.HDRSourceContainer {
			t.Fatal("Потерян фактический внешний цвет", demux.Init().Video.Info())
		}
		// description подтверждает происхождение реального nclx для последующего согласования собственного свежего результата.
		for _, description := range demux.Descriptions() {
			if description.Track == media.TrackVideo && description.HDR.ColorConfig != demux.Init().Video.Info().HDR.ColorConfig {
				t.Fatal("Descriptions потерял происхождение nclx")
			}
		}
		// restored проходит внутренний HEVC Inspector с сохранённым состоянием и сохраняет исходную запись/нагрузку.
		restored, readErr := demux.Next()
		if readErr != nil || !bytes.Equal(restored.Config().Headers(), headers) || !bytes.Equal(restored.Payload(), payload) || restored.Info() != original.Info() {
			t.Fatal(withUUID, restored.Info(), readErr)
		}
		// reopened проверяет повторную запись восстановленного снимка без UUID-расширения.
		var reopened bytes.Buffer
		if _, readErr = NewMuxer(&reopened, demux.Init(), testLimits()); readErr != nil {
			t.Fatal(readErr)
		}
		if _, readErr = NewDemuxer(bytes.NewReader(reopened.Bytes()), DemuxOptions{Limits: testLimits()}); readErr != nil {
			t.Fatal(readErr)
		}
	}
	// missing удаляет только доказательство цвета: известные внешние значения сами по себе не являются данными заголовка кодека.
	missing := info
	missing.HDR.ColorConfig = media.ColorConfigProvenance{}
	init.Video, err = media.NewCodecConfig(missing, headers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewMuxer(&bytes.Buffer{}, init, testLimits()); err == nil {
		t.Fatal("Mux принял желаемый цвет без реального внешнего дескриптора")
	}
	// wrongRange сохраняет корректное происхождение CICP, но противоречит обязательному диапазону сигнала заголовка кодека.
	wrongRange := info
	wrongRange.Video.Color.Range = "full"
	init.Video, err = media.NewCodecConfig(wrongRange, headers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewMuxer(&bytes.Buffer{}, init, testLimits()); err == nil {
		t.Fatal("nclx разрешил изменить кодированный диапазон")
	}
	// wrongMatrix противоречит действительно известной матрице исходного SPS.
	wrongMatrix := info
	wrongMatrix.Video.Color.Matrix = "bt2020nc"
	init.Video, err = media.NewCodecConfig(wrongMatrix, headers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewMuxer(&bytes.Buffer{}, init, testLimits()); err == nil {
		t.Fatal("nclx разрешил заменить известную кодированную матрицу")
	}
	// stale не имеет нового nclx; прежнее происхождение не должно переживать реконструкцию.
	stale, err := reconstructColor(original.Config(), TrackDescription{Track: media.TrackVideo, Codec: core.CodecHEVC, Headers: headers})
	if err != nil || stale.Info().HDR.ColorConfig != (media.ColorConfigProvenance{}) {
		t.Fatal(stale.Info(), err)
	}
}
