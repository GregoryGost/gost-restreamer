package group

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// TestGeneratedHDRPrefixRestoresSourceWithoutChangingAU проверяет согласование фактических OBU и блоков контейнера
// до публикации, сохраняя начальные аудиопакеты, полный TU, исходные метки и порядок дорожек.
// t проверяет происхождение HDR-сведений и побайтную сохранность первого кадра.
func TestGeneratedHDRPrefixRestoresSourceWithoutChangingAU(t *testing.T) {
	// template содержит буквальную av1C реального синтетического результата SVT/PQ10 в FFmpeg 7.1.5.
	// err сообщает об ошибке чтения фикстуры и последующих проверок её медиаданных.
	template, err := hex.DecodeString("81004d000a0e0200000419bfff00be1424402490")
	if err != nil {
		t.Fatal(err)
	}
	// payload содержит полный исходный первый TU с OBU метаданных MDCV/CLL.
	payload, err := hex.DecodeString("0a0e0200000419bfff12be54244024902a060103e80190802a1a02ae1451ec43d7b0a426660f5c500d54390003e800000000028032e80810008d008249104050000800db2af56409f9ca2129d22cd4eb164230a79c2c498b7459533f4f03a0bdb5e0eae2a42a8682780779d4f7756c486e7e473090a69b97dce80efaf84543ee241cae1083461d483a329fb87dc7de7004d4a8ce528f7b1146cd69d894c833b548a5786789cd0a10eb18ee32c1b29f64142c599297edb23bf6d53f2cce61ef5e9f50310e026d73c2ba4e0d9039ed02b6e7aec27cca8fc9180952e3b154bd17e0c105392ccf4eb600eadf02b6c214bea6d2767aea9e99b270d7fdbef081d05f4f8037c647b9d5ff4ec4f676f9e3f3c9b1d4c2fd116363aa324265f9115c985a27b28b3e49eea342b059990243e3808fe6b0a390140ed1610035c5086cc25656e63b476f739597daff806a0f52087270cdc96083d8db78b6f87507fbd1f4be62c5e24ffbe17304e32f76373902026b6a9ef76fe3fb82e28098f5898e49b0ad7d52bee42b24585364f2c118b5fd5dcdced2fcfad97f242e7a71ffc5ae8103949aa20051cdc52d9baec2fcc6eec3dc56417457ebf6bf8243a4a610cd3894ea06095b27728ed2425bba2f867c750c0ebb0be59370dd703636a8873869dc85c6225e2c8744ad5a3498ab8c290f5239f852f67b1a2ed3919861110e1936e96ef217afd1826bf28bf9d5bf247d34d0494b06ccfc5f0f3306d91475a1926c6cb62f7c8a8928f0e376a80348d9413ae5be127b303da64130aed893e9943e72a0449527096e7ec2c42c12c39e5cf9516ad964dbec78acbe8be8d48db5b5704ee3c78c995a9ce44213f8a48b20c10d928387faa14106deb0b69fe9e37ca66ba1ac53862e74fd0bfb3d5c55c3ed632b86e5ccea7929ccb2bdc6c1a7bcfd5502a039e2ed81c813db03d2fd157e9c4b4f2add06eea15120d8bf2cf7447277423066a299fd2f4cb52b84a365bc2c84466e3e67ca14f5989ed2bab3f4fb05be3dedb3c41e6d6b704895976111bfdcc6c4faa948f0855d7cb327ff3a646acb7a3188e9366fedc6be3263affc132dd9096bb7b1d43ba6e73b3488a08bf4276eda7ec76080bd03deb30afa7989eecd965894f263b0958854c452c104cfb23282b5e5cb28917fe5452e29e3492e99a20c21a7f283c968364f3f6967a807daf1efda6323924a4316909e776922dacef0d03aacb0f9978a6d48b3f7d2a72ac4605753772de57e5ae8d0e2afa0cbd48363abcfc77ba22e1198d7e8e079d00deec0c59ba05e031b6441a1a46613206afa67cd9cbddb3ed9b51b1a1b02ef9455a4f56bc7f83f6bee6c3d504543f5a59921a59d87b3bf6707abfc5fec812ef7177b4469fd12a7d97b95b2cbbb5eed9032e5c0d658a73459b163941d7753b47ef18153fe2ed6be291fbb4ddce823265c2039c2af02c916a644635844fa613dc7f3473c054fdbe18bce699a499eacd407f97254d07c5cdd3dc5de00bd98ab2975061f8616281d8cf1b44fcee5733f142a3772c8940879ab561bf0e96df6fb6e6224e8b9cc46bcac3752b7a4d6b002fbbb507d2cd267b21dbba2911c5fb9fe379451f9a0633579ecf07f626242f64cfcbf84183d9c91ff27bf74c9a8fe6c7414e86ed99f0278f3233c36983b5b72")
	if err != nil {
		t.Fatal(err)
	}
	// headers и actual получены из настоящего заголовка последовательности первого TU; нормализация сохраняет OBU метаданных.
	headers, actual, err := codec.AV1ConfigFromTUWithMetadata(template, payload, core.FrameRate{Numerator: 12, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	// state отделяет фактические внутрипоточные сведения от описания желаемого профиля.
	var state codec.HDRState
	// source извлечён из исходного TU и не зависит от записи и повторного чтения контейнера.
	source, err := state.Observe(core.CodecAV1, headers, payload, true)
	if err != nil {
		t.Fatal(err)
	}
	// fixture содержит настройки переключателя и профиля, audio — корректную конфигурацию AAC без запуска работников.
	fixture, _, audio, _ := switcherFixture(t)
	// profile описывает собственный HDR-профиль, нужен для границы восстановления.
	profile := fixture.config.Profile
	profile.Format.Video = actual
	profile.Mode, profile.LiveAudioMode = core.ModeCopy, core.LiveAudioCopy
	profile.Encoding = core.DefaultEncoding(profile.Mode, core.VideoEncoderSVTAV1, core.RateControlCRF)
	profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 3500}
	profile.VideoEncoding.TargetBitrateBPS, profile.VideoEncoding.MaxBitrateBPS, profile.VideoEncoding.BufferBits = 0, 0, 0
	if err = profile.Validate(); err != nil {
		t.Fatal(err)
	}
	// video связывает actual с эпохой аудио и общей точной шкалой.
	video, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: audio.Info().SourceEpoch, Revision: 1, TimeBase: audio.Info().TimeBase, Video: actual, HDR: source}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// init задаёт собственный fMP4, limits сохраняет прежние конечные бюджеты пакетов и блоков.
	init := fmp4.Init{Video: video, Audio: audio}
	limits := fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 65536, MaxMdatBytes: 1 << 20, MaxSampleBytes: 65536, MaxExtradataBytes: 4096, MaxSamples: 100}
	// storage хранит отдельные байты контейнера; mux записывает в него сжатые AU без изменения полезной нагрузки.
	var storage bytes.Buffer
	mux, err := fmp4.NewMuxer(&storage, init, limits)
	if err != nil {
		t.Fatal(err)
	}
	// audioPacket и videoPacket задают начальный аудиопакет перед первым независимым видео.
	audioPacket, err := media.NewPacket(media.PacketInfo{PTS: 0, DTS: 0, DurationTicks: 1024}, audio, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c})
	if err != nil {
		t.Fatal(err)
	}
	videoPacket, err := media.NewPacket(media.PacketInfo{PTS: 0, DTS: 0, DurationTicks: 4000, Keyframe: true}, video, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = mux.WritePacket(audioPacket); err != nil {
		t.Fatal(err)
	}
	if err = mux.WritePacket(videoPacket); err != nil {
		t.Fatal(err)
	}
	// demux восстанавливает HDR во время выполнения из стандартных блоков и заголовков, без доверия UUID-метаданным.
	demux, err := fmp4.NewDemuxer(bytes.NewReader(storage.Bytes()), fmp4.DemuxOptions{Limits: limits, FreshHDRSource: &source})
	if err != nil {
		t.Fatal(err)
	}
	// resolved содержит согласованный снимок, prefix — исходные пакеты, container — фактические метаданные контейнера.
	resolved, prefix, container, err := completeGeneratedHDR(context.Background(), demux, profile, source, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefix) != 2 || prefix[0].Config().Info().Track != media.TrackAudio || prefix[1].Config().Info().Track != media.TrackVideo || !bytes.Equal(prefix[1].Payload(), payload) || prefix[1].Info() != videoPacket.Info() {
		t.Fatal("Потерян исходный prefix, AU либо времена")
	}
	if err = codec.MatchHDRProjection(source, container); err != nil {
		t.Fatal(err)
	}
	// resolvedHDR сохраняет полные исходные MDCV/CLL и отдельное происхождение фактического nclx.
	resolvedHDR := resolved.Video.Info().HDR
	if resolvedHDR.MDCV != source.MDCV || resolvedHDR.CLL != source.CLL {
		t.Fatal("Actual OBU authority заменена округлённым контейнером")
	}
	if resolvedHDR.ColorConfig != (media.ColorConfigProvenance{Source: media.HDRSourceContainer, Primaries: true, Transfer: true, Matrix: true}) || resolved.Video.Info().Video.Color != (core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}) {
		t.Fatal("Потеряны фактические CICP либо происхождение nclx", resolved.Video.Info())
	}
	// restoredPacket перепривязывает только конфигурацию; байты и время первого AU остаются исходными.
	restoredPacket, err := generatedPacketConfig(prefix[1], resolved)
	if err != nil || restoredPacket.Config().Info().HDR != resolvedHDR || restoredPacket.Config().Info().Video.Color != resolved.Video.Info().Video.Color || !bytes.Equal(restoredPacket.Payload(), payload) || restoredPacket.Info() != videoPacket.Info() {
		t.Fatal("Перепривязка изменила HDR, AU либо время", err)
	}
}
