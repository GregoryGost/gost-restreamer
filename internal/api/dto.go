package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

type Decimal uint64

func (d Decimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(d), 10))
}
func (d *Decimal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return core.ErrInvalid
	}
	n, err := parseDecimal(s)
	if err != nil {
		return err
	}
	*d = Decimal(n)
	return nil
}
func (Decimal) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: "string", Pattern: "^(0|[1-9][0-9]{0,19})$", Description: "Точное беззнаковое целое в десятичной записи."}
}
func parseDecimal(s string) (uint64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, core.ErrInvalid
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, core.ErrInvalid
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, core.ErrInvalid
	}
	return v, nil
}
func revision(s string) (uint64, error) {
	v, err := parseDecimal(s)
	if err != nil || v == 0 {
		return 0, core.ErrInvalid
	}
	return v, nil
}

type ProfileText string

func (p ProfileText) MarshalJSON() ([]byte, error) {
	s := string(p)
	if utf8.ValidString(s) {
		return json.Marshal(s)
	}
	return json.Marshal(struct {
		Encoding string `json:"encoding"`
		Value    string `json:"value"`
	}{"base64", base64.StdEncoding.EncodeToString([]byte(s))})
}
func (p *ProfileText) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if !utf8.Valid(b) || json.Unmarshal(b, &s) != nil {
			return core.ErrInvalid
		}
		*p = ProfileText(s)
		return nil
	}
	var v struct {
		Encoding string `json:"encoding"`
		Value    string `json:"value"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&v) != nil || v.Encoding != "base64" {
		return core.ErrInvalid
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(v.Value)
	if err != nil {
		return core.ErrInvalid
	}
	*p = ProfileText(raw)
	return nil
}
func (ProfileText) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{OneOf: []*huma.Schema{{Type: "string"}, {Type: "object", Properties: map[string]*huma.Schema{"encoding": {Type: "string", Enum: []any{"base64"}}, "value": {Type: "string", ContentEncoding: "base64"}}, Required: []string{"encoding", "value"}, AdditionalProperties: false}}}
}

type Field[T any] struct {
	Set, Null bool
	Value     T
}

func (f *Field[T]) UnmarshalJSON(b []byte) error {
	f.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		f.Null = true
		return nil
	}
	return json.Unmarshal(b, &f.Value)
}
func (Field[T]) Schema(r huma.Registry) *huma.Schema {
	var zero T
	return huma.SchemaFromType(r, reflect.TypeOf(zero))
}
func (f Field[T]) apply(to *T) error {
	if !f.Set {
		return nil
	}
	if f.Null {
		return core.ErrInvalid
	}
	*to = f.Value
	return nil
}

type RecordDTO struct {
	ID        string    `json:"id"`
	Revision  Decimal   `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func recordDTO(r core.Record) RecordDTO {
	return RecordDTO{string(r.ID), Decimal(r.Revision), r.CreatedAt.UTC(), r.UpdatedAt.UTC()}
}

type FailureDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func failure(code string) *FailureDTO {
	if code == "" {
		return nil
	}
	return &FailureDTO{code, "Операция не завершена успешно. Подробности доступны в журнале приложения."}
}

type WatchdogDTO struct {
	VideoTimeoutNS    Decimal `json:"video_timeout_ns"`
	AudioTimeoutNS    Decimal `json:"audio_timeout_ns"`
	RecoveryWindowNS  Decimal `json:"recovery_window_ns"`
	KeyframeTimeoutNS Decimal `json:"keyframe_timeout_ns"`
}

func (w WatchdogDTO) domain() (core.WatchdogPolicy, error) {
	for _, n := range []Decimal{w.VideoTimeoutNS, w.AudioTimeoutNS, w.RecoveryWindowNS, w.KeyframeTimeoutNS} {
		if n == 0 || uint64(n) > uint64(1<<63-1) {
			return core.WatchdogPolicy{}, core.ErrInvalid
		}
	}
	return core.WatchdogPolicy{VideoTimeout: time.Duration(w.VideoTimeoutNS), AudioTimeout: time.Duration(w.AudioTimeoutNS), RecoveryWindow: time.Duration(w.RecoveryWindowNS), KeyframeTimeout: time.Duration(w.KeyframeTimeoutNS)}, nil
}

type PolicyDTO struct {
	AssetID  string      `json:"asset_id"`
	Watchdog WatchdogDTO `json:"watchdog"`
}

func (p PolicyDTO) domain() (core.FallbackPolicy, error) {
	w, err := p.Watchdog.domain()
	if err != nil {
		return core.FallbackPolicy{}, err
	}
	return core.FallbackPolicy{AssetID: core.ID(p.AssetID), Watchdog: w}, nil
}
func policyDTO(p core.FallbackPolicy) *PolicyDTO {
	if p.AssetID == "" {
		return nil
	}
	return &PolicyDTO{string(p.AssetID), WatchdogDTO{Decimal(p.Watchdog.VideoTimeout), Decimal(p.Watchdog.AudioTimeout), Decimal(p.Watchdog.RecoveryWindow), Decimal(p.Watchdog.KeyframeTimeout)}}
}

type StreamDTO struct {
	RecordDTO
	Name           string     `json:"name"`
	PublicationKey string     `json:"publication_key" doc:"Сохранённый постоянный ключ публикации OBS. В Session возвращается ключ из снимка запуска."`
	Fallback       *PolicyDTO `json:"fallback"`
}

func streamDTO(s core.Stream) StreamDTO {
	return StreamDTO{recordDTO(s.Record), s.Name, s.PublicationKey, policyDTO(s.Fallback)}
}

type DestinationDTO struct {
	RecordDTO
	StreamID  string `json:"stream_id"`
	GroupID   string `json:"group_id"`
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Endpoint  string `json:"endpoint" doc:"Полный сохранённый адрес назначения, включая учётные данные и параметры запроса. В Output возвращается адрес из снимка выхода."`
	StreamKey string `json:"stream_key" doc:"Сохранённый ключ назначения. Пустая строка допустима. В Output возвращается ключ из снимка выхода."`
	Enabled   bool   `json:"enabled"`
}

func destinationDTO(d core.Destination) DestinationDTO {
	return DestinationDTO{recordDTO(d.Record), string(d.StreamID), string(d.GroupID), d.Name, d.Platform, d.Endpoint, d.StreamKey, d.Enabled}
}

type FrameRateDTO struct {
	Numerator   uint32 `json:"numerator"`
	Denominator uint32 `json:"denominator"`
}
type ColorDTO struct {
	Range     ProfileText `json:"range"`
	Primaries ProfileText `json:"primaries"`
	Transfer  ProfileText `json:"transfer"`
	Matrix    ProfileText `json:"matrix"`
}
type VideoFormatDTO struct {
	Codec        string       `json:"codec"`
	Width        uint32       `json:"width"`
	Height       uint32       `json:"height"`
	FrameRate    FrameRateDTO `json:"frame_rate"`
	CodecProfile ProfileText  `json:"codec_profile"`
	Level        ProfileText  `json:"level"`
	Tier         ProfileText  `json:"tier"`
	PixelFormat  ProfileText  `json:"pixel_format"`
	BitDepth     uint8        `json:"bit_depth"`
	Color        ColorDTO     `json:"color"`
}
type AudioFormatDTO struct {
	Codec         string      `json:"codec"`
	CodecProfile  ProfileText `json:"codec_profile"`
	SampleRateHz  uint32      `json:"sample_rate_hz"`
	Channels      uint16      `json:"channels"`
	ChannelLayout ProfileText `json:"channel_layout"`
}
type FormatDTO struct {
	Video VideoFormatDTO `json:"video"`
	Audio AudioFormatDTO `json:"audio"`
}

func formatDTO(f core.MediaFormat) FormatDTO {
	v, a := f.Video, f.Audio
	return FormatDTO{VideoFormatDTO{string(v.Codec), v.Width, v.Height, FrameRateDTO{v.FrameRate.Numerator, v.FrameRate.Denominator}, ProfileText(v.CodecProfile), ProfileText(v.Level), ProfileText(v.Tier), ProfileText(v.PixelFormat), v.BitDepth, ColorDTO{ProfileText(v.Color.Range), ProfileText(v.Color.Primaries), ProfileText(v.Color.Transfer), ProfileText(v.Color.Matrix)}}, AudioFormatDTO{string(a.Codec), ProfileText(a.CodecProfile), a.SampleRateHz, a.Channels, ProfileText(a.ChannelLayout)}}
}
func (f FormatDTO) domain() core.MediaFormat {
	v, a := f.Video, f.Audio
	return core.MediaFormat{Video: core.VideoFormat{Codec: core.Codec(v.Codec), Width: v.Width, Height: v.Height, FrameRate: core.FrameRate{Numerator: v.FrameRate.Numerator, Denominator: v.FrameRate.Denominator}, CodecProfile: string(v.CodecProfile), Level: string(v.Level), Tier: string(v.Tier), PixelFormat: string(v.PixelFormat), BitDepth: v.BitDepth, Color: core.ColorInfo{Range: string(v.Color.Range), Primaries: string(v.Color.Primaries), Transfer: string(v.Color.Transfer), Matrix: string(v.Color.Matrix)}}, Audio: core.AudioFormat{Codec: core.Codec(a.Codec), CodecProfile: string(a.CodecProfile), SampleRateHz: a.SampleRateHz, Channels: a.Channels, ChannelLayout: string(a.ChannelLayout)}}
}

type VideoEncodingDTO struct {
	TargetBitrateBPS Decimal `json:"target_bitrate_bps"`
	MaxBitrateBPS    Decimal `json:"max_bitrate_bps"`
	BufferBits       Decimal `json:"buffer_bits"`
	GOPFrames        uint32  `json:"gop_frames"`
	MaxBFrames       uint32  `json:"max_b_frames"`
	ClosedGOP        bool    `json:"closed_gop"`
}
type AudioEncodingDTO struct {
	BitrateBPS Decimal `json:"bitrate_bps"`
}
type GroupProfileDTO struct {
	Mode          string           `json:"mode" enum:"copy,transcode"`
	LiveAudioMode string           `json:"live_audio_mode,omitempty" enum:"copy,transcode"`
	Format        FormatDTO        `json:"format"`
	VideoEncoding VideoEncodingDTO `json:"video_encoding"`
	AudioEncoding AudioEncodingDTO `json:"audio_encoding"`
	Encoding      *EncodingDTO     `json:"encoding" doc:"Материализованные настройки кодировщика. null обозначает прежний видеорецепт; выбор LIVE-аудио задаётся отдельно."`
}

func (p GroupProfileInput) domain() (core.GroupProfile, error) {
	var liveAudioMode core.LiveAudioMode
	if p.LiveAudioMode.Set {
		if p.LiveAudioMode.Null || p.LiveAudioMode.Value != string(core.LiveAudioCopy) && p.LiveAudioMode.Value != string(core.LiveAudioTranscode) {
			return core.GroupProfile{}, profileInputError{"profile.live_audio_mode", "Требуется явное значение copy или transcode."}
		}
		liveAudioMode = core.LiveAudioMode(p.LiveAudioMode.Value)
	}
	encoding, err := p.Encoding.domain(core.GroupMode(p.Mode))
	if err != nil {
		return core.GroupProfile{}, err
	}
	return core.GroupProfile{Mode: core.GroupMode(p.Mode), LiveAudioMode: liveAudioMode, Format: p.Format.domain(), VideoEncoding: core.VideoEncoding{TargetBitrateBPS: uint64(p.VideoEncoding.TargetBitrateBPS), MaxBitrateBPS: uint64(p.VideoEncoding.MaxBitrateBPS), BufferBits: uint64(p.VideoEncoding.BufferBits), GOPFrames: p.VideoEncoding.GOPFrames, MaxBFrames: p.VideoEncoding.MaxBFrames, ClosedGOP: p.VideoEncoding.ClosedGOP}, AudioEncoding: core.AudioEncoding{BitrateBPS: uint64(p.AudioEncoding.BitrateBPS)}, Encoding: encoding}, nil
}
func groupProfileDTO(p core.GroupProfile) GroupProfileDTO {
	v := p.VideoEncoding
	return GroupProfileDTO{string(p.Mode), string(p.LiveAudioMode), formatDTO(p.Format), VideoEncodingDTO{Decimal(v.TargetBitrateBPS), Decimal(v.MaxBitrateBPS), Decimal(v.BufferBits), v.GOPFrames, v.MaxBFrames, v.ClosedGOP}, AudioEncodingDTO{Decimal(p.AudioEncoding.BitrateBPS)}, encodingDTO(p.Encoding)}
}

type ProfileDTO struct {
	RecordDTO
	Name    string          `json:"name"`
	Profile GroupProfileDTO `json:"profile"`
}

func profileDTO(p core.TranscodeProfile) ProfileDTO {
	return ProfileDTO{recordDTO(p.Record), p.Name, groupProfileDTO(p.Profile)}
}

type GroupDTO struct {
	RecordDTO
	StreamID  string `json:"stream_id"`
	ProfileID string `json:"profile_id"`
	Name      string `json:"name"`
}

func groupDTO(g core.TranscodeGroup) GroupDTO {
	return GroupDTO{recordDTO(g.Record), string(g.StreamID), string(g.ProfileID), g.Name}
}

type GroupSnapshotDTO struct {
	Group   GroupDTO   `json:"group"`
	Profile ProfileDTO `json:"profile"`
}
type SessionDTO struct {
	RecordDTO
	StreamID      string             `json:"stream_id"`
	State         string             `json:"state"`
	DesiredPaused bool               `json:"desired_paused"`
	Stream        StreamDTO          `json:"stream"`
	Groups        []GroupSnapshotDTO `json:"groups"`
	EndedAt       *time.Time         `json:"ended_at"`
	Failure       *FailureDTO        `json:"failure"`
}

func sessionDTO(s core.Session) SessionDTO {
	groups := make([]GroupSnapshotDTO, 0, len(s.Snapshot.Groups))
	for _, g := range s.Snapshot.Groups {
		groups = append(groups, GroupSnapshotDTO{groupDTO(g.Group), profileDTO(g.Profile)})
	}
	var f *FailureDTO
	if s.State == core.SessionFailed {
		f = failure("session_failed")
	}
	if s.State == core.SessionInterrupted {
		f = failure("session_interrupted")
	}
	return SessionDTO{recordDTO(s.Record), string(s.StreamID), string(s.State), s.DesiredPaused, streamDTO(s.Snapshot.Stream), groups, utcPointer(s.EndedAt), f}
}

type OutputDTO struct {
	RecordDTO
	SessionID      string         `json:"session_id"`
	Destination    DestinationDTO `json:"destination"`
	Group          GroupDTO       `json:"group"`
	Profile        ProfileDTO     `json:"profile"`
	Fallback       *PolicyDTO     `json:"fallback"`
	DesiredRunning bool           `json:"desired_running"`
	Attached       bool           `json:"attached"`
}

func outputDTO(o core.SessionOutput) OutputDTO {
	return OutputDTO{recordDTO(o.Record), string(o.SessionID), destinationDTO(o.Snapshot.Destination), groupDTO(o.Snapshot.Group), profileDTO(o.Snapshot.Profile), policyDTO(o.Snapshot.Fallback), o.DesiredRunning, o.Attached}
}

type ReceiptDTO struct {
	RecordDTO
	RequestID string      `json:"request_id"`
	Kind      string      `json:"kind"`
	TargetID  string      `json:"target_id"`
	SessionID string      `json:"session_id"`
	OutputID  string      `json:"output_id"`
	Status    string      `json:"status"`
	Failure   *FailureDTO `json:"failure"`
}

func receiptDTO(r core.CommandReceipt) ReceiptDTO {
	var f *FailureDTO
	if r.Status == core.CommandFailed {
		f = failure("command_failed")
	}
	if r.Status == core.CommandInterrupted {
		f = failure("command_interrupted")
	}
	return ReceiptDTO{recordDTO(r.Record), r.RequestID, string(r.Kind), string(r.TargetID), string(r.SessionID), string(r.OutputID), string(r.Status), f}
}

type AssetDTO struct {
	RecordDTO
	Name                string  `json:"name"`
	Kind                string  `json:"kind"`
	State               string  `json:"state"`
	CurrentGenerationID string  `json:"current_generation_id"`
	PendingGenerationID string  `json:"pending_generation_id"`
	SizeBytes           Decimal `json:"size_bytes"`
}

func assetDTO(v core.FallbackAsset) AssetDTO {
	return AssetDTO{recordDTO(v.Record), v.Name, string(v.Kind), string(v.State), string(v.CurrentGenerationID), string(v.PendingGenerationID), Decimal(v.SizeBytes)}
}

type GenerationDTO struct {
	RecordDTO
	AssetID   string      `json:"asset_id"`
	Kind      string      `json:"kind"`
	Status    string      `json:"status"`
	SizeBytes Decimal     `json:"size_bytes"`
	Failure   *FailureDTO `json:"failure"`
}

func generationDTO(v core.FallbackGeneration) GenerationDTO {
	status := "error"
	var f *FailureDTO
	switch v.State {
	case core.PreparationQueued:
		status = "queued"
	case core.PreparationClaimed, core.PreparationIssued:
		status = "preparing"
	case core.PreparationPublished:
		status = "ready"
	case core.PreparationFailed:
		f = failure("preparation_failed")
	case core.PreparationInterrupted:
		f = failure("preparation_interrupted")
	}
	return GenerationDTO{recordDTO(v.Record), string(v.AssetID), string(v.Kind), status, Decimal(v.SizeBytes), f}
}

type VariantDTO struct {
	RecordDTO
	AssetID      string          `json:"asset_id"`
	GenerationID string          `json:"generation_id"`
	State        string          `json:"state"`
	Profile      GroupProfileDTO `json:"profile"`
	Format       FormatDTO       `json:"format"`
	SizeBytes    Decimal         `json:"size_bytes"`
}

func variantDTO(v core.FallbackVariant) VariantDTO {
	return VariantDTO{recordDTO(v.Record), string(v.AssetID), string(v.GenerationID), string(v.State), groupProfileDTO(v.Profile), formatDTO(v.VerifiedFormat), Decimal(v.SizeBytes)}
}

type PageDTO[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func pageDTO[A, B any](p core.Page[A], convert func(A) B) PageDTO[B] {
	items := make([]B, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, convert(v))
	}
	return PageDTO[B]{items, p.NextCursor}
}
func utcPointer(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}
func timestamp(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

type NullablePolicy Field[PolicyDTO]

func (p *NullablePolicy) UnmarshalJSON(b []byte) error {
	return (*Field[PolicyDTO])(p).UnmarshalJSON(b)
}
func (NullablePolicy) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{AnyOf: []*huma.Schema{r.Schema(reflect.TypeFor[PolicyDTO](), true, ""), {Type: "null"}}}
}
