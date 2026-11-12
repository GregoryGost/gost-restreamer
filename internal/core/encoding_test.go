package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Создаёт допустимый полный профиль выбранного кодировщика и CBR/VBR, согласуя кодек, режим, битрейт и
// буфер.
func encodedProfile(encoder, rc string) core.GroupProfile {
	// p содержит отдельный полный профиль текущего сценария проверки.
	p := validProfile()
	p.Encoding = core.DefaultEncoding(p.Mode, encoder, rc)
	if encoder == core.VideoEncoderNVENC {
		p.Mode = core.ModeTranscode
		p.Format.Video.Codec = core.CodecH264
		p.Format.Video.CodecProfile = "high"
		p.Encoding = core.DefaultEncoding(p.Mode, encoder, rc)
	}
	if rc == core.RateControlCBR {
		p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
	}
	if encoder == core.VideoEncoderSVTAV1 && rc == core.RateControlVBR {
		p.VideoEncoding.MaxBitrateBPS = 0
		p.VideoEncoding.BufferBits = 0
	}
	return p
}

// Проверяет исходные настройки кодировщиков, сохранение профиля v2 и одинаковые ключи сокращённых FPS
// без изменения входа.
func TestEncodingDefaultsAndImmutableRoundTrip(t *testing.T) {
	// encoder выбирает проверяемый видеокодировщик.
	for _, encoder := range []string{core.VideoEncoderNVENC, core.VideoEncoderSVTAV1} {
		// rc выбирает проверяемый режим видеобитрейта CBR или VBR.
		for _, rc := range []string{core.RateControlCBR, core.RateControlVBR} {
			t.Run(encoder+"/"+rc, func(t *testing.T) {
				// Проверяет точное сохранение настроек выбранного кодировщика и режима, версию 2 и одинаковую
				// идентичность равных дробей FPS без изменения исходника.

				// p содержит отдельный полный профиль текущего сценария проверки.
				p := encodedProfile(encoder, rc)
				// err сообщает об ошибке проверки допустимости профиля.
				if err := p.Validate(); err != nil {
					t.Fatal(err)
				}
				// before сохраняет исходный профиль для проверки отсутствия изменения.
				before := p
				p.Format.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: 2002}
				// data содержит байты JSON-профиля для чтения и сравнения. err сообщает
				// об ошибке кодирования проверенного профиля.
				data, err := core.EncodeProfile(p)
				if err != nil {
					t.Fatal(err)
				}
				// got содержит профиль, восстановленный из проверяемого представления.
				got, err := core.DecodeProfile(data)
				if err != nil || got != p {
					t.Fatalf("Профиль изменился после хранения: %v", err)
				}
				// header принимает версию сохранённого профиля для проверки независимых
				// версий.
				var header struct { /* Version содержит версию сохраняемого представления профиля. */
					Version int
				}
				// err сообщает об ошибке разбора JSON проверяемого представления.
				if err := json.Unmarshal(data, &header); err != nil || header.Version != 2 || core.MetadataVersion != 1 {
					t.Fatalf("Версии форматов изменены: %s", data)
				}
				// key содержит вычисленный ключ полного профиля. canonical содержит
				// канонические байты полного профиля.
				key, canonical, err := core.ProfileIdentity(p)
				if err != nil {
					t.Fatal(err)
				}
				// reduced содержит копию профиля с сокращённой эквивалентной частотой
				// кадров.
				reduced := p
				reduced.Format.Video.FrameRate = core.FrameRate{Numerator: 30000, Denominator: 1001}
				// otherKey содержит ключ изменённого полного профиля. otherCanonical
				// содержит канонические байты изменённого полного профиля.
				otherKey, otherCanonical, err := core.ProfileIdentity(reduced)
				if err != nil || key != otherKey || !bytes.Equal(canonical, otherCanonical) || p.Format.Video.FrameRate.Denominator != 2002 {
					t.Fatal("Канонизация FPS изменила профиль или идентичность")
				}
				if before.Encoding.VideoEncoder == core.VideoEncoderSVTAV1 && rc == core.RateControlVBR && before.Encoding.SVTAV1.LookaheadFrames != -1 {
					t.Fatal("VBR требует auto lookahead")
				}
			})
		}
	}
}

// Проверяет отказ неподдерживаемых сочетаний кодирования с типизированным путём поля и без изменения
// профиля.
func TestEncodingValidationRejectsUnsupportedRequests(t *testing.T) {
	// cases перечисляет сценарии проверки допустимости или различимости полей профиля.
	cases := []struct {
		// name задаёт имя текущего сценария проверки поля профиля. encoder выбирает проверяемый
		// видеокодировщик. rc выбирает проверяемый режим видеобитрейта CBR или VBR.
		name, encoder, rc string
		// change вносит выбранное изменение параметра в копию исходного профиля.
		change func(*core.GroupProfile)
	}{
		{"partial_legacy", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Обнуляет Encoding.Version при заполненных настройках, проверяя отказ для неполного блока
			// кодирования.
			p.Encoding.Version = 0
		}},
		{"unknown_version", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт неподдерживаемую Encoding.Version 3.
			p.Encoding.Version = 3
		}},
		{"mode", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неизвестный режим other.
			p.Mode = "other"
		}},
		{"rc", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемый режим управления битрейтом cq.
			p.Encoding.RateControl = "cq"
		}},
		{"audio_encoder", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемый аудиокодировщик opus.
			p.Encoding.AudioEncoder = "opus"
		}},
		{"video_encoder", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемый видеокодировщик libx264.
			p.Encoding.VideoEncoder = "libx264"
		}},
		{"codec", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт AV1 для настроек NVENC, проверяя несовместимость кодека и кодировщика.
			p.Format.Video.Codec = core.CodecAV1
		}},
		{"dimensions_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Обнуляет ширину кадра.
			p.Format.Video.Width = 0
		}},
		{"dimensions_odd", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт нечётную высоту 1079.
			p.Format.Video.Height = 1079
		}},
		{"h264_dimensions", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт ширину H.264 65536 за допустимой границей.
			p.Format.Video.Width = 65536
		}},
		{"pixel", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемый PixelFormat yuv444p.
			p.Format.Video.PixelFormat = "yuv444p"
		}},
		{"bit_depth", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт неподдерживаемую глубину 10 бит.
			p.Format.Video.BitDepth = 10
		}},
		{"color", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемую функцию переноса smpte2084.
			p.Format.Video.Color.Transfer = "smpte2084"
		}},
		{"fps_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Обнуляет знаменатель FPS.
			p.Format.Video.FrameRate.Denominator = 0
		}},
		{"fps_range", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт числитель FPS math.MaxUint32 за допустимой границей.
			p.Format.Video.FrameRate.Numerator = math.MaxUint32
		}},
		{"fps_scale", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт числитель FPS 65521 для проверки ограничения временной шкалы.
			p.Format.Video.FrameRate.Numerator = 65521
		}},
		{"open_gop", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Отключает обязательный закрытый GOP.
			p.VideoEncoding.ClosedGOP = false
		}},
		{"gop_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Обнуляет длину GOP.
			p.VideoEncoding.GOPFrames = 0
		}},
		{"gop_overflow", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт длину GOP math.MaxUint32 за допустимой границей.
			p.VideoEncoding.GOPFrames = math.MaxUint32
		}},
		{"b_equal_gop", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Приравнивает число B-кадров к длине GOP, создавая недопустимое сочетание.
			p.VideoEncoding.GOPFrames = 2
			p.VideoEncoding.MaxBFrames = 2
		}},
		{"b_limit", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт пять B-кадров сверх допустимого предела.
			p.VideoEncoding.MaxBFrames = 5
		}},
		{"baseline_b", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Добавляет B-кадр к профилю baseline.
			p.Format.Video.CodecProfile = "baseline"
			p.VideoEncoding.MaxBFrames = 1
		}},
		{"profile", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неизвестный профиль H.264 future.
			p.Format.Video.CodecProfile = "future"
		}},
		{"tier", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт неподдерживаемый Tier main для H.264.
			p.Format.Video.Tier = "main"
		}},
		{"level", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт неподдерживаемый Level 6.0 для H.264.
			p.Format.Video.Level = "6.0"
		}},
		{"target_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Обнуляет целевой видеобитрейт.
			p.VideoEncoding.TargetBitrateBPS = 0
		}},
		{"target_gt_max", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Поднимает целевой битрейт выше максимального.
			p.VideoEncoding.TargetBitrateBPS = p.VideoEncoding.MaxBitrateBPS + 1
		}},
		{"bitrate_overflow", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Поднимает максимальный битрейт выше math.MaxUint32.
			p.VideoEncoding.MaxBitrateBPS = math.MaxUint32 + 1
		}},
		{"buffer_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Обнуляет обязательный видеобуфер NVENC.
			p.VideoEncoding.BufferBits = 0
		}},
		{"buffer_overflow", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт размер видеобуфера math.MaxUint64 за допустимой границей.
			p.VideoEncoding.BufferBits = math.MaxUint64
		}},
		{"cbr_mismatch", core.VideoEncoderNVENC, "cbr", func(p *core.GroupProfile) {
			// Нарушает равенство целевого и максимального битрейта CBR.
			p.VideoEncoding.MaxBitrateBPS++
		}},
		{"foreign_svt", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Добавляет настройки SVTAV1 к профилю NVENC.
			p.Encoding.SVTAV1.Preset = 8
		}},
		{"preset", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемый пресет NVENC p8.
			p.Encoding.NVENC.Preset = "p8"
		}},
		{"tuning", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемое значение Tuning lossless.
			p.Encoding.NVENC.Tuning = "lossless"
		}},
		{"multipass", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неизвестный режим Multipass future.
			p.Encoding.NVENC.Multipass = "future"
		}},
		{"lookahead_negative", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт отрицательный LookaheadFrames для NVENC.
			p.Encoding.NVENC.LookaheadFrames = -1
		}},
		{"lookahead_sum", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Доводит сумму LookaheadFrames и B-кадров до недопустимой границы 32.
			p.Encoding.NVENC.LookaheadFrames = 31
			p.VideoEncoding.MaxBFrames = 1
		}},
		{"aq_strength", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт AQStrength 16 при включённом пространственном AQ.
			p.Encoding.NVENC.EnableSpatialAQ = true
			p.Encoding.NVENC.AQStrength = 16
		}},
		{"aq_disabled_strength", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт AQStrength 7 при выключенном пространственном AQ.
			p.Encoding.NVENC.AQStrength = 7
		}},
		{"aac_codec", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет opus вместо обязательного AAC.
			p.Format.Audio.Codec = "opus"
		}},
		{"aac_profile", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемый аудиопрофиль HE.
			p.Format.Audio.CodecProfile = "HE"
		}},
		{"aac_rate", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Поднимает частоту AAC до неподдерживаемых 96000 Гц.
			p.Format.Audio.SampleRateHz = 96000
		}},
		{"aac_layout", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Задаёт mono при неизменном числе каналов исходного профиля.
			p.Format.Audio.ChannelLayout = "mono"
		}},
		{"aac_zero", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Обнуляет аудиобитрейт.
			p.AudioEncoding.BitrateBPS = 0
		}},
		{"aac_clamp", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Поднимает аудиобитрейт на единицу выше 576000.
			p.AudioEncoding.BitrateBPS = 576001
		}},
		{"custom_not_normalized", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Подставляет ненормализованную строку VideoCustom с ведущим нулём и несортированными ключами.
			p.Encoding.VideoCustom = "strict_gop=01 nonref_p=0"
		}},
		{"copy_rescale", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Включает RescaleOutput для режима копирования с SVTAV1.
			p.Encoding.RescaleOutput = true
		}},
		{"foreign_nvenc", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Добавляет настройки NVENC к профилю SVTAV1.
			p.Encoding.NVENC.Preset = "p4"
		}},
		{"svt_small", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Опускает ширину SVTAV1 до 62, ниже допустимого минимума.
			p.Format.Video.Width = 62
		}},
		{"svt_odd", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Задаёт нечётную ширину SVTAV1 65.
			p.Format.Video.Width = 65
		}},
		{"svt_large", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Поднимает высоту SVTAV1 до 8706, выше допустимого максимума.
			p.Format.Video.Height = 8706
		}},
		{"svt_fps", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Поднимает частоту SVTAV1 до 241 кадра в секунду.
			p.Format.Video.FrameRate.Numerator = 241
		}},
		{"svt_profile", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемый профиль AV1 high.
			p.Format.Video.CodecProfile = "high"
		}},
		{"svt_level", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемый Level AV1 7.4.
			p.Format.Video.Level = "7.4"
		}},
		{"svt_tier", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Подставляет неподдерживаемый Tier AV1 high.
			p.Format.Video.Tier = "high"
		}},
		{"svt_gop", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Уменьшает GOP SVTAV1 до одного кадра.
			p.VideoEncoding.GOPFrames = 1
		}},
		{"svt_b", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Добавляет B-кадр к профилю SVTAV1.
			p.VideoEncoding.MaxBFrames = 1
		}},
		{"svt_bitrate_zero", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Обнуляет целевой битрейт SVTAV1.
			p.VideoEncoding.TargetBitrateBPS = 0
		}},
		{"svt_bitrate_high", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Поднимает целевой битрейт SVTAV1 на единицу выше 100000000.
			p.VideoEncoding.TargetBitrateBPS = 100000001
		}},
		{"svt_vbr_cap", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Задаёт запрещённый максимальный битрейт для VBR SVTAV1.
			p.VideoEncoding.MaxBitrateBPS = 6000000
		}},
		{"svt_vbr_buffer", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Задаёт запрещённый буфер для VBR SVTAV1.
			p.VideoEncoding.BufferBits = 1
		}},
		{"svt_cbr_nominal", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			// Нарушает равенство целевого и максимального битрейта CBR SVTAV1.
			p.VideoEncoding.MaxBitrateBPS++
		}},
		{"svt_buffer_low", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			// Уменьшает буфер CBR SVTAV1 до недопустимых 60000 бит.
			p.VideoEncoding.BufferBits = 60000
		}},
		{"svt_buffer_fractional", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			// Нарушает целочисленность длительности буфера CBR SVTAV1 добавлением одного бита.
			p.VideoEncoding.BufferBits++
		}},
		{"svt_buffer_overflow", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			// Задаёт переполняющий размер буфера CBR SVTAV1 math.MaxUint64.
			p.VideoEncoding.BufferBits = math.MaxUint64
		}},
		{"svt_preset", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Подставляет пресет SVTAV1 14 за допустимой границей.
			p.Encoding.SVTAV1.Preset = 14
		}},
		{"svt_cbr_vq", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			// Задаёт неподдерживаемое Tuning vq для CBR SVTAV1.
			p.Encoding.SVTAV1.Tuning = "vq"
		}},
		{"svt_aq", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Обнуляет обязательный AQMode SVTAV1.
			p.Encoding.SVTAV1.AQMode = 0
		}},
		{"svt_cbr_lookahead", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			// Задаёт автоматический LookaheadFrames -1 для CBR SVTAV1.
			p.Encoding.SVTAV1.LookaheadFrames = -1
		}},
		{"svt_vbr_lookahead_low", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Уменьшает LookaheadFrames VBR SVTAV1 до 5, ниже допустимого минимума.
			p.Encoding.SVTAV1.LookaheadFrames = 5
		}},
		{"svt_vbr_lookahead_high", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Поднимает LookaheadFrames VBR SVTAV1 до 121, выше допустимого максимума.
			p.Encoding.SVTAV1.LookaheadFrames = 121
		}},
	}
	// tc содержит параметры и ожидаемый результат текущего сценария.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет типизированный ErrInvalidProfile для выбранного нарушения, наличие пути и описания поля,
			// отсутствие мутации и неравенство некорректного профиля самому себе.

			// p содержит отдельный полный профиль текущего сценария проверки.
			p := encodedProfile(tc.encoder, tc.rc)
			tc.change(&p)
			// before сохраняет исходный профиль для проверки отсутствия изменения.
			before := p
			// err сообщает об ошибке проверки допустимости профиля.
			err := p.Validate()
			// profileErr принимает типизированный отказ профиля с публичным полем и
			// описанием.
			var profileErr *core.ProfileError
			if !errors.Is(err, core.ErrInvalidProfile) || !errors.As(err, &profileErr) || profileErr.Code() == "" || !strings.HasPrefix(profileErr.Field(), "profile.") || profileErr.Detail() == "" {
				t.Fatalf("Профиль принят или потерял типизированную ошибку: %v", err)
			}
			if p != before || p.Equal(p) {
				t.Fatal("Валидация изменила профиль либо некорректное значение равно себе")
			}
		})
	}
}

// Проверяет принятие выбранных нижних и верхних границ NVENC, SVT-AV1, буфера, LookaheadFrames и
// битрейта AAC.
func TestEncodingAcceptsExactBoundaries(t *testing.T) {
	// cases перечисляет сценарии проверки допустимости или различимости полей профиля.
	cases := []struct {
		// name задаёт имя текущего сценария проверки поля профиля. encoder выбирает проверяемый
		// видеокодировщик. rc выбирает проверяемый режим видеобитрейта CBR или VBR.
		name, encoder, rc string
		// change вносит выбранное изменение параметра в копию исходного профиля.
		change func(*core.GroupProfile)
	}{
		{"h264_min", core.VideoEncoderNVENC, "cbr", func(p *core.GroupProfile) {
			// Создаёт минимальные допустимые размеры H.264, единичные битрейты, буфер и GOP.

			p.Format.Video.Width = 2
			p.Format.Video.Height = 2
			p.VideoEncoding = core.VideoEncoding{TargetBitrateBPS: 1, MaxBitrateBPS: 1, BufferBits: 1, GOPFrames: 1, ClosedGOP: true}
			p.AudioEncoding.BitrateBPS = 1
		}},
		{"h264_max", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Выставляет максимальные проверяемые размеры, битрейты, буфер, GOP и AQ NVENC, сохраняя допустимую
			// сумму предсказания и B-кадров.

			p.Format.Video.Width = 65534
			p.Format.Video.Height = 65534
			p.VideoEncoding.TargetBitrateBPS = math.MaxUint32
			p.VideoEncoding.MaxBitrateBPS = math.MaxUint32
			p.VideoEncoding.BufferBits = math.MaxInt32
			p.VideoEncoding.GOPFrames = math.MaxInt32
			p.VideoEncoding.MaxBFrames = 4
			p.Encoding.NVENC.LookaheadFrames = 27
			p.Encoding.NVENC.EnableSpatialAQ = true
			p.Encoding.NVENC.AQStrength = 15
		}},
		{"vbr_equal", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Приравнивает максимальный битрейт к целевому в допустимом VBR NVENC.
			p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
		}},
		{"svt_min", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			// Выставляет минимальные размеры, GOP, длительность буфера и пресет SVTAV1.

			p.Format.Video.Width = 64
			p.Format.Video.Height = 64
			p.VideoEncoding.GOPFrames = 2
			p.VideoEncoding.BufferBits = p.VideoEncoding.TargetBitrateBPS / 50
			p.Encoding.SVTAV1.Preset = 0
		}},
		{"svt_max", core.VideoEncoderSVTAV1, "cbr", func(p *core.GroupProfile) {
			// Выставляет максимальные размеры, FPS, битрейт, буфер и пресет SVTAV1.

			p.Format.Video.Width = 16384
			p.Format.Video.Height = 8704
			p.Format.Video.FrameRate.Numerator = 240
			p.VideoEncoding.TargetBitrateBPS = 100000000
			p.VideoEncoding.MaxBitrateBPS = 100000000
			p.VideoEncoding.BufferBits = 1000000000
			p.Encoding.SVTAV1.Preset = 13
		}},
		{"svt_lookahead_low", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Выставляет нижнюю границу LookaheadFrames 6 и допустимое Tuning vq для VBR SVTAV1.
			p.Encoding.SVTAV1.LookaheadFrames = 6
			p.Encoding.SVTAV1.Tuning = "vq"
		}},
		{"svt_lookahead_high", core.VideoEncoderSVTAV1, "vbr", func(p *core.GroupProfile) {
			// Выставляет верхнюю границу LookaheadFrames 120 для VBR SVTAV1.
			p.Encoding.SVTAV1.LookaheadFrames = 120
		}},
		{"aac_max", core.VideoEncoderNVENC, "vbr", func(p *core.GroupProfile) {
			// Выставляет допустимый максимальный аудиобитрейт 576000.
			p.AudioEncoding.BitrateBPS = 576000
		}},
	}
	// tc содержит параметры и ожидаемый результат текущего сценария.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет принятие выбранного граничного сочетания параметров кодирования.

			// p содержит отдельный полный профиль текущего сценария проверки.
			p := encodedProfile(tc.encoder, tc.rc)
			tc.change(&p)
			// err сообщает об ошибке проверки допустимости профиля.
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Проверяет границу представимой длительности кадра и шкал FFmpeg для обоих кодировщиков без изменения
// исходной дроби.
func TestEncodingFrameRatePreparedDurationBounds(t *testing.T) {
	// tc содержит параметры и ожидаемый результат текущего сценария.
	for _, tc := range []struct {
		// name задаёт имя текущего сценария проверки поля профиля.
		name string
		// rate задаёт граничную точную дробь FPS для проверки шкал FFmpeg.
		rate core.FrameRate
		// valid задаёт ожидаемую представимость частоты и длительности кадра.
		valid bool
	}{
		{"last_low_step", core.FrameRate{Numerator: 1, Denominator: 44739}, true},
		{"first_overflowing_low_step", core.FrameRate{Numerator: 1, Denominator: 44740}, false},
		{"unreduced_overflowing_low_step", core.FrameRate{Numerator: 2000, Denominator: 89480000}, false},
		{"high_numerator", core.FrameRate{Numerator: 32749, Denominator: 65536}, true},
		{"high_numerator_large_denominator", core.FrameRate{Numerator: 32749, Denominator: 131072}, true},
		{"native_scale_threshold", core.FrameRate{Numerator: 1000, Denominator: 44741}, true},
	} {
		// encoder выбирает проверяемый видеокодировщик.
		for _, encoder := range []string{core.VideoEncoderNVENC, core.VideoEncoderSVTAV1} {
			t.Run(tc.name+"/"+encoder, func(t *testing.T) {
				// Проверяет для обоих кодировщиков границу представимости длительности кадра, точный путь ошибки и
				// сохранность исходного профиля.

				// p содержит отдельный полный профиль текущего сценария проверки.
				p := encodedProfile(encoder, core.RateControlVBR)
				p.Format.Video.FrameRate = tc.rate
				p.Format.Audio.SampleRateHz = 48000
				// before сохраняет исходный профиль для проверки отсутствия изменения.
				before := p
				// err сообщает об ошибке проверки допустимости профиля.
				err := p.Validate()
				if tc.valid {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					// typed принимает типизированную ошибку профиля для проверки
					// кода, поля и объяснения.
					var typed *core.ProfileError
					if !errors.Is(err, core.ErrInvalidProfile) || !errors.As(err, &typed) || typed.Field() != "profile.format.video.frame_rate" {
						t.Fatalf("Непредставимая длительность кадра принята или потеряла путь поля: %v", err)
					}
				}
				if p != before {
					t.Fatal("Проверка длительности изменила профиль")
				}
			})
		}
	}
}

// Проверяет, что новое ограничение длительности кадра не меняет допустимость и точные байты прежнего
// профиля без Encoding.
func TestLegacyProfilePreservesLongFrameDuration(t *testing.T) {
	// p содержит отдельный полный профиль текущего сценария проверки.
	p := encodedProfile(core.VideoEncoderNVENC, core.RateControlVBR)
	p.Encoding = core.EncodingSettings{}
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 1, Denominator: 44740}
	// err сообщает об ошибке проверки допустимости профиля.
	if err := p.Validate(); err != nil {
		t.Fatalf("Прежняя допустимость профиля изменилась: %v", err)
	}
	// raw содержит сохраняемое представление проверяемого профиля. err сообщает об ошибке
	// кодирования проверенного профиля.
	raw, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	// got содержит профиль, восстановленный из проверяемого представления.
	got, err := core.DecodeProfile(raw)
	if err != nil || got != p {
		t.Fatalf("Прежний профиль изменился после хранения: %v", err)
	}
	// again содержит повторно полученное представление для проверки идемпотентности.
	again, err := core.EncodeProfile(got)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("Байты прежнего профиля изменились: %v", err)
	}
}

// Проверяет идемпотентную нормализацию разрешённых параметров, категории конфликтов и отказов и
// отсутствие чувствительного значения в объяснении.
func TestCustomNormalizationAndSafeErrors(t *testing.T) {
	// tc содержит параметры и ожидаемый результат текущего сценария.
	for _, tc := range []struct { /* encoder выбирает каталог параметров кодировщика. input содержит исходную строку дополнительных параметров. want задаёт ожидаемую каноническую строку. */
		encoder, input, want string
	}{
		{"h264_nvenc", " strict_gop=+01  nonref_p=00 ", "nonref_p=0 strict_gop=1"},
		{"libsvtav1", "lp=0006", "lp=6"},
		{"aac", "aac_coder=fast", "aac_coder=fast"},
		{"aac", "aac_coder=twoloop", "aac_coder=twoloop"},
		{"h264_nvenc", "   ", ""},
	} {
		// got содержит нормализованную строку дополнительных параметров. err сообщает об ошибке
		// нормализации пользовательских параметров кодировщика.
		got, err := core.NormalizeCustom(tc.encoder, tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("Нормализация %q: %q, %v", tc.input, got, err)
		}
		// again содержит результат повторной нормализации уже канонической строки.
		again, err := core.NormalizeCustom(tc.encoder, got)
		if err != nil || got != again {
			t.Fatal("Нормализация не идемпотентна")
		}
	}
	// inputs перечисляет повреждённые или недопустимые входные представления.
	inputs := []string{"lp=1", "nonref_p=2", "nonref_p=-1", "nonref_p=1 nonref_p=1", "nonref_p=1=1", "nonref_p=", "=1", "nonref_p=1\tstrict_gop=0", "nonref_p='1'", "nonref_p=1\\", "nonref_p=[1]", "nonref_p=99999999999999999999999999999999999", "future=private-sensitive-value", "nonref_p=1\n", "é=1", "nonref_p=1;", "not/a/name=1", strings.Repeat("x", 4097), strings.Repeat("x", 65) + "=1", "x=" + strings.Repeat("1", 129)}
	// input содержит очередной проверяемый вход выбранного сценария.
	for _, input := range inputs {
		// err сообщает об ошибке нормализации пользовательских параметров кодировщика.
		_, err := core.NormalizeCustom(core.VideoEncoderNVENC, input)
		// typed принимает типизированную ошибку профиля для проверки кода, поля и объяснения.
		var typed *core.ProfileError
		if !errors.As(err, &typed) || typed.Detail() == "" || strings.Contains(typed.Detail(), "private-sensitive-value") {
			t.Fatalf("Некорректные custom приняты либо раскрыты: %v", err)
		}
	}
	// encoder выбирает проверяемый видеокодировщик.
	for _, encoder := range []string{core.VideoEncoderSVTAV1, core.AudioEncoderAAC} {
		// err сообщает об ошибке нормализации пользовательских параметров кодировщика.
		_, err := core.NormalizeCustom(encoder, "nonref_p=1")
		// typed принимает типизированную ошибку профиля для проверки кода, поля и объяснения.
		var typed *core.ProfileError
		if !errors.As(err, &typed) || typed.Code() != "unknown_custom" {
			t.Fatal("Чужой каталог параметров принят")
		}
	}
	// value содержит целочисленное значение lp за разрешёнными границами.
	for _, value := range []string{"lp=7", "lp=-1"} {
		// err сообщает об ошибке нормализации пользовательских параметров кодировщика.
		if _, err := core.NormalizeCustom(core.VideoEncoderSVTAV1, value); err == nil {
			t.Fatal("Граница lp не проверена")
		}
	}
	// err сообщает об ошибке нормализации пользовательских параметров кодировщика.
	if _, err := core.NormalizeCustom(core.AudioEncoderAAC, "aac_coder=anmr"); err == nil {
		t.Fatal("Неподдержанный AAC coder принят")
	}
	// err сообщает об ошибке нормализации пользовательских параметров кодировщика.
	if _, err := core.NormalizeCustom("future", ""); err == nil {
		t.Fatal("Неизвестный encoder принят")
	}
	// name содержит имя типизированной или внутренней настройки, запрещённой в custom.
	for _, name := range []string{"rate_control", "rc", "b:v", "maxrate", "max_bitrate_bps", "vbv-bufsize", "averageBitRate", "vbvBufferSize", "enableLookahead", "enableAQ", "enableTemporalAQ", "aqStrength", "g", "keyint", "intra-period-length", "bf", "preset", "tune", "multipass", "rc-lookahead", "lookahead_frames", "spatial-aq", "temporal_aq", "aq-mode", "pred-struct", "hierarchical-levels", "enable-overlays", "enable-dg", "enable-tf", "scd", "profile:v", "level", "tier", "width", "pix_fmt", "fps", "color_primaries", "ar:a", "channel_layout", "i", "output", "filter_complex", "path", "pass", "stats", "svtav1-params"} {
		// err сообщает об ошибке нормализации пользовательских параметров кодировщика.
		_, err := core.NormalizeCustom(core.VideoEncoderNVENC, name+"=1")
		// typed принимает типизированную ошибку профиля для проверки кода, поля и объяснения.
		var typed *core.ProfileError
		if !errors.As(err, &typed) || typed.Code() != "custom_conflict" {
			t.Fatalf("Конфликт %s не опознан: %v", name, err)
		}
	}
}

// Проверяет влияние применимых настроек Encoding на равенство, канонические байты и ключ варианта без
// изменения входного профиля.
func TestEveryEncodingSettingParticipatesInIdentity(t *testing.T) {
	// cases перечисляет сценарии проверки допустимости или различимости полей профиля.
	cases := []struct {
		// name задаёт имя текущего сценария проверки поля профиля. encoder выбирает проверяемый
		// видеокодировщик.
		name, encoder string
		// change вносит выбранное изменение параметра в копию исходного профиля.
		change func(*core.GroupProfile)
	}{
		{"rc", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Меняет RateControl на cbr для проверки участия режима в идентичности.
			p.Encoding.RateControl = "cbr"
		}},
		{"rescale", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Отключает RescaleOutput для проверки участия масштабирования в идентичности.
			p.Encoding.RescaleOutput = false
		}},
		{"video_custom", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Задаёт VideoCustom nonref_p=1 для проверки участия дополнительных параметров в идентичности.
			p.Encoding.VideoCustom = "nonref_p=1"
		}},
		{"audio_custom", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Задаёт AudioCustom aac_coder=fast для проверки участия аудиопараметров в идентичности.
			p.Encoding.AudioCustom = "aac_coder=fast"
		}},
		{"nvenc_preset", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Меняет пресет NVENC на p7 для проверки различимости профилей.
			p.Encoding.NVENC.Preset = "p7"
		}},
		{"nvenc_tuning", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Меняет Tuning NVENC на ll для проверки различимости профилей.
			p.Encoding.NVENC.Tuning = "ll"
		}},
		{"nvenc_multipass", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Меняет Multipass NVENC на qres для проверки различимости профилей.
			p.Encoding.NVENC.Multipass = "qres"
		}},
		{"nvenc_lookahead", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Меняет LookaheadFrames NVENC на 1 для проверки различимости профилей.
			p.Encoding.NVENC.LookaheadFrames = 1
		}},
		{"nvenc_spatial_aq", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Включает пространственный AQ NVENC для проверки различимости профилей.
			p.Encoding.NVENC.EnableSpatialAQ = true
		}},
		{"nvenc_temporal_aq", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Включает временной AQ NVENC для проверки различимости профилей.
			p.Encoding.NVENC.EnableTemporalAQ = true
		}},
		{"nvenc_aq_strength", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Меняет AQStrength NVENC на 9 для проверки различимости профилей.
			p.Encoding.NVENC.AQStrength = 9
		}},
		{"svt_preset", core.VideoEncoderSVTAV1, func(p *core.GroupProfile) {
			// Меняет пресет SVTAV1 на 0 для проверки различимости профилей.
			p.Encoding.SVTAV1.Preset = 0
		}},
		{"svt_tuning", core.VideoEncoderSVTAV1, func(p *core.GroupProfile) {
			// Меняет Tuning SVTAV1 на vq для проверки различимости профилей.
			p.Encoding.SVTAV1.Tuning = "vq"
		}},
		{"svt_lookahead", core.VideoEncoderSVTAV1, func(p *core.GroupProfile) {
			// Меняет LookaheadFrames SVTAV1 на 6 для проверки различимости профилей.
			p.Encoding.SVTAV1.LookaheadFrames = 6
		}},
		{"svt_custom", core.VideoEncoderSVTAV1, func(p *core.GroupProfile) {
			// Задаёт VideoCustom lp=1 для проверки различимости профилей SVTAV1.
			p.Encoding.VideoCustom = "lp=1"
		}},
		{"version", core.VideoEncoderNVENC, func(p *core.GroupProfile) {
			// Убирает EncodingSettings целиком для сравнения идентичности явных и прежних настроек.
			p.Encoding = core.EncodingSettings{}
		}},
	}
	// tc содержит параметры и ожидаемый результат текущего сценария.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет, что выбранная настройка меняет ключи профиля и варианта, канонические байты и Equal,
			// сохраняя исходное значение.

			// p содержит отдельный полный профиль текущего сценария проверки.
			p := encodedProfile(tc.encoder, "vbr")
			if tc.encoder == core.VideoEncoderNVENC {
				p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
			}
			if tc.name == "nvenc_aq_strength" {
				p.Encoding.NVENC.EnableSpatialAQ = true
			}
			// before сохраняет исходный профиль для проверки отсутствия изменения.
			before := p
			// other содержит копию профиля с выбранным отличающимся параметром.
			other := p
			tc.change(&other)
			// key содержит вычисленный ключ полного профиля. canonical содержит канонические
			// байты полного профиля. err сообщает об ошибке вычисления канонической
			// идентичности профиля.
			key, canonical, err := core.ProfileIdentity(p)
			if err != nil {
				t.Fatal(err)
			}
			// otherKey содержит ключ изменённого полного профиля. otherCanonical содержит
			// канонические байты изменённого полного профиля.
			otherKey, otherCanonical, err := core.ProfileIdentity(other)
			if err != nil || key == otherKey || bytes.Equal(canonical, otherCanonical) || p.Equal(other) || other.Equal(p) || p != before {
				t.Fatalf("Новое поле не различено: %v", err)
			}
			// v1 содержит ключ варианта исходного профиля.
			v1, err := core.VariantIdentity("source", p, "generator")
			if err != nil {
				t.Fatal(err)
			}
			// v2 содержит ключ варианта изменённого профиля.
			v2, err := core.VariantIdentity("source", other, "generator")
			if err != nil || v1 == v2 {
				t.Fatalf("Варианты совпали: %v", err)
			}
		})
	}
}

// Проверяет отказ при удалении каждого сохранённого поля v2 и при null вместо материализованных
// нулевых значений без частичной записи получателя.
func TestStoredEncodingRequiresEveryMaterializedField(t *testing.T) {
	// p содержит отдельный полный профиль текущего сценария проверки.
	p := encodedProfile(core.VideoEncoderSVTAV1, "vbr")
	// raw содержит сохраняемое представление проверяемого профиля. err сообщает об ошибке
	// кодирования проверенного профиля.
	raw, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	// root предоставляет корневые поля сохранённого JSON-профиля.
	var root map[string]json.RawMessage
	// err сообщает об ошибке разбора JSON проверяемого представления.
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	// visit рекурсивно обходит вложенные поля профиля и запускает проверки их обязательности.
	var visit func(map[string]json.RawMessage, []string)
	visit = func(values map[string]json.RawMessage, path []string) {
		// Обходит вложенные объекты сохранённого профиля и для каждого поля запускает проверку обязательности
		// его присутствия.

		// name содержит имя очередного поля JSON, а value — его исходное представление для
		// рекурсивного обхода.
		for name, value := range values {
			// key содержит полный путь текущего JSON-поля от корня профиля.
			key := append(append([]string{}, path...), name)
			t.Run(strings.Join(key, "/"), func(t *testing.T) {
				// Удаляет выбранное вложенное поле из копии JSON и проверяет ErrInvalidMetadata без частичного
				// изменения приёмника.

				// copyRoot предоставляет отдельную копию корневого JSON-объекта для
				// повреждения.
				var copyRoot map[string]json.RawMessage
				// err сообщает об ошибке разбора JSON проверяемого представления.
				if err := json.Unmarshal(raw, &copyRoot); err != nil {
					t.Fatal(err)
				}
				// remove рекурсивно удаляет поле по выбранному пути и перекодирует
				// родителей.
				var remove func(map[string]json.RawMessage, []string)
				remove = func(object map[string]json.RawMessage, keys []string) {
					// Рекурсивно удаляет поле по точному пути и заново кодирует изменённые родительские объекты.

					if len(keys) == 1 {
						delete(object, keys[0])
						return
					}
					// child принимает очередной вложенный JSON-объект для обхода или
					// удаления поля.
					var child map[string]json.RawMessage
					// err сообщает об ошибке разбора JSON проверяемого
					// представления.
					if err := json.Unmarshal(object[keys[0]], &child); err != nil {
						t.Fatal(err)
					}
					remove(child, keys[1:])
					// b содержит перекодированный вложенный JSON-объект после
					// удаления поля. err сообщает об ошибке кодирования проверяемого
					// JSON-представления.
					b, err := json.Marshal(child)
					if err != nil {
						t.Fatal(err)
					}
					object[keys[0]] = b
				}
				remove(copyRoot, key)
				// corrupt содержит профиль с выбранным удалённым или обнулённым полем.
				// err сообщает об ошибке кодирования проверяемого JSON-представления.
				corrupt, err := json.Marshal(copyRoot)
				if err != nil {
					t.Fatal(err)
				}
				// receiver содержит исходное значение получателя для проверки отсутствия
				// частичного изменения.
				receiver := validProfile()
				// before сохраняет исходный профиль для проверки отсутствия изменения.
				before := receiver
				err = receiver.UnmarshalJSON(corrupt)
				if !errors.Is(err, core.ErrInvalidMetadata) || receiver != before {
					t.Fatalf("Отсутствующее поле принято или частично записано: %v", err)
				}
			})
			// child принимает очередной вложенный JSON-объект для обхода или удаления поля.
			var child map[string]json.RawMessage
			if len(value) > 0 && value[0] == '{' {
				// err сообщает об ошибке разбора JSON проверяемого представления.
				if err := json.Unmarshal(value, &child); err != nil {
					t.Fatal(err)
				}
				visit(child, key)
			}
		}
	}
	visit(root, nil)
	// field выбирает обязательное поле сохраняемого представления для проверки.
	for _, field := range []string{"RescaleOutput", "VideoCustom", "AudioCustom"} {
		// copyRoot предоставляет отдельную копию корневого JSON-объекта для повреждения.
		var copyRoot map[string]json.RawMessage
		// err сообщает об ошибке разбора JSON проверяемого представления.
		if err := json.Unmarshal(raw, &copyRoot); err != nil {
			t.Fatal(err)
		}
		// settings предоставляет поля блока Encoding для замены выбранного значения на null.
		var settings map[string]json.RawMessage
		// err сообщает об ошибке разбора JSON проверяемого представления.
		if err := json.Unmarshal(copyRoot["Encoding"], &settings); err != nil {
			t.Fatal(err)
		}
		settings[field] = json.RawMessage("null")
		copyRoot["Encoding"], err = json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		// corrupt содержит профиль с выбранным удалённым или обнулённым полем. err сообщает об
		// ошибке кодирования проверяемого JSON-представления.
		corrupt, err := json.Marshal(copyRoot)
		if err != nil {
			t.Fatal(err)
		}
		// err сообщает об ошибке чтения и проверки сохранённого профиля.
		if _, err := core.DecodeProfile(corrupt); err == nil {
			t.Fatalf("null поля %s принят как нулевое значение", field)
		}
	}
}

// Проверяет отсутствие разделяемых указательных полей профиля, запрет понижения v2 и приоритет CAS,
// сохраняя переименование прежнего профиля и явный переход к v2.
func TestEncodingValueOwnershipAndServiceTransition(t *testing.T) {
	// inspect рекурсивно проверяет состав GroupProfile на поля с разделяемым состоянием.
	var inspect func(reflect.Type)
	inspect = func(typ reflect.Type) {
		// Рекурсивно проверяет поля GroupProfile и отклоняет указатели, карты, срезы и интерфейсы, способные
		// удерживать разделяемое состояние.

		switch typ.Kind() {
		case reflect.Struct:
			// field содержит очередное отражённое поле структуры для проверки его типа.
			for field := range typ.Fields() {
				inspect(field.Type)
			}
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
			t.Fatalf("Профиль удерживает разделяемое состояние %s", typ)
		}
	}
	inspect(reflect.TypeFor[core.GroupProfile]())
	// output предоставляет корректный снимок выхода с исходным именованным профилем.
	_, output := contractSnapshots(t)
	// stale выбирает актуальную либо заведомо устаревшую ожидаемую ревизию.
	for _, stale := range []bool{false, true} {
		// current содержит исходный сохранённый профиль перед попыткой понижения версии.
		current := output.Profile
		current.Profile = encodedProfile(core.VideoEncoderNVENC, "vbr")
		// u предоставляет подменяемый конфигурационный репозиторий и учитывает записи.
		u := &configContractUOW{repository: configContractRepository{profile: current}}
		// service предоставляет доменные операции изменения тестового профиля. err сообщает об
		// ошибке создания доменного сервиса проверяемого профиля.
		service, err := core.NewService(u, "owner")
		if err != nil {
			t.Fatal(err)
		}
		// replacement содержит кандидат понижения полного профиля v2 до прежнего представления.
		replacement := current
		replacement.Profile = validProfile()
		// expected задаёт ожидаемую ревизию для проверки приоритета CAS.
		expected := current.Revision
		if stale {
			expected++
		}
		_, err = service.UpdateProfile(context.Background(), replacement, expected)
		if stale {
			if !errors.Is(err, core.ErrConflict) {
				t.Fatalf("CAS должен предшествовать проверке перехода: %v", err)
			}
		} else {
			// typed принимает типизированную ошибку профиля для проверки кода, поля и
			// объяснения.
			var typed *core.ProfileError
			if !errors.As(err, &typed) || typed.Field() != "profile.encoding" {
				t.Fatalf("Downgrade принят: %v", err)
			}
		}
		if u.repository.writes != 0 || u.repository.profile != current {
			t.Fatal("Отклонённый переход записан")
		}
	}
	// legacy задаёт исходный профиль без явного LIVE-аудиоселектора.
	legacy := output.Profile
	legacy.Profile.Format.Video.Width = 1
	legacy.Profile.VideoEncoding.BufferBits = math.MaxUint64
	// u предоставляет подменяемый конфигурационный репозиторий и учитывает записи.
	u := &configContractUOW{repository: configContractRepository{profile: legacy}}
	// service предоставляет доменные операции изменения тестового профиля. err сообщает об ошибке
	// создания доменного сервиса проверяемого профиля.
	service, err := core.NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	// candidate содержит кандидат переименования или явного обновления прежнего профиля.
	candidate := legacy
	candidate.Name = "Другое имя"
	if _, err = service.UpdateProfile(context.Background(), candidate, legacy.Revision); err != nil || u.repository.profile.Profile != legacy.Profile {
		t.Fatalf("Переименование применило новые ограничения к legacy: %v", err)
	}
	candidate = u.repository.profile
	candidate.Profile = encodedProfile(core.VideoEncoderNVENC, "vbr")
	if _, err = service.UpdateProfile(context.Background(), candidate, candidate.Revision); err != nil {
		t.Fatalf("Явное обновление legacy до v2 отклонено: %v", err)
	}
	candidate = u.repository.profile
	candidate.Name = "Профиль с новым именем"
	if _, err = service.UpdateProfile(context.Background(), candidate, candidate.Revision); err != nil {
		t.Fatalf("Переименование v2 отклонено: %v", err)
	}
}
