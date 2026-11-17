package group

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// flacPlayerArtifact возвращает синтетический FLAC-файл, полный пригодный манифест и бюджеты.
// t проверяет неизменность исходника FFmpeg из anullsrc; отдельный процесс и хранилище не нужны.
// H.264 High уровня 1.1, 128×128, 30 FPS и FLAC s16, 48 kHz, stereo зафиксированы при создании файла.
func flacPlayerArtifact(t *testing.T) ([]byte, core.ArtifactManifest, fmp4.Limits) {
	t.Helper()
	// data содержит исходный fMP4 из каталога подготовки; err сохраняет отказ его чтения.
	data, err := os.ReadFile(filepath.Join("..", "..", "fallback", "testdata", "audio_bindings", "flac-s16-silent.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 15573 || playerContentHash(data) != "90c5f7ff176c32ba06d4666cbd4cb07f4cf5fd1e810f869d7f8667c0155158c7" {
		t.Fatal("изменились байты синтетического FLAC-исходника")
	}
	// profile задаёт явное перекодирование FLAC с нулевым неприменимым целевым битрейтом.
	profile := bindingProfile(t, core.CodecFLAC)
	// manifest содержит все обязательные поля отдельного подготовленного варианта.
	manifest := core.ArtifactManifest{
		GenerationID: "player-generation", VariantID: "player-variant", AssetID: "player-asset",
		SourceHash: "synthetic-black-and-silence", Profile: profile, VerifiedFormat: profile.Format,
		GeneratorVersion: core.CurrentFallbackGeneratorVersion, ManagedPath: "variants/flac.mp4",
		SizeBytes: uint64(len(data)), ContentHash: playerContentHash(data),
	}
	manifest.VerifiedFormat.Video.Level = "1.1"
	manifest.ProfileKey, manifest.CanonicalProfile, err = core.ProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err = manifest.ValidateUsable(); err != nil {
		t.Fatal("непригоден исходный манифест", err)
	}
	// limits допускает исходные небольшие блоки, ограничивая разбор контейнера и первой аудиоединицы.
	limits := fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 65536, MaxMdatBytes: 1 << 20, MaxSampleBytes: 65536, MaxExtradataBytes: 4096, MaxSamples: 100}
	return data, manifest, limits
}

// playerContentHash возвращает шестнадцатеричный SHA256 всех data для манифеста временного файла.
func playerContentHash(data []byte) string {
	// digest сохраняет полную контрольную сумму, включая данные после первой единицы доступа.
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// playerContractFile создаёт отдельный файл с data и открывает его с flags для данного сценария.
// t удаляет временный каталог и закрывает дескриптор; сценарий вправе закрыть его раньше.
func playerContractFile(t *testing.T, data []byte, flags int) *os.File {
	t.Helper()
	// path задаёт тестовый файл, не связанный с исходным артефактом репозитория.
	path := filepath.Join(t.TempDir(), "artifact.mp4")
	// err сообщает о невозможности записать собственную копию исходника.
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// file содержит дескриптор с выбранным режимом доступа; err сохраняет отказ открытия.
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Обработчик закрывает file после сценария; уже закрытый дескриптор допустим в проверке отказа.
	t.Cleanup(func() {
		// closeErr сообщает о неожиданной ошибке освобождения собственного дескриптора.
		if closeErr := file.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			t.Error(closeErr)
		}
	})
	return file
}

// playerWithFreeTail дополняет data законным свободным блоком MP4 размером 40008 байт.
// Его дальний хвост позволяет отличить полный SHA256 от ошибочной проверки первых 32 KiB.
func playerWithFreeTail(data []byte) []byte {
	// free содержит заголовок и свободные байты отдельного блока, не затрагивающего медиадорожки.
	free := make([]byte, 40008)
	binary.BigEndian.PutUint32(free[:4], uint32(len(free)))
	copy(free[4:8], "free")
	return append(bytes.Clone(data), free...)
}

// requirePlayerFailure проверяет у t причину want и отсутствие пригодной команды при err.
// command не должен содержать даже частично разрешённый запуск или переданный дескриптор.
func requirePlayerFailure(t *testing.T, command ffmpeg.Command, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("ожидался отказ %v, получен %v", want, err)
	}
	if command.Binary != "" || len(command.Args) != 0 || len(command.ExtraFiles) != 0 {
		t.Fatalf("после отказа выдана команда: %+v", command)
	}
}

// TestVerifiedFLACPlayerUsesOwnedFDAndContinuousEncoder проверяет пригодный файл и полный SHA256.
// t запускает два сценария, включая файл больше 32 KiB; вложенный t сообщает об ошибках своего файла.
// Построитель возвращает FD в позиции 0, копирует видео и выбирает один FLAC/s16-кодировщик на все петли.
func TestVerifiedFLACPlayerUsesOwnedFDAndContinuousEncoder(t *testing.T) {
	// original хранит зафиксированный исходник, manifest — его полную идентичность; limits ограничивает разбор.
	original, manifest, limits := flacPlayerArtifact(t)
	// extended выбирает обычный файл либо тот же медиапоток с большим свободным хвостом.
	for _, extended := range []bool{false, true} {
		// Вложенный сценарий t проверяет собственный дескриптор; состояние других сценариев не меняется.
		t.Run(map[bool]string{false: "native-file", true: "large-free-tail"}[extended], func(t *testing.T) {
			// data хранит отдельные байты сценария, expected — манифест текущего размера.
			data, expected := bytes.Clone(original), manifest
			if extended {
				data = playerWithFreeTail(data)
				expected.SizeBytes, expected.ContentHash = uint64(len(data)), playerContentHash(data)
			}
			// file открыт тестом; исходная позиция в конце исключает случайный успех без начального Seek.
			file := playerContractFile(t, data, os.O_RDONLY)
			// err сохраняет отказ установки исходной позиции и построения команды.
			if _, err := file.Seek(0, io.SeekEnd); err != nil {
				t.Fatal(err)
			}
			// attempt задаёт только синтетический выходной UDS; файл сокета и процесс не создаются.
			attempt := ffmpeg.Attempt{OutputPath: "/private/tmp/verified-flac-player.sock"}
			// command и err содержат результат настоящей проверки и стандартного построителя.
			command, err := buildVerifiedFLACPlayer(context.Background(), "/unused/ffmpeg", file, attempt, expected, expected.Profile, limits)
			if err != nil {
				t.Fatal(err)
			}
			if command.Binary != "/unused/ffmpeg" || len(command.ExtraFiles) != 1 || command.ExtraFiles[0] != file {
				t.Fatalf("потерян выбранный бинарник или точный FD: %+v", command)
			}
			// args содержит аргументы команды; pair задаёт проверяемые параметр и значение без привязки к диагностике.
			args := command.Args
			for _, pair := range [][2]string{{"-stream_loop", "-1"}, {"-fd", "3"}, {"-i", "fd:"}, {"-c:v", "copy"}, {"-c:a", "flac"}, {"-sample_fmt:a", "s16"}, {"-ar:a", "48000"}, {"-ac:a", "2"}, {"-channel_layout:a", "stereo"}} {
				// index указывает единственное вхождение проверяемого параметра.
				index := slices.Index(args, pair[0])
				if index < 0 || index+1 >= len(args) || args[index+1] != pair[1] || slices.Contains(args[index+1:], pair[0]) {
					t.Fatalf("параметр %v не задан однозначно: %v", pair, args)
				}
			}
			if !slices.Contains(args, "-re") || slices.Contains(args, "-c") || args[len(args)-1] != "unix:"+attempt.OutputPath {
				t.Fatal("изменён темп, кодеки или выход прежнего файлового проигрывателя", args)
			}
			// position подтверждает возврат к началу после хеширования и чтения первой AU; seekErr сохраняет отказ.
			position, seekErr := file.Seek(0, io.SeekCurrent)
			if seekErr != nil || position != 0 {
				t.Fatalf("позиция FD=%d, ошибка=%v", position, seekErr)
			}
			// retained и readErr подтверждают неизменность файла и сохранение открытого FD у вызывающего.
			retained, readErr := io.ReadAll(file)
			if readErr != nil || !bytes.Equal(retained, data) {
				t.Fatal("проверка изменила или закрыла артефакт", readErr)
			}
			// serialized содержит JSON манифеста без права FLACOwned; marshalErr сохраняет отказ кодирования.
			serialized, marshalErr := json.Marshal(expected)
			if marshalErr != nil || bytes.Contains(serialized, []byte("FLACOwned")) {
				t.Fatal("право времени выполнения попало в манифест", marshalErr)
			}
			// demux и demuxErr восстанавливают фактические заголовки без предоставления права переписывать FLAC.
			demux, demuxErr := fmp4.NewDemuxer(bytes.NewReader(retained), fmp4.DemuxOptions{Limits: limits, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
			if demuxErr != nil {
				t.Fatal(demuxErr)
			}
			if demux.Init().Audio.Info().FLACOwned || demux.Init().Audio.Info().Audio != expected.VerifiedFormat.Audio {
				t.Fatal("внешний файл изменил формат или выдал право переписывать FLAC")
			}
		})
	}
}

// TestVerifiedFLACPlayerRejectsUnusableManifest проверяет обязательное доказательство пригодности.
// t изолирует подмену каждого поля; вложенный t подтверждает отказ до выдачи какой-либо команды.
func TestVerifiedFLACPlayerRejectsUnusableManifest(t *testing.T) {
	// data хранит исходник, manifest — его пригодное доказательство, limits — бюджеты разбора.
	data, manifest, limits := flacPlayerArtifact(t)
	// scenario выбирает повреждённое поле либо пригодный манифест другого аудиокодека.
	for _, scenario := range []string{"missing-settings", "generator", "profile-key", "canonical", "asset", "path", "non-flac"} {
		// Вложенный сценарий t проверяет один вид недостоверного манифеста.
		t.Run(scenario, func(t *testing.T) {
			// forged отделяет канонические байты сценария от исходного манифеста.
			forged := manifest
			forged.CanonicalProfile = bytes.Clone(manifest.CanonicalProfile)
			switch scenario {
			case "missing-settings":
				forged.Profile.Encoding = core.EncodingSettings{}
			case "generator":
				forged.GeneratorVersion = "fallback-fmp4-silent-v1"
			case "profile-key":
				forged.ProfileKey = strings.Repeat("0", 64)
			case "canonical":
				forged.CanonicalProfile[0] ^= 1
			case "asset":
				forged.AssetID = ""
			case "path":
				forged.ManagedPath = "../outside.mp4"
			case "non-flac":
				forged.Profile = bindingProfile(t, core.CodecAAC)
				forged.VerifiedFormat = forged.Profile.Format
				// identityErr сообщает об отказе пересчёта пригодного AAC-манифеста.
				var identityErr error
				forged.ProfileKey, forged.CanonicalProfile, identityErr = core.ProfileIdentity(forged.Profile)
				if identityErr != nil || forged.ValidateUsable() != nil {
					t.Fatal("не подготовлен пригодный манифест другого кодека", identityErr)
				}
			}
			// file остаётся открытой собственной копией; command и err описывают проверяемый отказ.
			file := playerContractFile(t, data, os.O_RDONLY)
			command, err := buildVerifiedFLACPlayer(context.Background(), "/unused/ffmpeg", file, ffmpeg.Attempt{OutputPath: "/private/tmp/player.sock"}, forged, forged.Profile, limits)
			requirePlayerFailure(t, command, err, core.ErrCorrupt)
		})
	}
}

// TestVerifiedFLACPlayerBindsAllBytesSizeAndFullProfile исключает подмену файла или настроек кодирования.
// t сохраняет пригодность манифеста; вложенный t различает размер, хеш дальнего хвоста и полный профиль.
func TestVerifiedFLACPlayerBindsAllBytesSizeAndFullProfile(t *testing.T) {
	// original хранит исходные байты, manifest — их пригодное доказательство, limits — бюджеты разбора.
	original, manifest, limits := flacPlayerArtifact(t)
	// scenario выбирает нарушение конкретной связи между выбранным файлом и профилем.
	for _, scenario := range []string{"hash", "smaller-size", "larger-size", "distant-tail", "encoder-profile"} {
		// Вложенный сценарий t получает собственный FD и не меняет общие входные данные.
		t.Run(scenario, func(t *testing.T) {
			// data содержит отдельную копию, expected описывает артефакт, requested задаёт запрошенный профиль.
			data, expected, requested := bytes.Clone(original), manifest, manifest.Profile
			switch scenario {
			case "hash":
				expected.ContentHash = strings.Repeat("0", 64)
			case "smaller-size":
				expected.SizeBytes--
			case "larger-size":
				expected.SizeBytes++
			case "distant-tail":
				data = playerWithFreeTail(data)
				expected.SizeBytes, expected.ContentHash = uint64(len(data)), playerContentHash(data)
				data[len(data)-1] = 1
			case "encoder-profile":
				requested.VideoEncoding.GOPFrames++
				if requested.Format != expected.Profile.Format || requested.Validate() != nil {
					t.Fatal("сценарий должен менять пригодный полный профиль при прежнем медиаформате")
				}
			}
			// usableErr исключает ранний отказ из-за случайного повреждения структуры манифеста.
			if usableErr := expected.ValidateUsable(); usableErr != nil {
				t.Fatal(usableErr)
			}
			// file содержит отдельный FD, command — полученную команду, err — причину отказа проверки.
			file := playerContractFile(t, data, os.O_RDONLY)
			command, err := buildVerifiedFLACPlayer(context.Background(), "/unused/ffmpeg", file, ffmpeg.Attempt{OutputPath: "/private/tmp/player.sock"}, expected, requested, limits)
			requirePlayerFailure(t, command, err, core.ErrCorrupt)
		})
	}
}

// TestVerifiedFLACPlayerChecksActualAudioAfterDigest проверяет контейнер после успешной сверки SHA256.
// t пересчитывает размер и хеш подменённых байтов; вложенный t проверяет другой кодек, заголовок и бюджеты.
func TestVerifiedFLACPlayerChecksActualAudioAfterDigest(t *testing.T) {
	// original хранит исходные байты, manifest — ожидаемый FLAC-формат, limits — бюджеты разбора.
	original, manifest, limits := flacPlayerArtifact(t)
	// scenario выбирает несоответствие после проверки файла по размеру и хешу.
	for _, scenario := range []string{"other-audio", "flac-metadata", "flac-first-frame", "truncated-init", "container-limit"} {
		// Вложенный сценарий t получает собственные байты и бюджеты контейнера.
		t.Run(scenario, func(t *testing.T) {
			// data отделяет повреждаемый контейнер, expected копирует манифест, bounded — бюджеты сценария.
			data, expected, bounded := bytes.Clone(original), manifest, limits
			switch scenario {
			case "other-audio":
				// readErr сохраняет отказ чтения другого синтетического кодека из того же набора исходников.
				var readErr error
				data, readErr = os.ReadFile(filepath.Join("..", "..", "fallback", "testdata", "audio_bindings", "mp3-mpeg1-silent.mp4"))
				if readErr != nil || playerContentHash(data) != "7c64faba6559ca7b2abcaae99a2d4691fb5da49d2e8f0c04763a962354f3771e" {
					t.Fatal("изменился синтетический MP3-исходник", readErr)
				}
				// demux и parseErr доказывают пригодность подмены: это настоящий MP3, а не повреждённый FLAC.
				demux, parseErr := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: limits, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				if demux.Init().Audio.Info().Audio.Codec != core.CodecMP3 {
					t.Fatal("подмена не содержит ожидаемый фактический MP3")
				}
			case "flac-metadata":
				// index находит единственный dfLa, чей блок метаданных объявляет 34 байта STREAMINFO.
				index := bytes.Index(data, []byte("dfLa"))
				if index < 0 || bytes.Count(data, []byte("dfLa")) != 1 || !bytes.Equal(data[index+8:index+12], []byte{0x80, 0, 0, 34}) {
					t.Fatal("не найден ожидаемый стандартный заголовок FLAC")
				}
				data[index+11] = 33
			case "flac-first-frame":
				// demux и parseErr извлекают первую подтверждённую AU исходника для точной подмены её байтов.
				demux, parseErr := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: limits, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				// description перебирает стандартные дорожки; изменяется только первая фактическая FLAC AU.
				for _, description := range demux.Descriptions() {
					if description.Codec != core.CodecFLAC {
						continue
					}
					// payload содержит полную первую AU; index указывает её единственное положение в файле.
					payload := description.FirstSample
					index := bytes.Index(data, payload)
					if len(payload) == 0 || index < 0 || bytes.Count(data, payload) != 1 {
						t.Fatal("первая FLAC AU не найдена однозначно")
					}
					data[index] ^= 1
				}
			case "truncated-init":
				data = data[:28]
			case "container-limit":
				bounded.MaxSampleBytes = 0
			}
			expected.SizeBytes, expected.ContentHash = uint64(len(data)), playerContentHash(data)
			// usableErr подтверждает, что подмена не была отклонена до обращения к реальным заголовкам.
			if usableErr := expected.ValidateUsable(); usableErr != nil {
				t.Fatal(usableErr)
			}
			// file содержит отдельный FD, command — полученную команду, err — отказ проверки контейнера.
			file := playerContractFile(t, data, os.O_RDONLY)
			command, err := buildVerifiedFLACPlayer(context.Background(), "/unused/ffmpeg", file, ffmpeg.Attempt{OutputPath: "/private/tmp/player.sock"}, expected, expected.Profile, bounded)
			requirePlayerFailure(t, command, err, core.ErrCorrupt)
			if scenario == "container-limit" && !errors.Is(err, fmp4.ErrLimit) {
				t.Fatal("потеряна причина отказа бюджета контейнера", err)
			}
		})
	}
}

// TestVerifiedFLACPlayerRejectsUnavailableDescriptors проверяет реальные отказы файловых дескрипторов.
// t создаёт собственные временные файлы; вложенный t проверяет nil, закрытый, пустой и недоступный для чтения FD.
func TestVerifiedFLACPlayerRejectsUnavailableDescriptors(t *testing.T) {
	// data хранит исходник, manifest — пригодное доказательство файла, limits — бюджеты разбора.
	data, manifest, limits := flacPlayerArtifact(t)
	// scenario выбирает отсутствующий, закрытый, пустой, не обычный или недоступный для чтения файл.
	for _, scenario := range []string{"nil", "closed", "empty", "directory", "write-only"} {
		// Вложенный сценарий t освобождает только собственные дескрипторы.
		t.Run(scenario, func(t *testing.T) {
			// file задаёт входной FD сценария; want содержит установленную причину отказа.
			var file *os.File
			want := core.ErrCorrupt
			switch scenario {
			case "closed":
				file = playerContractFile(t, data, os.O_RDONLY)
				// closeErr сообщает о невозможности подготовить закрытый дескриптор.
				if closeErr := file.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			case "empty":
				file = playerContractFile(t, nil, os.O_RDONLY)
			case "directory":
				// openErr сообщает об отказе открытия собственного каталога вместо обычного файла.
				var openErr error
				file, openErr = os.Open(t.TempDir())
				if openErr != nil {
					t.Fatal(openErr)
				}
				// Обработчик закрывает дескриптор каталога после завершения сценария.
				t.Cleanup(func() {
					// closeErr сохраняет ошибку закрытия собственного каталога.
					if closeErr := file.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				})
			case "write-only":
				file = playerContractFile(t, data, os.O_WRONLY)
				want = syscall.EBADF
			}
			// command и err содержат результат проверки без запуска процесса.
			command, err := buildVerifiedFLACPlayer(context.Background(), "/unused/ffmpeg", file, ffmpeg.Attempt{OutputPath: "/private/tmp/player.sock"}, manifest, manifest.Profile, limits)
			requirePlayerFailure(t, command, err, want)
		})
	}
}

// TestVerifiedFLACPlayerPreservesCancellationAndBorrowedFile проверяет отмену до чтения содержимого.
// t подтверждает context.Canceled и сохранение открытого дескриптора для прежнего владельца попытки.
func TestVerifiedFLACPlayerPreservesCancellationAndBorrowedFile(t *testing.T) {
	// data хранит исходник, manifest — пригодное доказательство файла, limits — бюджеты разбора.
	data, manifest, limits := flacPlayerArtifact(t)
	// file остаётся у теста, поскольку построенная команда не передаётся Supervisor.
	file := playerContractFile(t, data, os.O_RDONLY)
	// ctx уже отменён вызывающим; cancel закрывает Done до начала проверки SHA256.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// command и err должны сохранить отмену, не выдавая команду проигрывателя.
	command, err := buildVerifiedFLACPlayer(ctx, "/unused/ffmpeg", file, ffmpeg.Attempt{OutputPath: "/private/tmp/player.sock"}, manifest, manifest.Profile, limits)
	requirePlayerFailure(t, command, err, context.Canceled)
	// retained и readErr проверяют, что отменённая проверка не закрыла FD и не прочитала содержимое.
	retained, readErr := io.ReadAll(file)
	if readErr != nil || !bytes.Equal(retained, data) {
		t.Fatal("отмена изменила доступность или позицию файла", readErr)
	}
}
