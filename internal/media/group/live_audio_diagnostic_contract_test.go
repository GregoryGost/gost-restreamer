package group

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

// liveAudioDiagnosticLog хранит JSON-журнал с синхронизацией записи и чтения контрольных событий.
type liveAudioDiagnosticLog struct {
	// mu защищает накопление и копирование байтов журнала.
	mu sync.Mutex
	// buffer накапливает полные JSON-записи диагностического сценария.
	buffer bytes.Buffer
}

// Write записывает данные JSON-журнала под блокировкой для конкурентного тестового чтения.
func (l *liveAudioDiagnosticLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

// Возвращает независимую копию накопленного журнала под блокировкой.
func (l *liveAudioDiagnosticLog) snapshot() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.buffer.Bytes())
}

// Проверяет отказ несовместимого AAC при первом LIVE, смене конфигурации и замене источника,
// сохранение обеих дорожек исправной заглушки и коррелированную причину без запуска перекодировщика.
func TestLiveAudioDirectCopyRejectsMismatchWithDiagnosticAndHealthyFallback(t *testing.T) {
	// scenario выбирает несовместимый AAC при первом LIVE, смене конфигурации либо замене источника.
	for _, scenario := range []string{"initial", "configuration_change", "override"} {
		t.Run(scenario, func(t *testing.T) {
			// Проверяет отказ несовместимого LIVE AAC на первом входе, смене конфигурации либо Override, сохраняя
			// готовый fallback и причину в журнале.

			// f предоставляет сохранённые профили и точное выполнение группы для проверки
			// LIVE AAC copy.
			f := newRuntimeAcceptanceFixture(t)
			// snapshot сохраняет закреплённый снимок AV1 copy с AAC 48000 Гц.
			var snapshot core.OutputSnapshot
			// output содержит очередной сохранённый выход при поиске подходящего
			// copy-профиля.
			for _, output := range f.outputs {
				if output.Snapshot.Profile.Profile.Mode == core.ModeCopy {
					snapshot = output.Snapshot
					break
				}
			}
			if snapshot.Profile.Profile.Format.Video.Codec != core.CodecAV1 || snapshot.Profile.Profile.Format.Audio.SampleRateHz != 48000 {
				t.Fatal("нет исходного профиля AV1 copy с AAC 48 kHz")
			}
			// ctx ограничивает аудиосценарий двенадцатью секундами; cancel освобождает
			// таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			// live содержит Router проверяемой LIVE-публикации; err сохраняет результаты
			// создания, настройки и чтения медиаресурсов.
			live, err := media.NewRouter(f.config.Switcher.Router)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает LIVE Router сценария аудио, игнорируя ошибку.
				_ = live.Close()
			})
			// logs хранит коррелированный JSON-журнал отказа несовместимого AAC.
			var logs liveAudioDiagnosticLog
			// transcodes считает недопустимые запуски перекодирования в группе прямого AV1
			// copy.
			var transcodes atomic.Int32
			// config содержит настройки группы с управляемым проигрывателем и запретом
			// перекодирования.
			config := f.config
			config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			// directory задаёт каталог временных файлов для отдельных попыток проигрывателя.
			directory := t.TempDir()
			config.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) {
				// Создаёт отдельный временный файл для каждой попытки условного проигрывателя заглушки.

				return os.CreateTemp(directory, "fallback-")
			}
			config.Player = func(_ context.Context, _ string, _ *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
				// Строит команду тестового проигрывателя с профилем выбранного выхода и путями текущей попытки.

				return acceptanceHelperCommand(snapshot.Profile.Profile, attempt)
			}
			config.Transcode = func(context.Context, ffmpeg.NVIDIAConfig, ffmpeg.Attempt) (ffmpeg.Command, error) {
				// Считает недопустимые вызовы transcode в группе AV1 copy и возвращает ошибку.

				transcodes.Add(1)
				return ffmpeg.Command{}, errors.New("прямое копирование не должно запускать перекодирование")
			}
			// registry содержит реестр единственной проверяемой группы и ссылки.
			registry, err := NewRegistry(ctx, RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: config, MaxGroups: 1, MaxReferences: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает реестр групп после сценария и сообщает ошибку через t.Error.

				// err сообщает об отказе резервного закрытия реестра групп.
				if err := registry.Close(context.Background()); err != nil {
					t.Error("остановка группы:", err)
				}
			})
			// ref удерживает группу и предоставляет её постоянный выходной Router.
			ref, err := registry.Acquire(ctx, snapshot, live)
			if err != nil {
				t.Fatal(err)
			}
			// group содержит проверяемую группу с готовой заглушкой и несовместимым LIVE.
			group := ref.Group()
			if err = group.WaitReady(ctx); err != nil {
				t.Fatal("заглушка не готова:", err)
			}
			// status содержит состояние до LIVE для проверки выбранной исправной заглушки.
			if status := group.Status(); !status.Available || status.Source != Fallback {
				t.Fatal("до LIVE не выбрана исправная заглушка:", status)
			}
			// output содержит выходную подписку для проверки исходного LIVE AAC и
			// сохранности заглушки.
			output, err := ref.Router().Subscribe(config.Switcher.Queue)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает выходную подписку после сценария, игнорируя ошибку.
				_ = output.Close()
			})
			// source содержит текущую эпоху входного LIVE до возможного Override.
			source, err := live.BeginSource()
			if err != nil {
				t.Fatal(err)
			}
			// video содержит контрольную AV1-конфигурацию; audio содержит первоначально
			// совместимую AAC-конфигурацию текущей эпохи.
			video, audio := acceptanceRuntimeConfigs(t, snapshot.Profile.Profile, source.Epoch())
			// Полный AAC-LC AU с FIL отличается от AU заглушки; его байты должны сохраниться.
			// payload содержит полный контрольный AAC-LC AU с FIL, отличающийся от аудио
			// заглушки.
			payload, err := hex.DecodeString("de02004c61766336312e31392e31303100422008c11838")
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "initial" {
				if err = source.UpdateConfigs(video, audio); err != nil {
					t.Fatal(err)
				}
				// stop отменяет и дожидается производителя первоначального совместимого
				// LIVE.
				stop := publishLiveAudioDiagnostic(t, ctx, source, video, audio, payload, 0)
				waitRuntimeAcceptance(t, func() bool {
					// Ожидает доступный LIVE после подачи совместимого AAC.

					// status содержит текущее состояние при ожидании восстановления
					// совместимого LIVE.
					status := group.Status()
					return status.Available && status.Source == Live
				})
				liveAudioDiagnosticPayload(t, ctx, output, payload)
				stop()
			}
			// start задаёт начальный видеотик несовместимой публикации; 60 продолжает шкалу
			// после смены конфигурации.
			start := int64(0)
			switch scenario {
			case "override":
				source, err = live.ReplaceSource()
				if err != nil {
					t.Fatal(err)
				}
				video, audio = acceptanceRuntimeConfigs(t, snapshot.Profile.Profile, source.Epoch())
			case "configuration_change":
				start = 60
			}
			// audioInfo содержит копию описания AAC для согласованной смены частоты,
			// временной базы и при необходимости ревизии.
			audioInfo := audio.Info()
			audioInfo.Audio.SampleRateHz = 44100
			audioInfo.TimeBase = media.TimeBase{Numerator: 1, Denominator: 44100}
			if scenario == "configuration_change" {
				audioInfo.Revision++
			}
			// ASC 0x1210 задаёт AAC-LC, 44,1 kHz и два канала: метаданные соответствуют заголовку.
			audio, err = media.NewCodecConfig(audioInfo, []byte{0x12, 0x10})
			if err != nil {
				t.Fatal(err)
			}
			if err = source.UpdateConfigs(video, audio); err != nil {
				t.Fatal(err)
			}
			publishLiveAudioDiagnostic(t, ctx, source, video, audio, payload, start)
			waitRuntimeAcceptance(t, func() bool {
				// Ожидает доступный fallback после подачи несовместимого LIVE AAC.

				// status содержит состояние при ожидании возврата на исправную заглушку
				// после отказа LIVE AAC.
				status := group.Status()
				return status.Available && status.Source == Fallback
			})
			// seenAudio отмечает получение исправного AAC заглушки; seenVideo отмечает
			// получение её видео после отказа LIVE.
			seenAudio, seenVideo := false, false
			for range 24 {
				// delivery содержит очередную выходную выдачу; err сообщает об отказе
				// чтения продолжающейся заглушки.
				delivery, err := output.Next(ctx)
				if err != nil {
					t.Fatal("выдача заглушки прервалась:", err)
				}
				if !delivery.IsCurrent() {
					continue
				}
				if delivery.Packet.Config().Info().Track == media.TrackAudio {
					seenAudio = true
					if delivery.AudioConfig.Info().Audio != snapshot.Profile.Profile.Format.Audio || !bytes.Equal(delivery.Packet.Payload(), switchAcceptanceAACPayload()) {
						t.Fatal("несовместимый LIVE заменил AAC исправной заглушки")
					}
				} else {
					seenVideo = true
				}
			}
			if !seenAudio || !seenVideo || group.Ready() != nil {
				t.Fatal("после отказа LIVE не продолжаются обе дорожки исправной заглушки")
			}
			// calls содержит число фактических вызовов построителя transcode для проверки их
			// отсутствия.
			if calls := transcodes.Load(); calls != 0 {
				t.Fatalf("AV1 copy вызвал построитель перекодирования %d раз", calls)
			}
			liveAudioDiagnosticReason(t, ctx, &logs, f.parent.SessionID, snapshot.Group.ID)
		})
	}
}

// Запускает контрольный LIVE с заданными AAC-байтами и начальной меткой; возвращаемая функция единожды
// отменяет и ждёт производителя и регистрируется после теста.
func publishLiveAudioDiagnostic(t *testing.T, parent context.Context, source *media.Source, video, audio media.CodecConfig, payload []byte, start int64) func() {
	t.Helper()
	// ctx задаёт отдельный отменяемый срок контрольной LIVE-публикации; cancel прекращает её
	// производство.
	ctx, cancel := context.WithCancel(parent)
	// done передаёт результат фактического завершения производителя LIVE.
	done := make(chan error, 1)
	go func() {
		// Публикует AV1 по сетке 60 FPS и полные AAC AU до конца каждого кадра, завершаясь при ошибке
		// публикации либо отмене.

		// ticker задаёт темп контрольного AV1-видео 60 кадров в секунду.
		ticker := time.NewTicker(time.Second / 60)
		defer ticker.Stop()
		// audioTicks хранит следующую аудиометку в шкале частоты AAC с шагом 1024.
		audioTicks := start * int64(audio.Info().Audio.SampleRateHz) / 60
		// frame задаёт очередную видеометку в шкале 1/60, начиная с переданного start.
		for frame := start; ; frame++ {
			// packet содержит очередной AV1 либо AAC-пакет; err сохраняет результаты
			// создания и публикации.
			packet, err := media.NewPacket(media.PacketInfo{PTS: frame, DTS: frame, DurationTicks: 1, Keyframe: true}, video, append(video.Headers()[4:], 0x1a, 1, 0x10))
			if err == nil {
				err = source.Publish(packet)
			}
			if err != nil {
				done <- err
				return
			}
			for audioTicks*60 < (frame+1)*int64(audio.Info().Audio.SampleRateHz) {
				packet, err = media.NewPacket(media.PacketInfo{PTS: audioTicks, DTS: audioTicks, DurationTicks: 1024}, audio, payload)
				if err == nil {
					err = source.Publish(packet)
				}
				if err != nil {
					done <- err
					return
				}
				audioTicks += 1024
			}
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
			}
		}
	}()
	// once исключает повторную отмену и повторное чтение результата производителя.
	var once sync.Once
	// stop единожды отменяет контрольный LIVE и ожидает возврата его производителя.
	stop := func() {
		// Единожды останавливает публикацию через sync.Once и ждёт её результата.

		once.Do(func() {
			// Отменяет контекст публикации, дожидается горутины и сообщает ошибку публикации через t.Error.

			cancel()
			// err содержит окончательный результат производства LIVE после отмены.
			if err := <-done; err != nil {
				t.Error("публикация контролируемого LIVE:", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// Ожидает актуальный аудиопакет с точными исходными байтами в подписке до срока контекста.
func liveAudioDiagnosticPayload(t *testing.T, ctx context.Context, output *media.Subscription, want []byte) {
	t.Helper()
	for {
		// delivery содержит очередную выдачу для поиска актуального AAC с точными исходными
		// байтами; err сообщает об отказе ожидания.
		delivery, err := output.Next(ctx)
		if err != nil {
			t.Fatal("совместимый LIVE не передал исходный AAC:", err)
		}
		if delivery.IsCurrent() && delivery.Packet.Config().Info().Track == media.TrackAudio && bytes.Equal(delivery.Packet.Payload(), want) {
			return
		}
	}
}

// Ожидает запись ERROR с точными Session и Group и раздельными фактической 44100 и ожидаемой 48000
// частотами в причине AAC.
func liveAudioDiagnosticReason(t *testing.T, parent context.Context, logs *liveAudioDiagnosticLog, session, group core.ID) {
	t.Helper()
	// ctx ограничивает поиск коррелированной причины одной секундой; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	// ticker задаёт период повторного чтения накопленного журнала.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		// decoder разбирает независимую копию накопленных JSON-записей текущего прохода.
		decoder := json.NewDecoder(bytes.NewReader(logs.snapshot()))
		for {
			// record содержит очередную JSON-запись для проверки частот AAC и корреляции
			// отказа.
			var record map[string]any
			// err сообщает о конце текущей копии журнала либо некорректном JSON.
			if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal("некорректный JSON журнала:", err)
			}
			// reason содержит строковую причину отказа из поля error текущей записи.
			reason, _ := record["error"].(string)
			// actual содержит часть причины до описания ожидаемого формата; expected
			// содержит оставшуюся часть; describesExpected показывает наличие разделителя
			// ожидания.
			actual, expected, describesExpected := strings.Cut(reason, "ожида")
			if describesExpected && strings.Contains(actual, "AAC") && strings.Contains(actual, "44100") && strings.Contains(expected, "48000") && !strings.Contains(expected, "44100") {
				if record["session_id"] != string(session) || record["group_id"] != string(group) || record["component"] != "group" || record["operation"] == nil || record["level"] != "ERROR" {
					t.Fatal("причина несовместимого AAC потеряла корреляцию или уровень:", record)
				}
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("при исправной заглушке нет коррелированной причины отказа LIVE AAC с фактической частотой 44100 и ожидаемой 48000; журнал:\n%s", logs.snapshot())
		case <-ticker.C:
		}
	}
}
