package core_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// modelContractProfile задаёт явный HEVC/FLAC-профиль без вызова конструктора начальных настроек.
// Сотые CRF, нулевой FLAC-битрейт и false позволяют заметить подмену сохранённых значений.
func modelContractProfile() core.GroupProfile {
	return core.GroupProfile{
		Mode: core.ModeTranscode, LiveAudioMode: core.LiveAudioTranscode,
		Format: core.MediaFormat{
			Video: core.VideoFormat{Codec: core.CodecHEVC, CodecProfile: "main10", Width: 640, Height: 360, FrameRate: core.FrameRate{Numerator: 60000, Denominator: 2002}, PixelFormat: "yuv420p10le", BitDepth: 10, Color: core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}},
			Audio: core.AudioFormat{Codec: core.CodecFLAC, CodecProfile: "s16", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"},
		},
		VideoEncoding: core.VideoEncoding{GOPFrames: 30, ClosedGOP: true},
		Encoding:      core.EncodingSettings{VideoEncoder: "libx265", AudioEncoder: "flac", RateControl: "crf", CRF: core.OptionalCRF{Set: true, ValueHundredths: 2351}},
	}
}

// modelContractObject разбирает data в дерево без преобразования точных чисел через float64.
// t сообщает ошибку фикстуры; возвращаемое дерево принадлежит только вызывающему тесту.
func modelContractObject(t *testing.T, data []byte) map[string]any {
	t.Helper()
	// decoder сохраняет исходные числовые лексемы; result принимает корневой объект.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var result map[string]any
	// err сообщает об ошибке разбора проверяемой полной формы.
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

// modelContractPaths перечисляет все поля object, включая объекты, с префиксом prefix.
// Каждый путь является самостоятельным кандидатом удаления/null в полной сохраняемой форме.
func modelContractPaths(object map[string]any, prefix string) []string {
	// paths накапливает независимые пути; name и value обходят текущий уровень объекта.
	var paths []string
	for name, value := range object {
		// path адресует поле текущего уровня с сохранением точного регистра.
		path := prefix + name
		paths = append(paths, path)
		// child и ok выявляют вложенный обязательный объект без предположений о его значениях.
		if child, ok := value.(map[string]any); ok {
			paths = append(paths, modelContractPaths(child, path+".")...)
		}
	}
	return paths
}

// TestProfileModelStoredShapeRequiresEveryMaterializedField проверяет полную форму целиком.
// t удаляет или обнуляет каждое поле, включая false/0/пустые неприменимые блоки;
// обработчик подтеста требует отказ без изменения ранее корректного получателя.
func TestProfileModelStoredShapeRequiresEveryMaterializedField(t *testing.T) {
	// profile задаёт независимый положительный контроль; raw и err фиксируют его сохранённую форму.
	profile := modelContractProfile()
	raw, err := core.EncodeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	// complete содержит все поля единственной формы, включая независимую версию Format.
	complete := modelContractObject(t, raw)
	if len(complete) != 6 || complete["Version"] != nil || complete["Encoding"].(map[string]any)["Version"] != nil || complete["Format"].(map[string]any)["Version"] != json.Number("1") {
		t.Fatalf("Неверная форма полного профиля: %s", raw)
	}
	// decoded подтверждает положительное чтение до проверки повреждений.
	decoded, err := core.DecodeProfile(raw)
	if err != nil || decoded != profile {
		t.Fatalf("Полный профиль изменился: %+v, %v", decoded, err)
	}
	// path выбирает обязательное поле, mutation — отсутствие либо явный null.
	for _, path := range modelContractPaths(complete, "") {
		for _, mutation := range []string{"missing", "null"} {
			// t сообщает об ошибках проверяемого сценария.
			t.Run(path+"/"+mutation, func(t *testing.T) {
				// object является отдельной копией; parts разбивает проверенный путь на имена полей.
				object := modelContractObject(t, raw)
				parts := strings.Split(path, ".")
				// parent указывает объект, непосредственно содержащий удаляемое поле.
				parent := object
				// part последовательно выбирает каждый промежуточный объект пути.
				for _, part := range parts[:len(parts)-1] {
					parent = parent[part].(map[string]any)
				}
				if mutation == "missing" {
					delete(parent, parts[len(parts)-1])
				} else {
					parent[parts[len(parts)-1]] = nil
				}
				// damaged кодирует ровно одно повреждение; err сообщает ошибку его подготовки.
				damaged, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				// receiver сохраняет прежний профиль даже при глубоком отказе декодирования.
				receiver := profile
				if err = receiver.UnmarshalJSON(damaged); !errors.Is(err, core.ErrInvalidMetadata) || receiver != profile {
					t.Fatalf("Неполная форма принята или изменила получателя: %v, %+v", err, receiver)
				}
			})
		}
	}
}

// TestProfileModelCanonicalIdentityOnlyReducesFPS проверяет равенство дробей и полную чувствительность ключа.
// t использует SHA256 от возвращённых байтов как независимый контракт ключа и проверяет отдельные изменения.
func TestProfileModelCanonicalIdentityOnlyReducesFPS(t *testing.T) {
	// profile и before фиксируют неизменяемые исходные значения; key, canonical и err — результат идентичности.
	profile := modelContractProfile()
	before := profile
	key, canonical, err := core.ProfileIdentity(profile)
	if err != nil || profile != before {
		t.Fatal("Канонизация изменила вход", err)
	}
	// digest независимо проверяет договорённый алгоритм ключа по полным байтам.
	digest := sha256.Sum256(canonical)
	if key != hex.EncodeToString(digest[:]) {
		t.Fatal("Ключ не является SHA256 полной формы")
	}
	// reduced отличается только эквивалентной записью FPS; same и sameBytes должны совпасть с исходными.
	reduced := profile
	reduced.Format.Video.FrameRate = core.FrameRate{Numerator: 30000, Denominator: 1001}
	same, sameBytes, err := core.ProfileIdentity(reduced)
	if err != nil || same != key || !bytes.Equal(sameBytes, canonical) {
		t.Fatal("Эквивалентный FPS изменил идентичность", err)
	}
	// change задаёт допустимое самостоятельное изменение, которое обязано изменить полный ключ.
	for _, change := range []string{"quality", "rescale", "range", "gop", "audio"} {
		// changed сохраняет все остальные параметры, чтобы обнаружить пропущенное поле canonical.
		changed := profile
		switch change {
		case "quality":
			changed.Encoding.CRF.ValueHundredths = 2352
		case "rescale":
			changed.Encoding.RescaleOutput = true
		case "range":
			changed.Format.Video.Color.Range = "full"
		case "gop":
			changed.VideoEncoding.GOPFrames = 60
		case "audio":
			changed.Format.Audio = core.AudioFormat{Codec: core.CodecOpus, CodecProfile: "celt", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
			changed.Encoding.AudioEncoder, changed.AudioEncoding.BitrateBPS = "libopus", 128000
		}
		// other и otherBytes проверяют, что изменение не потеряно ни в ключе, ни в полных байтах.
		// err сообщает об отказе вычисления идентичности изменённого профиля.
		other, otherBytes, err := core.ProfileIdentity(changed)
		if err != nil || other == key || bytes.Equal(otherBytes, canonical) {
			t.Fatalf("Поле %s потеряно в идентичности: %v", change, err)
		}
	}
}

// TestProfileModelActualGrammarDoesNotBecomeDesiredCatalog проверяет раздельные контракты формата и запроса.
// t сохраняет неизвестные непустые наблюдаемые параметры без превращения их в разрешённые желаемые настройки.
func TestProfileModelActualGrammarDoesNotBecomeDesiredCatalog(t *testing.T) {
	// profile содержит некаталогизированные, но структурно полные наблюдаемые параметры дорожек.
	profile := modelContractProfile()
	profile.Format.Video.CodecProfile, profile.Format.Video.PixelFormat = "observed-profile", "observed-pixels"
	profile.Format.Video.Color.Primaries = "unspecified"
	profile.Format.Audio.CodecProfile = "observed-audio-profile"
	// raw, err и actual проверяют независимый цикл метаданных формата без проверки желаемых настроек.
	raw, err := core.EncodeMediaFormat(profile.Format)
	if err != nil {
		t.Fatal("Структурный actual отвергнут каталогом desired", err)
	}
	actual, err := core.DecodeMediaFormat(raw)
	if err != nil || actual != profile.Format {
		t.Fatal("Фактические параметры изменены", actual, err)
	}
	if !errors.Is(profile.Validate(), core.ErrInvalidProfile) {
		t.Fatal("Наблюдаемые неизвестные строки стали желаемыми настройками")
	}
}
