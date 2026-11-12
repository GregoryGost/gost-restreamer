package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// packetSide принимает дополнительные сведения ffprobe о пропускаемых сэмплах аудиопакета.
type packetSide struct {
	// Type содержит обозначение дополнительных сведений ffprobe, в том числе Skip Samples.
	Type string `json:"side_data_type"`
	// Skip задаёт число сэмплов, пропускаемых в начале пакета.
	Skip uint64 `json:"skip_samples"`
	// Discard задаёт число сэмплов заполнения, отбрасываемых в конце пакета.
	Discard uint64 `json:"discard_padding"`
}

// packetStamp принимает временные метки и дополнительные сведения об одном пакете ffprobe.
type packetStamp struct {
	// Type содержит тип дорожки пакета, сообщённый ffprobe.
	Type string `json:"codec_type"`
	// Index связывает пакет с индексом потока ffprobe.
	Index int `json:"stream_index"`
	// PTS содержит время показа в отсчётах потока либо nil, если ffprobe его не сообщил.
	PTS *int64 `json:"pts"`
	// DTS содержит время декодирования в отсчётах потока либо nil, если ffprobe его не сообщил.
	DTS *int64 `json:"dts"`
	// Duration содержит длительность в отсчётах потока либо nil при отсутствии значения ffprobe.
	Duration *int64 `json:"duration"`
	// Side содержит дополнительные сведения ffprobe о пропуске или отбрасывании сэмплов.
	Side []packetSide `json:"side_data_list"`
}

// packetDocument принимает ограниченный JSON ffprobe со списками потоков и пакетов.
type packetDocument struct {
	// Streams содержит индексы, типы и шкалы времени обнаруженных потоков.
	Streams []struct {
		// Index обозначает индекс потока ffprobe для связи с пакетами.
		Index int `json:"index"`
		// Type обозначает видео- или аудиопоток.
		Type string `json:"codec_type"`
		// Base содержит рациональную запись time_base потока.
		Base string `json:"time_base"`
	} `json:"streams"`
	// Packets содержит метки пакетов и дополнительные сведения в порядке вывода ffprobe.
	Packets []packetStamp `json:"packets"`
}

// preparedTiming хранит проверенные временные метки ffprobe отдельно для каждой дорожки.
type preparedTiming struct {
	// packets содержит последовательности пакетов видео и аудио, включая первый пакет задержки AAC.
	packets map[media.Track][]packetStamp
	// bases содержит единицы времени, в которых записаны метки пакетов каждой дорожки.
	bases map[media.Track]media.TimeBase
}

// Переводит знаковую метку в точное рациональное время без округления.
func ticks(t int64, b media.TimeBase) *big.Rat {
	return new(big.Rat).Mul(new(big.Rat).SetInt64(t), new(big.Rat).SetFrac64(int64(b.Numerator), int64(b.Denominator)))
}

// Собирает ограниченные метки ffprobe и подтверждает удаляемый целый пакет задержки AAC, отсутствие
// прочих поправок и полный общий период; недостающую duration вычисляет для последующей сверки с самим
// fMP4.
func (p Prober) inspectPreparedTiming(ctx context.Context, input *os.File, actual core.MediaFormat) (*preparedTiming, error) {
	// err сообщает об отказе перемотки файла перед запуском ffprobe.
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// out принимает ограниченный JSON ffprobe с временными метками.
	var out bytes.Buffer
	// cmd содержит команду ffprobe для чтения фактических меток без синтетического заполнения.
	cmd := Command{Binary: p.Binary, Args: []string{"-v", "error", "-protocol_whitelist", "fd,pipe", "-format_whitelist", "mov", "-fflags", "+nofillin", "-fd", "3", "-i", "fd:", "-show_packets", "-show_streams", "-show_entries", "packet=codec_type,stream_index,pts,dts,duration:packet_side_data=side_data_type,skip_samples,discard_padding:stream=index,codec_type,time_base", "-of", "json"}}
	// err сообщает об отказе запуска ffprobe или превышении бюджета его вывода.
	if _, err := p.Runner.Run(ctx, cmd, input, &boundedWriter{w: &out, remaining: p.Limits.MaxJSONBytes}); err != nil {
		return nil, err
	}
	// doc принимает разобранный JSON с описаниями потоков и пакетов.
	var doc packetDocument
	// err сообщает о некорректном JSON ffprobe.
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		return nil, err
	}
	if len(doc.Streams) != 2 || len(doc.Packets) == 0 || uint64(len(doc.Packets)) > p.Limits.MaxPackets {
		return nil, ErrLimit
	}
	// plan накапливает проверенные шкалы и пакеты каждой дорожки.
	plan := &preparedTiming{packets: map[media.Track][]packetStamp{}, bases: map[media.Track]media.TimeBase{}}
	// indices связывает уникальные индексы потоков ffprobe с типами дорожек.
	indices := map[int]media.Track{}
	// stream содержит очередное описание потока ffprobe.
	for _, stream := range doc.Streams {
		// track содержит тип дорожки, полученный из codec_type потока.
		track := media.Track(stream.Type)
		if track != media.TrackVideo && track != media.TrackAudio {
			return nil, ErrInvalid
		}
		// ok показывает, зарегистрирована ли уже дорожка такого типа.
		if _, ok := plan.bases[track]; ok {
			return nil, ErrInvalid
		}
		// ok показывает, зарегистрирован ли уже такой индекс потока.
		if _, ok := indices[stream.Index]; ok {
			return nil, ErrInvalid
		}
		// rate содержит разобранную дробь time_base. err сообщает о некорректной записи шкалы.
		rate, err := parseRate(stream.Base)
		if err != nil {
			return nil, err
		}
		plan.bases[track] = media.TimeBase{Numerator: rate.Numerator, Denominator: rate.Denominator}
		indices[stream.Index] = track
	}
	// packet содержит очередную запись пакета ffprobe для проверки и распределения по дорожкам.
	for _, packet := range doc.Packets {
		// track содержит дорожку найденного индекса потока. ok показывает, зарегистрирован ли
		// такой индекс.
		track, ok := indices[packet.Index]
		if ok && packet.Duration == nil {
			// ffprobe без синтетического заполнения не всегда сообщает duration.
			// Ожидание вычисляется из формата и обязательно сверяется с duration самого fMP4.
			// base содержит шкалу потока для восстановления отсутствующей длительности.
			base := plan.bases[track]
			// numerator содержит числитель длительности полной единицы доступа в секундах.
			// denominator содержит знаменатель этой длительности для AAC или видео.
			numerator, denominator := uint64(1024), uint64(actual.Audio.SampleRateHz)
			if track == media.TrackVideo {
				numerator, denominator = uint64(actual.Video.FrameRate.Denominator), uint64(actual.Video.FrameRate.Numerator)
			}
			// n содержит числитель длительности после перевода в отсчёты дорожки без
			// переполнения.
			n := new(big.Int).Mul(new(big.Int).SetUint64(numerator), new(big.Int).SetUint64(uint64(base.Denominator)))
			// d содержит знаменатель длительности после перевода в отсчёты дорожки без
			// переполнения.
			d := new(big.Int).Mul(new(big.Int).SetUint64(denominator), new(big.Int).SetUint64(uint64(base.Numerator)))
			// q содержит целую длительность в отсчётах. r содержит остаток деления для
			// проверки точной представимости.
			q, r := new(big.Int), new(big.Int)
			q.QuoRem(n, d, r)
			if r.Sign() != 0 || !q.IsInt64() {
				return nil, core.ErrCorrupt
			}
			// duration содержит восстановленную целую длительность пакета в отсчётах потока.
			duration := q.Int64()
			packet.Duration = &duration
		}
		if !ok || string(track) != packet.Type || packet.PTS == nil || packet.DTS == nil || packet.Duration == nil || *packet.Duration <= 0 {
			return nil, fmt.Errorf("%w: ffprobe не подтвердил время packet %s index %d", core.ErrCorrupt, packet.Type, packet.Index)
		}
		plan.packets[track] = append(plan.packets[track], packet)
	}
	// audio содержит проверяемую последовательность аудиопакетов. video содержит проверяемую
	// последовательность видеопакетов.
	audio, video := plan.packets[media.TrackAudio], plan.packets[media.TrackVideo]
	if len(audio) < 2 || len(video) == 0 {
		return nil, core.ErrNotReady
	}
	// first содержит первый аудиопакет, который должен целиком соответствовать задержке
	// AAC-кодировщика.
	first := audio[0]
	if len(first.Side) != 1 || first.Side[0].Type != "Skip Samples" || first.Side[0].Skip != 1024 || first.Side[0].Discard != 0 || !exactAudioDuration(*first.Duration, plan.bases[media.TrackAudio], actual.Audio.SampleRateHz) {
		return nil, fmt.Errorf("%w: не подтверждён целый пакет задержки AAC-кодировщика", core.ErrNotReady)
	}
	// firstEnd содержит точный конец первого аудиопакета в секундах; допустимое значение равно нулю.
	firstEnd := new(big.Rat).Add(ticks(*first.DTS, plan.bases[media.TrackAudio]), ticks(*first.Duration, plan.bases[media.TrackAudio]))
	if *first.PTS != *first.DTS || firstEnd.Sign() != 0 || *audio[1].DTS != 0 {
		return nil, fmt.Errorf("%w: неподдержанное начальное смещение A/V", core.ErrNotReady)
	}
	// period проверяет непрерывность и общий полный период после исключения задержки AAC.
	period := newPreparedPeriod(actual)
	// track обозначает проверяемую дорожку. packets содержит все записи ffprobe этой дорожки.
	for track, packets := range plan.packets {
		// start задаёт индекс первого пакета периода, пропуская только задержку AAC.
		start := 0
		if track == media.TrackAudio {
			start = 1
		}
		// packet содержит очередной пакет периода после возможного пропуска задержки.
		for _, packet := range packets[start:] {
			if len(packet.Side) != 0 {
				return nil, core.ErrNotReady
			}
			// err сообщает о разрыве меток, неполной единице или нарушении начальных условий
			// дорожки.
			if err := period.add(track, plan.bases[track], *packet.PTS, *packet.DTS, *packet.Duration); err != nil {
				return nil, err
			}
		}
	}
	// err сообщает об отсутствии общего полного периода видео и аудио.
	if _, err := period.duration(); err != nil {
		return nil, err
	}
	return plan, nil
}

// Сверяет PTS, DTS и длительность очередного пакета fMP4 с соответствующей записью ffprobe в точном
// рациональном времени.
func (t *preparedTiming) match(packet media.Packet, index int) error {
	// track определяет, с какой последовательностью ffprobe сравнивается пакет.
	track := packet.Config().Info().Track
	if index >= len(t.packets[track]) {
		return core.ErrCorrupt
	}
	// expected содержит запись ffprobe с тем же номером пакета внутри дорожки.
	expected := t.packets[track][index]
	// info содержит фактические метки и длительность пакета из fMP4.
	info := packet.Info()
	// base задаёт единицу времени фактического пакета fMP4.
	base := packet.Config().Info().TimeBase
	// probeBase задаёт единицу времени соответствующей записи ffprobe.
	probeBase := t.bases[track]
	// pair содержит фактическое и ожидаемое значения одной метки либо длительности для точного
	// сравнения.
	for _, pair := range [][2]int64{{info.PTS, *expected.PTS}, {info.DTS, *expected.DTS}, {info.DurationTicks, *expected.Duration}} {
		if ticks(pair[0], base).Cmp(ticks(pair[1], probeBase)) != 0 {
			return fmt.Errorf("%w: %s[%d] fMP4 %s != ffprobe %s", core.ErrCorrupt, track, index, ticks(pair[0], base), ticks(pair[1], probeBase))
		}
	}
	return nil
}
