package integration

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
)

// audioCaptureEpochFixture читает для t существующий синтетический FLAC fMP4 без внешних процессов.
// epoch задаёт только идентичность захвата; форматы, заголовки, AU и время восстанавливаются из файла.
// Признак FLACOwned не назначается: выборка проверяет наблюдение пакетов, а не допуск источника.
func audioCaptureEpochFixture(t *testing.T, epoch uint64) []media.Packet {
	t.Helper()
	// raw содержит неизменяемую файловую фикстуру; err сообщает об ошибке чтения или инициализации разбора.
	raw, err := os.ReadFile("../../internal/fallback/testdata/audio_bindings/flac-s16-silent.mp4")
	if err != nil {
		t.Fatal(err)
	}
	// demux восстанавливает фактические конфигурации обеих дорожек и проверяет полные сжатые AU.
	demux, err := fmp4.NewDemuxer(bytes.NewReader(raw), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: epoch, VideoRevision: 1, AudioRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	// packets сохраняет исходный порядок файла без подстановки желаемого профиля.
	var packets []media.Packet
	for {
		// packet содержит следующую полную AU; readErr различает конец файла и неверный контейнер.
		packet, readErr := demux.Next()
		if errors.Is(readErr, io.EOF) {
			return packets
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		packets = append(packets, packet)
	}
}

// TestAudioCaptureRequiresCurrentLiveAudio проверяет через t границу наблюдения независимого захвата:
// множество старых пакетов и LIVE-статус с текущим видео не заменяют аудио текущей эпохи.
// После его поступления выборка обязана сохранить обе дорожки, исходные AU, время и смешанное окно.
func TestAudioCaptureRequiresCurrentLiveAudio(t *testing.T) {
	// fallback сохраняет старую эпоху заглушки; previousLive моделирует запоздалую прежнюю LIVE-доставку.
	fallback, previousLive := audioCaptureEpochFixture(t, 1), audioCaptureEpochFixture(t, 2)
	// current содержит настоящую пару дорожек новой эпохи без признака FLACOwned.
	current := audioCaptureEpochFixture(t, 4)
	// video и audio закрепляют по одному исходному пакету текущей эпохи для управляемого захвата.
	var video, audio media.Packet
	// packet ищет первую фактическую AU каждой дорожки, сохраняя её исходные байты и время.
	for _, packet := range current {
		if packet.Config().Info().Track == media.TrackVideo && video.Validate() != nil {
			video = packet
		}
		if packet.Config().Info().Track == media.TrackAudio && audio.Validate() != nil {
			audio = packet
		}
	}
	if video.Validate() != nil || audio.Validate() != nil {
		t.Fatal("фикстура не содержит обеих фактических дорожек")
	}
	// window содержит больше 150 старых пакетов и текущее видео; прежнее LIVE-аудио уже присутствует.
	window := append(append(append([]media.Packet(nil), fallback...), previousLive...), video)
	if len(window) <= 150 {
		t.Fatal("фикстура не воспроизвела накопление старого захвата")
	}
	// status воспроизводит уже опубликованную Router текущую эпоху до добавления её аудио сборщиком.
	status := group.Status{Available: true, Source: group.Live, Epoch: 4}
	// ready проверяет отсутствие ложной готовности при старом звуке и текущем видео.
	if _, ready := audioContractLiveEpoch(status, window); ready {
		t.Fatal("прежнее аудио принято вместо ещё не захваченного текущего LIVE")
	}
	window = append(window, audio)
	// original закрепляет полный смешанный вход для проверки отсутствия изменений при фильтрации.
	original := append([]media.Packet(nil), window...)
	// selected и ready содержат текущую пару и результат наблюдения после поступления её аудио.
	selected, ready := audioContractLiveEpoch(status, window)
	if !ready || len(selected) != 2 {
		t.Fatalf("текущая пара не выбрана: ready=%t packets=%d", ready, len(selected))
	}
	// index задаёт исходный порядок текущих video/audio; want содержит соответствующий исходный пакет.
	for index, want := range []media.Packet{video, audio} {
		// got содержит отобранный пакет для полного сравнения времени, конфигурации и сжатых байтов.
		got := selected[index]
		if got.Info() != want.Info() || got.Config().Info() != want.Config().Info() || !bytes.Equal(got.Config().Headers(), want.Config().Headers()) || !bytes.Equal(got.Payload(), want.Payload()) {
			t.Fatalf("фильтрация изменила пакет текущей эпохи: index=%d", index)
		}
	}
	if !reflect.DeepEqual(window, original) {
		t.Fatal("фильтрация изменила смешанный захват")
	}
	selected[0] = media.Packet{}
	if !reflect.DeepEqual(window, original) {
		t.Fatal("выборка разделяет изменяемый список со смешанным захватом")
	}
	// audioOnly оставляет текущее аудио рядом со старым видео; декодеру нужна видеоконфигурация текущей эпохи.
	audioOnly := append(append([]media.Packet(nil), fallback...), audio)
	// ready требует также реально захваченную видеоконфигурацию той же эпохи.
	if _, ready := audioContractLiveEpoch(status, audioOnly); ready {
		t.Fatal("прежнее видео принято как конфигурация текущей эпохи")
	}
	status.Available = false
	// ready не принимает накопленные пакеты за подтверждение доступного LIVE при отказе выхода.
	if _, ready := audioContractLiveEpoch(status, window); ready {
		t.Fatal("недоступный LIVE признан готовым")
	}
	status.Available, status.Source = true, group.Fallback
	// ready не приписывает пакетам заглушки выбор LIVE даже при совпадающем номере эпохи.
	if _, ready := audioContractLiveEpoch(status, window); ready {
		t.Fatal("выбранная заглушка признана текущим LIVE")
	}
}
