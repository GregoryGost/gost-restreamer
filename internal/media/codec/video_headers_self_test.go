package codec

import (
	"bytes"
	"encoding/hex"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"testing"
)

// TestEncodedHEVCAndVP9 проверяет независимость первого AU, состояние межкадровых ссылок и фактическую геометрию.
// Литеральные сжатые кадры получены FFmpeg7.1.5 из 64×64 красного кадра; SEI пользовательских данных опущены.
func TestEncodedHEVCAndVP9(t *testing.T) {
	// fixtures содержит буквальные записи hvcC/VP9 и два последовательных исходных AU.
	fixtures := []struct {
		// kind задаёт фактический кодек, config — исходную запись заголовков.
		kind   core.Codec
		config string
		// first/second сохраняют сжатые key/inter AU в hex без пользовательского SEI.
		first, second string
	}{
		{core.CodecHEVC, "0101600000009000000000001ef000fcfdf8f800000f03200001001840010c01ffff01600000030090000003000003001eba0240210001002b42010101600000030090000003000003001ea020810596ea4932bc05a810100820000003002000000303c122000100074401c172b42240", "0000001840010c01ffff01600000030090000003000003001eba02400000002b42010101600000030090000003000003001ea020810596ea4932bc05a810100820000003002000000303c1000000074401c172b42240000000142801af0860f9eead5ffea9b7f2614b9762feb980", "0000000b0201d0097e10c610c0fb8c"},
		{core.CodecVP9, "000a820202010000", "824983424003f003f60038241c184a00003060000078b9ffff57b0ffffff7c17fffffffc78c1cfd2e880", "860040929c00500000036000005470"},
	}
	// fixture выбирает кодек, для которого проверяются оба вида кадра.
	for _, fixture := range fixtures {
		// headers/err содержат буквальную конфигурацию и возможную ошибку hex.
		headers, err := hex.DecodeString(fixture.config)
		if err != nil {
			t.Fatal(err)
		}
		// first/second содержат отдельные исходные payload для двух AU.
		first, _ := hex.DecodeString(fixture.first)
		second, _ := hex.DecodeString(fixture.second)
		// fps соответствует исходной точной частоте synthetic fixture.
		fps := core.FrameRate{Numerator: 30, Denominator: 1}
		if fixture.kind == core.CodecHEVC {
			// actual подтверждает VUI, профиль, глубину сжатого потока и геометрию.
			// parseErr проверяет успешный разбор фактического SPS/VUI.
			actual, parseErr := HEVCFormat(VideoDescription{Headers: headers})
			if parseErr != nil {
				t.Fatal("HEVC config", parseErr)
			}
			if actual.Width != 64 || actual.Height != 64 || !actual.FrameRate.Equal(fps) || actual.BitDepth != 8 {
				t.Fatal(actual)
			}
			// key/inspectErr устанавливают независимость по slice header.
			key, inspectErr := InspectHEVCAU(VideoDescription{Headers: headers}, first)
			if inspectErr != nil || !key {
				t.Fatal("HEVC first", key, inspectErr)
			}
			key, inspectErr = InspectHEVCAU(VideoDescription{Headers: headers}, second)
			if inspectErr != nil || key {
				t.Fatal("HEVC second", key, inspectErr)
			}
			// original сохраняет неполный исходный hvcC: массивы будут выведены лишь во временном состоянии разборщика.
			original := bytes.Clone(headers[:23])
			original[22] = 0
			// config хранит исходную запись без добавления выведенных VPS/SPS/PPS в заголовки транспорта.
			// configErr проверяет допустимость исходной неполной записи в конфигурации времени выполнения.
			config, configErr := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: actual}, original)
			if configErr != nil {
				t.Fatal(configErr)
			}
			// inspector сохраняет выведенный контекст между ключевым и межкадровым кадрами при неизменной исходной записи.
			var inspector Inspector
			// index и payload выбирают первый независимый и следующий межкадровый AU.
			for index, payload := range [][]byte{first, second} {
				// packet содержит исходное время и один неизменённый payload.
				// packetErr проверяет создание immutable пакета с исходным временем.
				packet, packetErr := media.NewPacket(media.PacketInfo{DTS: int64(index), PTS: int64(index), DurationTicks: 1, Keyframe: index == 0}, config, payload)
				if packetErr != nil {
					t.Fatal(packetErr)
				}
				key, inspectErr = inspector.Inspect(packet)
				if inspectErr != nil || key != (index == 0) || !bytes.Equal(packet.Config().Headers(), original) {
					t.Fatal("HEVC incomplete context", key, inspectErr)
				}
			}

		} else {
			// description сохраняет reference geometry между действительными VP9 AU.
			// parseErr проверяет допустимость буквальной записи VP9.
			description, parseErr := ParseVP9Config(headers)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			// actual/key/inspectErr содержат фактический формат и независимость первого показанного кадра.
			actual, key, inspectErr := description.Inspect(first, fps)
			if inspectErr != nil || !key || actual.Width != 64 || actual.Height != 64 {
				t.Fatal("VP9 first", actual, key, inspectErr)
			}
			actual, key, inspectErr = description.Inspect(second, fps)
			if inspectErr != nil || key || actual.Width != 64 {
				t.Fatal("VP9 inter", actual, key, inspectErr)
			}
			if _, key, inspectErr = description.Inspect([]byte{0x88}, fps); inspectErr == nil || key {
				t.Fatal("Принят show_existing_frame")
			}
		}
	}
}
