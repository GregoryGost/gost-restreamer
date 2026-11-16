package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// audioBindingProfile строит явный HEVC-рецепт с выбранным codec для проверок полного аудиоконтракта.
// t сообщает об ошибке фикстуры; буквальный эталон задаёт независимую видеоматрицу.
func audioBindingProfile(t *testing.T, codec core.Codec) core.GroupProfile {
	t.Helper()
	// p содержит самостоятельный профиль с точным CRF23.51 и явным LIVE transcode.
	p := literalHEVCHDRProfile()
	p.LiveAudioMode = core.LiveAudioTranscode
	p.Format.Audio = core.AudioFormat{Codec: codec, SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
	switch codec {
	case core.CodecOpus:
		p.Format.Audio.CodecProfile, p.Encoding.AudioEncoder, p.AudioEncoding.BitrateBPS = "celt", "libopus", 128000
	case core.CodecFLAC:
		p.Format.Audio.CodecProfile, p.Encoding.AudioEncoder, p.AudioEncoding.BitrateBPS = "s16", "flac", 0
	case core.CodecMP3:
		p.Format.Audio.CodecProfile, p.Encoding.AudioEncoder, p.AudioEncoding.BitrateBPS = "mpeg1-layer3", "libmp3lame", 128000
	case core.CodecAC3:
		p.Format.Audio.CodecProfile, p.Encoding.AudioEncoder, p.AudioEncoding.BitrateBPS = "ac3", "ac3", 192000
	default:
		t.Fatal("Неподдержанная тестовая привязка", codec)
	}
	return p
}

// TestAudioBindingsIdentityAndRoundTrip проверяет текущую полную форму, точный CRF и отдельную идентичность каждой привязки.
// t сверяет новый профиль с точной сохраняемой формой без потери значений.
func TestAudioBindingsIdentityAndRoundTrip(t *testing.T) {
	// seen отмечает уже полученные полные идентичности аудиорецептов.
	seen := map[string]bool{}
	// codec выбирает каждую независимо заданную аудиопривязку.
	for _, codec := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// p содержит проверяемый профиль; before защищает исходник от мутации валидатора.
		p := audioBindingProfile(t, codec)
		before := p
		// raw и err проверяют фактическую новую сохраняемую форму.
		raw, err := core.EncodeProfile(p)
		if err != nil || !bytes.Contains(raw, []byte(`"ValueHundredths":2351`)) {
			t.Fatal("Не сохранён полный профиль", string(raw), err)
		}
		// decoded получает тот же полный профиль через строгий читатель.
		decoded, err := core.DecodeProfile(raw)
		if err != nil || decoded != p || p != before {
			t.Fatal("Профиль изменился при проверке или чтении", decoded, err)
		}
		// key различает новые рецепты; canonical содержит точную сокращённую форму.
		key, canonical, err := core.ProfileIdentity(p)
		if err != nil || seen[key] || !bytes.Equal(canonical, raw) {
			t.Fatal("Идентичность не учитывает полный аудиорецепт", key, err)
		}
		seen[key] = true
		if codec != core.CodecFLAC {
			// copyAudio сохраняет выбранный видеорецепт и явно разрешает неизменённое LIVE-аудио.
			copyAudio := p
			copyAudio.LiveAudioMode = core.LiveAudioCopy
			if err := copyAudio.Validate(); err != nil { // err сообщает об отказе допустимого audio copy.
				t.Fatal(codec, err)
			}
			copyAudio.Mode = core.ModeCopy
			if err := copyAudio.Validate(); err != nil { // err сообщает об отказе допустимого HEVC/video copy.
				t.Fatal(codec, err)
			}
		}
	}
}

// TestAudioBindingsRejectUnsupportedTuples проверяет каждый компонент конечной новой привязки отдельно.
// t требует отказ без автоматической смены кодировщика, частоты, раскладки или LIVE-режима.
func TestAudioBindingsRejectUnsupportedTuples(t *testing.T) {
	// codec перебирает четыре независимых конечных аудиорецепта.
	for _, codec := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// field выбирает единственное нарушение в остальном допустимого профиля.
		for _, field := range []string{"profile", "rate", "channels", "layout", "encoder", "bitrate", "custom", "selector"} {
			// p содержит исходный допустимый профиль перед точечной мутацией.
			p := audioBindingProfile(t, codec)
			switch field {
			case "profile":
				p.Format.Audio.CodecProfile = "LC"
			case "rate":
				p.Format.Audio.SampleRateHz = 44100
			case "channels":
				p.Format.Audio.Channels = 1
			case "layout":
				p.Format.Audio.ChannelLayout = "mono"
			case "encoder":
				p.Encoding.AudioEncoder = "aac"
			case "bitrate":
				p.AudioEncoding.BitrateBPS++
			case "custom":
				p.Encoding.AudioCustom = "aac_coder=fast"
			case "selector":
				p.LiveAudioMode = ""
			}
			if !errors.Is(p.Validate(), core.ErrInvalidProfile) {
				t.Fatal("Неподдержанная привязка принята", codec, field)
			}
		}
	}
	// flac проверяет отдельный запрет LIVE copy даже при совместимом видеорежиме transcode.
	flac := audioBindingProfile(t, core.CodecFLAC)
	flac.LiveAudioMode = core.LiveAudioCopy
	if flac.Validate() == nil || (core.AudioEncoding{}).Validate() == nil {
		t.Fatal("FLAC copy либо общий нулевой битрейт принят")
	}
	// aac сохраняет прежний полный допуск многоканального AAC во всех текущих профилях.
	aac := literalHEVCHDRProfile()
	aac.Format.Audio.SampleRateHz, aac.Format.Audio.Channels, aac.Format.Audio.ChannelLayout = 44100, 6, "5.1"
	aac.Encoding.AudioCustom = "aac_coder=fast"
	if err := aac.Validate(); err != nil { // err сообщает о непредусмотренном сужении прежнего AAC.
		t.Fatal(err)
	}
}

// TestAudioProfileRequiresEveryStoredField проверяет полноту каждого объекта профиля и атомарность отказа.
// t удаляет либо обнуляет каждое материализованное поле, включая нули и false в неприменимых блоках.
func TestAudioProfileRequiresEveryStoredField(t *testing.T) {
	// p задаёт допустимый FLAC с законным нулём target; raw и err готовят полную форму хранения.
	p := audioBindingProfile(t, core.CodecFLAC)
	raw, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	// root содержит дерево исключительно тестового JSON для последовательных независимых мутаций.
	var root map[string]json.RawMessage
	if err = json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	// visit проверяет все поля object по path; вложенные вызовы сохраняют исходные родительские bytes.
	var visit func(map[string]json.RawMessage, []string)
	visit = func(object map[string]json.RawMessage, path []string) {
		// name и original обозначают ключ и его неизменное значение на этом уровне JSON.
		for name, original := range object {
			// remove отличает отсутствующий ключ от явного null.
			for _, remove := range []bool{true, false} {
				// damaged содержит самостоятельное дерево всей исходной формы; err сообщает о неверной фикстуре.
				var damaged map[string]any
				if err := json.Unmarshal(raw, &damaged); err != nil {
					t.Fatal(err)
				}
				// parent проходит объектный путь к единственному изменяемому ключу.
				parent := damaged
				// segment выбирает следующий родительский объект поля.
				for _, segment := range path {
					parent = parent[segment].(map[string]any)
				}
				if remove {
					delete(parent, name)
				} else {
					parent[name] = nil
				}
				// input и err получают одну повреждённую форму; receiver сохраняет прежнее значение при ошибке.
				input, err := json.Marshal(damaged)
				if err != nil {
					t.Fatal(err)
				}
				receiver := p
				if err = receiver.UnmarshalJSON(input); !errors.Is(err, core.ErrInvalidMetadata) || receiver != p {
					t.Fatal("Неполная форма принята", path, name, remove, err)
				}
			}
			// child распознаёт вложенный объект для проверки каждого его обязательного поля.
			var child map[string]json.RawMessage
			if len(original) > 0 && original[0] == '{' && json.Unmarshal(original, &child) == nil {
				visit(child, append(append([]string{}, path...), name))
			}
		}
	}
	visit(root, nil)
}

// TestAudioProfileRejectsForeignVersionsAndFields проверяет версии, неизвестные поля и числовой target0.
// t требует отказа без изменения корректного получателя полного профиля.
func TestAudioProfileRejectsForeignVersionsAndFields(t *testing.T) {
	// profile задаёт корректный FLAC-рецепт; raw и err содержат его явную сохраняемую форму.
	profile := audioBindingProfile(t, core.CodecFLAC)
	raw, err := core.EncodeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	// mutation содержит независимое нарушение root/encoding/format версии либо строгого состава полей.
	for _, mutation := range []struct {
		// before выбирает единственную исходную последовательность; after содержит недопустимую замену.
		before, after string
	}{
		{`"Mode":`, `"Version":7,"Mode":`}, {`"Encoding":{"QP"`, `"Encoding":{"Version":6,"QP"`},
		{`"Format":{"Version":1`, `"Format":{"Version":2`},
		{`"BitrateBPS":0`, `"BitrateBPS":"0"`},
		{`"AudioEncoder":"flac"`, `"AudioEncoder":"flac","Extra":false`},
		{`"SampleRateHz":48000`, `"SampleRateHz":48000,"BitsPerSample":16`},
	} {
		// input меняет ровно одну границу; receiver сохраняет исходное значение при отказе.
		input := bytes.Replace(raw, []byte(mutation.before), []byte(mutation.after), 1)
		receiver := profile
		if bytes.Equal(input, raw) {
			t.Fatal("Тестовая мутация не изменила JSON", mutation)
		}
		if err = receiver.UnmarshalJSON(input); !errors.Is(err, core.ErrInvalidMetadata) || receiver != profile {
			t.Fatal("Неверная форма принята", mutation, err)
		}
	}
}
