package codec

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// audioBindingNative запускает объявленный FFmpeg binary с ограниченными синтетическими данными input.
// t сообщает об ошибке процесса, binary задаёт проверенный путь, args — точные аргументы кодека.
// Результат возвращает stdout; процесс ограничен 15 с и не обращается к файлам или сети.
func audioBindingNative(t *testing.T, binary string, input []byte, args ...string) []byte {
	t.Helper()
	// ctx ограничивает срок отдельного кодирования или декодирования; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// command получает только статические аргументы и синтетический stdin.
	command := exec.CommandContext(ctx, binary, append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-threads", "1"}, args...)...)
	command.Stdin = bytes.NewReader(input)
	// diagnostic сохраняет stderr процесса; output содержит stdout, ограниченный синтетическим входом,
	// err сообщает о завершении процесса, ошибке запуска либо превышении срока.
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	output, err := command.Output()
	if err != nil || diagnostic.Len() != 0 {
		t.Fatalf("native command=%v err=%v stderr=%s", args, err, diagnostic.String())
	}
	return output
}

// TestAC3NativeFreshSilenceWithDifferentBSI проверяет нормализацию разных свежих кадров кодировщика.
// t использует синтетический PCM и явно включённый FFmpeg; отдельный декодер обязан выдать точный 0.
// Второй вариант меняет полосу и длину BSI, исключая зависимость от смещений одного тестового образца.
func TestAC3NativeFreshSilenceWithDifferentBSI(t *testing.T) {
	// binary задаётся только явным включением проверок FFmpeg, как в остальных медиатестах проекта.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("нужен явный GOST_MEDIA_FFMPEG для native audio oracle")
	}
	// pcm содержит 3072 нулевых отсчёта PCM16 на канал для нескольких последовательных AU.
	pcm := make([]byte, 3072*2*2)
	// extra задаёт варианты допустимого синтаксиса встроенного кодировщика с разными BSI и полосой.
	for _, extra := range [][]string{nil, {"-cutoff", "16000", "-mixing_level", "85", "-room_type", "2"}, {"-cutoff", "8000"}} {
		// args создаёт одну ограниченную попытку кодирования AC3 без coupling и rematrix.
		args := []string{"-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0", "-c:a", "ac3", "-b:a", "192k", "-channel_coupling", "0", "-stereo_rematrixing", "0"}
		args = append(args, extra...)
		args = append(args, "-f", "ac3", "pipe:1")
		// encoded хранит свежие кадры кодировщика до проверки их количества и границ.
		encoded := audioBindingNative(t, binary, pcm, args...)
		if len(encoded) == 0 || len(encoded)%768 != 0 || len(encoded) > 4*768 {
			t.Fatalf("native AC3 size=%d", len(encoded))
		}
		// normalized накапливает только AU с доказанными нулевыми коэффициентами всех шести блоков.
		var normalized []byte
		// offset выбирает границу очередного полного кадра AC3, 192 кбит/с, 48 кГц.
		for offset := 0; offset < len(encoded); offset += 768 {
			// headers содержит фактический dac3 текущей AU; err сообщает об ошибке её CRC или параметров.
			headers, err := AC3Headers(encoded[offset : offset+768])
			if err != nil {
				t.Fatal(err)
			}
			// frame содержит результат доказательства всех шести блоков по фактическому синтаксису.
			frame, err := NormalizeAC3Silence(headers, encoded[offset:offset+768])
			if err != nil {
				t.Fatalf("extra=%v offset=%d err=%v", extra, offset, err)
			}
			normalized = append(normalized, frame...)
		}
		// decoded использует PCM с плавающей точкой, чтобы перевод в s16 не скрыл малые ненулевые значения.
		decoded := audioBindingNative(t, binary, normalized, "-c:a", "ac3", "-f", "ac3", "-i", "pipe:0", "-c:a", "pcm_f32le", "-f", "f32le", "pipe:1")
		if len(decoded) != len(encoded)/768*1536*2*4 || !bytes.Equal(decoded, make([]byte, len(decoded))) {
			t.Fatalf("неполная либо ненулевая native AC3 тишина: encoded=%d PCMbytes=%d", len(encoded), len(decoded))
		}
	}
	// sample заполняет слышимый стереосигнал, который не получает коррекцию тишины.
	for sample := range 3072 {
		pcm[4*sample], pcm[4*sample+1], pcm[4*sample+2], pcm[4*sample+3] = 0xe8, 3, 0x18, 0xfc
	}
	// audible содержит свежую корректную AC3 AU с ненулевыми переданными коэффициентами.
	audible := audioBindingNative(t, binary, pcm, "-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0", "-c:a", "ac3", "-b:a", "192k", "-channel_coupling", "0", "-stereo_rematrixing", "0", "-f", "ac3", "pipe:1")
	if len(audible) < 768 {
		t.Fatal("отсутствует native слышимая AC3 AU")
	}
	// headers содержит фактический dac3 слышимого кадра; err сообщает об ошибке кадра либо CRC.
	headers, err := AC3Headers(audible[:768])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NormalizeAC3Silence(headers, audible[:768]); err == nil {
		t.Fatal("normalizer принял слышимую native AC3 frame")
	}
}

// TestFLACNativePCMAndPrivateRebase проверяет подкадры кодировщика с постоянными данными и предсказанием.
// t кодирует ровно 4608 отсчётов PCM16, разбирает FLAC и декодирует отдельный перенумерованный поток.
func TestFLACNativePCMAndPrivateRebase(t *testing.T) {
	// binary берётся только из явно включённой локальной среды FFmpeg.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("нужен явный GOST_MEDIA_FFMPEG для native audio oracle")
	}
	// tone выбирает цифровую тишину либо слышимый изменяющийся PCM без внешних медиа.
	for _, tone := range []bool{false, true} {
		// pcm содержит ровно 4608 отсчётов на канал, чтобы кодировщик не создал короткий последний кадр.
		pcm := make([]byte, 4608*2*2)
		if tone {
			// sample задаёт ограниченную треугольную волну в каждом канале.
			for sample := range 4608 {
				// value изменяется без плавающей точки; оба канала получают разные знаковые 16-битные последовательности.
				value := int16((sample%128 - 64) * 256)
				pcm[4*sample], pcm[4*sample+1] = byte(value), byte(uint16(value)>>8)
				pcm[4*sample+2], pcm[4*sample+3] = byte(-value), byte(uint16(-value)>>8)
			}
		}
		// encoded содержит один поток FLAC, включая STREAMINFO и необязательные блоки метаданных.
		encoded := audioBindingNative(t, binary, pcm, "-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0", "-c:a", "flac", "-sample_fmt", "s16", "-frame_size", "4608", "-f", "flac", "pipe:1")
		if len(encoded) < 42 || string(encoded[:4]) != "fLaC" || encoded[4]&127 != 0 {
			t.Fatal("native FLAC не содержит initial STREAMINFO")
		}
		// headers сохраняет 34 байта фактического STREAMINFO; offset проходит только полные блоки метаданных.
		headers := bytes.Clone(encoded[8:42])
		offset := 4
		for {
			if offset > len(encoded)-4 {
				t.Fatal("усечённый native FLAC metadata header")
			}
			// last определяет завершающий блок метаданных, size — фактическую длину его тела.
			last, size := encoded[offset]&128 != 0, int(flacUint24(encoded[offset+1:offset+4]))
			if size > len(encoded)-offset-4 {
				t.Fatal("усечённый native FLAC metadata payload")
			}
			offset += 4 + size
			if last {
				break
			}
		}
		// frame содержит полную фактическую AU на 4608 отсчётов; err сообщает об ошибке заголовка, CRC или подкадров.
		frame, err := FLACFrameInfo(headers, encoded[offset:])
		if err != nil || frame.Samples != 4608 {
			t.Fatalf("tone=%v frame=%+v err=%v", tone, frame, err)
		}
		// continuous убирает файловую длину, MD5 и границы кадров для нового отдельного физического потока.
		continuous, err := FLACContinuousHeaders(headers)
		if err != nil {
			t.Fatal(err)
		}
		// frameN имитирует поздний внешний номер отсчёта, которому затем назначается начало с 0.
		frameN, err := RewriteFLACFrame(headers, encoded[offset:], 123456789)
		if err != nil {
			t.Fatal(err)
		}
		// private содержит тот же сжатый сигнал, начиная отдельный физический поток с 0.
		private, err := RewriteFLACFrame(continuous, frameN, 0)
		if err != nil {
			t.Fatal(err)
		}
		// wire содержит минимальный заголовок потока FLAC; err проверяет продолжаемый STREAMINFO.
		wire, err := FLACWireHeader(continuous)
		if err != nil {
			t.Fatal(err)
		}
		// decoded обязан совпасть побайтово с исходным PCM после изменения только заголовка.
		decoded := audioBindingNative(t, binary, append(wire, private...), "-f", "flac", "-i", "pipe:0", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1")
		if !bytes.Equal(decoded, pcm) {
			t.Fatalf("tone=%v: lossless FLAC PCM изменён: got%d want%d bytes", tone, len(decoded), len(pcm))
		}
	}
}
