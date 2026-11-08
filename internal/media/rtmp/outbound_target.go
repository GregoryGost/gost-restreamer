package rtmp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
)

// OutboundErrorKind отделяет окончательный отказ привязки формата от временной сетевой ошибки.
type OutboundErrorKind string

const (
	OutboundInvalid     OutboundErrorKind = "invalid"
	OutboundProtocol    OutboundErrorKind = "protocol"
	OutboundMedia       OutboundErrorKind = "media"
	OutboundTiming      OutboundErrorKind = "timing"
	OutboundDenied      OutboundErrorKind = "denied"
	OutboundCertificate OutboundErrorKind = "certificate"
)

// OutboundError сохраняет тип отказа, код сервера и исходную диагностическую причину.
type OutboundError struct {
	Kind  OutboundErrorKind
	Cause error
	Code  string
}

func (e *OutboundError) Error() string {
	return fmt.Sprintf("RTMP outbound: %s: %s: %v", e.Kind, e.Code, e.Cause)
}
func (e *OutboundError) Unwrap() error { return e.Cause }

// IsOutboundTerminal распознаёт структурные, медийные, авторизационные и TLS-отказы.
// EOF, сброс соединения, отказ подключения и истечение срока остаются временными ошибками транспорта.
func IsOutboundTerminal(err error) bool {
	var out *OutboundError
	var verify *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &out) || errors.As(err, &verify) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

func outboundError(kind OutboundErrorKind, cause error) error {
	return &OutboundError{Kind: kind, Cause: cause}
}

// OutboundTarget хранит проверенный URL приложения и отдельное точное имя публикации.
// Поля закрыты, чтобы подготовленное назначение нельзя было изменить перед DialOutbound.
type OutboundTarget struct {
	endpoint, address, hostname, app, key string
	tls                                   bool
	profile                               core.MediaFormat
}

// OutboundResolver позволяет добавить правила площадки без закрытого перечня платформ.
type OutboundResolver func(endpoint, streamKey string, profile core.MediaFormat) (OutboundTarget, error)

// Validate повторно проверяет результат произвольного преобразователя адреса и полный ожидаемый
// формат, а также размеры команд до первой сети или резервирования исполнения.
func (t OutboundTarget) Validate(expected core.MediaFormat, limits OutboundLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	verified, err := ResolveOutbound(t.endpoint, t.key, expected)
	if err != nil {
		return err
	}
	if !t.profile.Equal(expected) {
		return outboundError(OutboundInvalid, core.ErrInvalid)
	}
	verified.profile = t.profile
	if verified != t {
		return outboundError(OutboundInvalid, core.ErrInvalid)
	}
	if t.retainedBytes() > limits.MaxRetainedBytes-limits.baseBytes() {
		return outboundError(OutboundInvalid, ErrLimit)
	}
	for _, data := range []amf0.Data{outboundConnectData(t), {"createStream", float64(2), nil}, {"publish", float64(0), nil, t.key, "live"}} {
		n, err := data.MarshalSize()
		if err != nil || uint64(n) > uint64(limits.MaxAMFBytes) || uint64(n) > uint64(limits.MaxMessageBytes) {
			return outboundError(OutboundInvalid, ErrLimit)
		}
		b, err := data.Marshal()
		if err != nil {
			return outboundError(OutboundInvalid, err)
		}
		if err = preflightAMF(b, limits.amfLimits()); err != nil {
			return outboundError(OutboundInvalid, err)
		}
	}
	return nil
}

// ResolveOutbound проверяет полный профиль, URL приложения и отдельный ключ без сетевых операций.
func ResolveOutbound(endpoint, streamKey string, profile core.MediaFormat) (OutboundTarget, error) {
	bad := func() (OutboundTarget, error) {
		return OutboundTarget{}, outboundError(OutboundInvalid, core.ErrInvalid)
	}
	if len(endpoint) == 0 || len(endpoint) > math.MaxUint16 || len(streamKey) == 0 || len(streamKey) > math.MaxUint16 || strings.Contains(endpoint, "#") {
		return bad()
	}
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || u.User != nil || u.Opaque != "" || u.Host == "" || u.Fragment != "" || (u.Scheme != "rtmp" && u.Scheme != "rtmps") {
		return bad()
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, " \t\r\n/\\") {
		return bad()
	}
	// Parse отвергает некорректный порт, но пустой явный порт проверяется отдельно.
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return bad()
	}
	if port == "" {
		if u.Scheme == "rtmps" {
			port = "443"
		} else {
			port = "1935"
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return bad()
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return bad()
	}
	if err = validateOutboundProfile(profile); err != nil {
		return OutboundTarget{}, outboundError(OutboundInvalid, err)
	}
	for _, s := range outboundFormatStrings(profile) {
		if len(s) > math.MaxUint16 {
			return bad()
		}
	}
	app := strings.TrimPrefix(u.EscapedPath(), "/")
	if app == "" {
		return bad()
	}
	if u.RawQuery != "" || u.ForceQuery {
		app += "?" + u.RawQuery
	}
	profile = cloneOutboundFormat(profile)
	return OutboundTarget{endpoint: strings.Clone(endpoint), address: net.JoinHostPort(host, port), hostname: strings.Clone(host), app: strings.Clone(app), key: strings.Clone(streamKey), tls: u.Scheme == "rtmps", profile: profile}, nil
}

func validateOutboundProfile(p core.MediaFormat) error {
	if err := codec.Compatible(p, p); err != nil {
		return err
	}
	a, v := p.Audio, p.Video
	if a.CodecProfile != "LC" || a.SampleRateHz > 0xffffff || a.Channels != 1 && a.Channels != 2 || a.Channels == 1 && a.ChannelLayout != "mono" || a.Channels == 2 && a.ChannelLayout != "stereo" {
		return ErrCodec
	}
	if v.Color.Range != "limited" && v.Color.Range != "full" && v.Color.Range != "tv" && v.Color.Range != "pc" {
		return ErrCodec
	}
	if v.Codec == core.CodecAV1 {
		if v.CodecProfile != "main" || v.Width > 65536 || v.Height > 65536 || v.Tier != "" && v.Tier != "main" && v.Tier != "high" {
			return ErrCodec
		}
		if v.Level != "" && v.Level != "auto" && v.Level != "unspecified" {
			valid := false
			for level := 0; level < 24; level++ {
				if v.Level == fmt.Sprintf("%d.%d", 2+level/4, level%4) {
					valid = true
					if level < 8 && v.Tier == "high" {
						return ErrCodec
					}
					break
				}
			}
			if !valid {
				return ErrCodec
			}
		}
		for _, value := range []string{v.Color.Primaries, v.Color.Transfer, v.Color.Matrix} {
			if !outboundColorValue(value, "cicp:", map[uint8]string{1: "bt709", 2: "unspecified"}) {
				return ErrCodec
			}
		}
		if v.Color.Transfer == "cicp:16" || v.Color.Transfer == "cicp:18" || v.Color.Primaries == "bt709" && v.Color.Transfer == "cicp:13" && v.Color.Matrix == "cicp:0" {
			return ErrCodec
		}
		return nil
	}
	if v.Tier != "" || v.Width%2 != 0 || v.Height%2 != 0 {
		return ErrCodec
	}
	switch v.CodecProfile {
	case "baseline", "constrained-baseline", "main", "extended", "high", "high10", "high422", "high444":
	default:
		if !outboundColorValue(v.CodecProfile, "h264-profile-idc-", map[uint8]string{66: "baseline", 77: "main", 88: "extended", 100: "high", 110: "high10", 122: "high422", 244: "high444"}) {
			return ErrCodec
		}
	}
	if v.Level != "" && v.Level != "auto" {
		valid := false
		for level := 0; level <= 255; level++ {
			if v.Level == fmt.Sprintf("%d.%d", level/10, level%10) {
				valid = true
				break
			}
		}
		if !valid {
			return ErrCodec
		}
	}
	primary := map[uint8]string{1: "bt709", 2: "unspecified", 5: "bt470bg", 6: "smpte170m", 9: "bt2020"}
	transfer := map[uint8]string{1: "bt709", 2: "unspecified", 6: "smpte170m", 8: "linear", 13: "iec61966-2-1", 16: "smpte2084", 18: "arib-std-b67"}
	if !outboundColorValue(v.Color.Primaries, "iso-", primary) || !outboundColorValue(v.Color.Transfer, "iso-", transfer) || !outboundColorValue(v.Color.Matrix, "iso-", primary) {
		return ErrCodec
	}
	return nil
}

func outboundColorValue(value, prefix string, names map[uint8]string) bool {
	for _, name := range names {
		if value == name {
			return true
		}
	}
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	raw := strings.TrimPrefix(value, prefix)
	n, err := strconv.ParseUint(raw, 10, 8)
	if err != nil || raw != strconv.FormatUint(n, 10) {
		return false
	}
	_, named := names[uint8(n)]
	return !named
}

func outboundFormatStrings(p core.MediaFormat) []string {
	return []string{string(p.Video.Codec), p.Video.CodecProfile, p.Video.Level, p.Video.Tier, p.Video.PixelFormat, p.Video.Color.Range, p.Video.Color.Primaries, p.Video.Color.Transfer, p.Video.Color.Matrix, string(p.Audio.Codec), p.Audio.CodecProfile, p.Audio.ChannelLayout}
}

func cloneOutboundFormat(p core.MediaFormat) core.MediaFormat {
	p.Video.Codec = core.Codec(strings.Clone(string(p.Video.Codec)))
	p.Video.CodecProfile = strings.Clone(p.Video.CodecProfile)
	p.Video.Level = strings.Clone(p.Video.Level)
	p.Video.Tier = strings.Clone(p.Video.Tier)
	p.Video.PixelFormat = strings.Clone(p.Video.PixelFormat)
	p.Video.Color.Range = strings.Clone(p.Video.Color.Range)
	p.Video.Color.Primaries = strings.Clone(p.Video.Color.Primaries)
	p.Video.Color.Transfer = strings.Clone(p.Video.Color.Transfer)
	p.Video.Color.Matrix = strings.Clone(p.Video.Color.Matrix)
	p.Audio.Codec = core.Codec(strings.Clone(string(p.Audio.Codec)))
	p.Audio.CodecProfile = strings.Clone(p.Audio.CodecProfile)
	p.Audio.ChannelLayout = strings.Clone(p.Audio.ChannelLayout)
	return p
}

func (t OutboundTarget) retainedBytes() uint64 {
	n := uint64(len(t.endpoint) + len(t.address) + len(t.hostname) + len(t.app) + len(t.key))
	for _, s := range outboundFormatStrings(t.profile) {
		n += uint64(len(s))
	}
	return n
}

// OutboundLimits задаёт положительные сроки и конечные логические бюджеты соединения.
type OutboundLimits struct {
	MaxChunkStreams, MaxMessageBytes, MaxChunkBytes, MaxFragments             uint32
	MaxControlBytes, MaxConfigBytes, MaxAMFBytes, MaxAMFDepth, MaxAMFElements uint32
	ChunkSize                                                                 uint32
	MaxControlPending                                                         uint32
	MaxRetainedBytes                                                          uint64
	DialTimeout, TLSTimeout, StartupTimeout, MessageTimeout, WriteTimeout     time.Duration
}

func (l OutboundLimits) baseBytes() uint64 {
	return 8192 + 8*uint64(l.MaxMessageBytes) + 4*uint64(l.MaxChunkBytes) + 192*uint64(l.MaxAMFElements) + 128*uint64(l.MaxAMFDepth) + 512*uint64(l.MaxChunkStreams) + 64*uint64(l.MaxControlPending)
}

// Validate выполняется до выделения соединения или тела удалённого сообщения.
func (l OutboundLimits) Validate() error {
	if l.MaxControlPending == 0 || l.MaxControlPending > 1<<16 {
		return outboundError(OutboundInvalid, ErrLimit)
	}
	if l.MaxChunkStreams == 0 || l.MaxChunkStreams > 62 || l.MaxMessageBytes == 0 || l.MaxMessageBytes > 10<<20 || l.MaxChunkBytes < 128 || l.MaxChunkBytes > 10<<20 || l.MaxFragments == 0 || l.MaxFragments > 1<<20 || l.MaxControlBytes < 6 || l.MaxControlBytes > l.MaxMessageBytes || l.MaxConfigBytes < 4 || l.MaxConfigBytes > l.MaxMessageBytes || l.MaxAMFBytes == 0 || l.MaxAMFBytes > l.MaxMessageBytes || l.MaxAMFDepth == 0 || l.MaxAMFDepth > 64 || l.MaxAMFElements == 0 || l.MaxAMFElements > 1<<20 || l.ChunkSize < 128 || l.ChunkSize > l.MaxChunkBytes || l.DialTimeout <= 0 || l.TLSTimeout <= 0 || l.StartupTimeout <= 0 || l.MessageTimeout <= 0 || l.WriteTimeout <= 0 || l.MaxRetainedBytes < l.baseBytes() {
		return outboundError(OutboundInvalid, ErrLimit)
	}
	return nil
}

func (l OutboundLimits) amfLimits() Limits {
	return Limits{MaxAMFDepth: l.MaxAMFDepth, MaxAMFElements: l.MaxAMFElements}
}

// OutboundOperation сопоставляется вызывающим с фиксированным слотом резерва исполнения.
type OutboundOperation uint8

const (
	OutboundEstablishment OutboundOperation = iota + 1
	OutboundRead
	OutboundWrite
)

// OutboundOptions не устанавливает скрытых сроков или настроек кодировщика.
// OnWrite вызывается синхронно вне блокировки полномочий для каждого положительного n,
// включая частичную запись с ошибкой и неудавшееся согласование соединения. Обработчик не должен
// повторно входить в соединение или блокировать его остановку.
// OnACKAnomaly вызывается читателем после обновления снимка, без блокировок транспорта.
// Оба обработчика выполняют только короткое локальное обновление без I/O,
// журналирования и обращения к полномочиям.
type OutboundOptions struct {
	Limits       OutboundLimits
	TLSConfig    *tls.Config
	DialContext  func(context.Context, string, string) (net.Conn, error)
	Admit        func(context.Context, OutboundOperation) (func(), error)
	OnWrite      func(n int)
	OnACKAnomaly func(bytes uint64, at time.Time)
}

// Validate проверяет подготовку без сети. nil DialContext выбирает net.Dialer.
func (o OutboundOptions) Validate() error {
	if err := o.Limits.Validate(); err != nil {
		return err
	}
	if o.TLSConfig != nil && o.TLSConfig.InsecureSkipVerify {
		return outboundError(OutboundInvalid, fmt.Errorf("%w: TLS", core.ErrInvalid))
	}
	return nil
}
