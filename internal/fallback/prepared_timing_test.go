package fallback

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// preparedProbeRunner подставляет управляемые метки пакетов и фиксированные метаданные вместо внешнего
// ffprobe.
type preparedProbeRunner struct { /* packets содержит JSON записей пакетов для запроса -show_packets. */
	packets []byte
}

// Run выдаёт управляемый JSON пакетов для -show_packets и метаданные формата для остальных запросов
// ffprobe.
func (r preparedProbeRunner) Run(_ context.Context, c Command, _ *os.File, w io.Writer) (RunResult, error) {
	// data выбирает JSON формата либо заданные метки пакетов по аргументам команды.
	data := []byte(av1ProbeJSON)
	if slices.Contains(c.Args, "-show_packets") {
		data = r.packets
	}
	// err сообщает об отказе записи подставленного JSON получателю.
	_, err := w.Write(data)
	return RunResult{}, err
}

// Создаёт метки полного периода из 48 видеокадров и 75 AAC-пакетов с отдельным начальным пакетом Skip
// Samples.
func selfPreparedMetadata() packetDocument {
	// doc накапливает эталонные потоки и пакеты полного периода.
	doc := packetDocument{}
	doc.Streams = append(doc.Streams, struct {
		// Index задаёт индекс эталонной видеодорожки.
		Index int `json:"index"`
		// Type обозначает эталонный поток как video.
		Type string `json:"codec_type"`
		// Base задаёт шкалу времени видеопотока 1/30 секунды.
		Base string `json:"time_base"`
	}{0, "video", "1/30"}, struct {
		// Index задаёт индекс эталонной аудиодорожки.
		Index int `json:"index"`
		// Type обозначает эталонный поток как audio.
		Type string `json:"codec_type"`
		// Base задаёт шкалу времени аудиопотока 1/48000 секунды.
		Base string `json:"time_base"`
	}{1, "audio", "1/48000"})
	// stamp создаёт запись пакета с равными PTS и DTS и заданной длительностью.
	stamp := func(kind string, index int, pts, duration int64) packetStamp {
		// Создаёт отметки одного пакета с равными PTS и DTS и заданной длительностью для эталонных метаданных
		// ffprobe.

		return packetStamp{Type: kind, Index: index, PTS: &pts, DTS: &pts, Duration: &duration}
	}
	// i задаёт очередной номер AAC-пакета, включая задержку, и видеокадра при наличии.
	for i := range 76 {
		if i < 48 {
			doc.Packets = append(doc.Packets, stamp("video", 0, int64(i), 1))
		}
		// p содержит очередной AAC-пакет; первому назначается подтверждение Skip Samples.
		p := stamp("audio", 1, int64(i-1)*1024, 1024)
		if i == 0 {
			p.Side = []packetSide{{Type: "Skip Samples", Skip: 1024}}
		}
		doc.Packets = append(doc.Packets, p)
	}
	return doc
}

// Проверяет удаление только подтверждённого целого пакета задержки AAC, исправление av1C и точный
// период с явными либо вычисленными длительностями.
func TestPreparedNormalizationDropsOnlyProvenWholePrimingAndChecksPeriod(t *testing.T) {
	// durationAbsent выбирает наличие явных длительностей ffprobe либо их восстановление и сверку с
	// контейнером.
	for _, durationAbsent := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-duration", true: "codec-and-fmp4-duration"}[durationAbsent], func(t *testing.T) {
			// Проверяет удаление ровно первой AAC AU задержки кодировщика и петлю 1,6 с как при явных
			// длительностях ffprobe, так и при подтверждении длительностей контейнером.

			// doc содержит эталонный полный период для текущего варианта длительностей.
			doc := selfPreparedMetadata()
			if durationAbsent {
				// i задаёт позицию пакета, у которого удаляется явная длительность
				// ffprobe.
				for i := range doc.Packets {
					doc.Packets[i].Duration = nil
				}
			}
			// encoded содержит JSON меток текущего сценария. err сообщает об отказе
			// кодирования.
			encoded, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			// p содержит проверяющий порт с управляемым JSON и конечными бюджетами.
			p := Prober{Binary: "probe", Runner: preparedProbeRunner{packets: encoded}, Limits: selfProbeLimits()}
			// raw содержит исходный AV1-файл с устаревшим av1C и подтверждаемой задержкой
			// AAC.
			raw := selfRawAV1Counts(t, false, 48, 76)
			if _, err = p.Verify(context.Background(), raw, selfProfile(core.CodecAV1)); err == nil {
				t.Fatal("устаревший av1C принят до нормализации")
			}
			// output содержит временный результат нормализации для проверки пакетов и
			// периода.
			output, err := os.CreateTemp(t.TempDir(), "normalized")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, output)
			if err = p.NormalizePrepared(context.Background(), raw, &boundedWriter{w: output, remaining: 1 << 20}); err != nil {
				t.Fatal(err)
			}
			selfComparePackets(t, p, raw, output, true)
			if _, err = p.Verify(context.Background(), output, selfProfile(core.CodecAV1)); err != nil {
				t.Fatal(err)
			}
			if err = p.VerifyLoopDuration(context.Background(), output, 1600*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			if err = p.VerifyLoopDuration(context.Background(), output, time.Second); !errors.Is(err, core.ErrNotReady) {
				t.Fatal(err)
			}
		})
	}
}

// Проверяет отказ нормализации при неподтверждённых поправках, метках и длительностях, несогласованном
// периоде, повреждённом контейнере, отмене и превышении бюджетов.
func TestPreparedNormalizationRejectsUnknownPartialPaddingAndUnprovenTime(t *testing.T) {
	// scenario выбирает конкретное нарушение доказательства задержки, времени, контейнера или
	// бюджета.
	for _, scenario := range []string{"missing-side", "partial-skip", "discard-tail", "unknown-side", "missing-dts", "unknown-index", "truncated-json", "unequal-loop", "b-frame", "changed-probe-time", "output-budget", "cancel", "missing-sequence", "initialization-limit", "packet-limit", "truncated-raw", "missing-duration-wrong-container"} {
		t.Run(scenario, func(t *testing.T) {
			// Вносит выбранное нарушение доказательства задержки, времени, контейнера или бюджета и требует отказа
			// нормализации.

			// doc содержит изменяемую копию эталонных меток для выбранного нарушения.
			doc := selfPreparedMetadata()
			switch scenario {
			case "missing-side":
				doc.Packets[1].Side = nil
			case "partial-skip":
				doc.Packets[1].Side[0].Skip = 1000
			case "discard-tail":
				doc.Packets[1].Side[0].Discard = 1
			case "unknown-side":
				doc.Packets[1].Side[0].Type = "unknown"
			case "missing-duration-wrong-container":
				// i задаёт позицию пакета, у которого удаляется duration перед проверкой
				// неверного контейнера.
				for i := range doc.Packets {
					doc.Packets[i].Duration = nil
				}
			case "missing-dts":
				doc.Packets[0].DTS = nil
			case "unknown-index":
				doc.Packets[0].Index = 99
			case "unequal-loop":
				doc.Packets = doc.Packets[:len(doc.Packets)-1]
			case "b-frame":
				// pts задаёт несогласованное время показа первого AV1-кадра.
				pts := int64(1)
				doc.Packets[0].PTS = &pts
			case "changed-probe-time":
				// duration задаёт подменённую длительность первого видеопакета.
				duration := int64(2)
				doc.Packets[0].Duration = &duration
			}
			// encoded содержит JSON изменённых меток. err сообщает об отказе его
			// кодирования.
			encoded, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "truncated-json" {
				encoded = encoded[:len(encoded)/2]
			}
			// p содержит проверяющий порт с выбранным нарушением метаданных или ограничений.
			p := Prober{Binary: "probe", Runner: preparedProbeRunner{packets: encoded}, Limits: selfProbeLimits()}
			if scenario == "initialization-limit" {
				p.Limits.Container.MaxInitBytes = 500
				p.Limits.Container.MaxExtradataBytes = 64
			}
			if scenario == "packet-limit" {
				p.Limits.MaxPackets = 1
			}
			// ctx позволяет проверить отмену нормализации. cancel освобождает контекст либо
			// заранее отменяет его в соответствующем сценарии.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancel" {
				cancel()
			}
			// out принимает нормализованные байты до ожидаемого отказа.
			var out bytes.Buffer
			// limit задаёт бюджет выходного потока, уменьшаемый в сценарии превышения.
			limit := uint64(1 << 20)
			if scenario == "output-budget" {
				limit = 8
			}
			// raw содержит исходный fMP4, при необходимости без заголовка
			// последовательности.
			raw := selfRawAV1Counts(t, scenario == "missing-sequence", 48, 76)
			if scenario == "missing-duration-wrong-container" {
				if _, err = raw.Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				// data содержит все байты исходного контейнера для изменения
				// длительности первого trun. e сообщает об отказе чтения.
				data, e := io.ReadAll(raw)
				if e != nil {
					t.Fatal(e)
				}
				// at задаёт позицию имени первого trun в байтах контейнера для
				// контролируемой подмены duration.
				at := bytes.Index(data, []byte("trun"))
				if at < 0 || len(data)-at < 20 {
					t.Fatal("нет первого trun")
				}
				binary.BigEndian.PutUint32(data[at+16:at+20], 2)
				if _, e = raw.WriteAt(data, 0); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "truncated-raw" {
				// info содержит размер исходного файла перед усечением. e сообщает об
				// отказе чтения свойств.
				info, e := raw.Stat()
				if e != nil {
					t.Fatal(e)
				}
				if e = raw.Truncate(info.Size() - 10); e != nil {
					t.Fatal(e)
				}
			}
			err = p.NormalizePrepared(ctx, raw, &boundedWriter{w: &out, remaining: limit})
			if err == nil {
				t.Fatal("неподтверждённый padding или период принят")
			}
			if scenario == "output-budget" && !errors.Is(err, ErrLimit) {
				t.Fatal(err)
			}
		})
	}
}
