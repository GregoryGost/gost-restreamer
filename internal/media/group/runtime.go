package group

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// FileOpener открывает проверенный файл артефакта по манифесту для отдельной попытки проигрывателя.
type FileOpener func(context.Context, core.ArtifactManifest) (*os.File, error)

// PlayerBuilder строит файловую команду FFmpeg с переданным дескриптором и выходным UDS попытки.
type PlayerBuilder func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error)

// TranscodeBuilder строит команду LIVE-перекодирования по входному формату, профилю и UDS попытки.
type TranscodeBuilder func(context.Context, ffmpeg.NVIDIAConfig, ffmpeg.Attempt) (ffmpeg.Command, error)

// RuntimeConfig задаёт зависимости, бюджеты и сроки общей обработки одной группы.
type RuntimeConfig struct {
	// Scope учитывает процессы и временные ресурсы всех работников группы в области выполнения
	// Session.
	Scope *processowner.Scope
	// Switcher задаёт профиль, контроль продвижения и пределы переключения источников группы.
	Switcher SwitcherConfig
	// FFmpeg задаёт сроки и бюджеты физических работников проигрывания и перекодирования.
	FFmpeg ffmpeg.Config
	// Container ограничивает чтение и запись fMP4 между Go и FFmpeg.
	Container fmp4.Limits
	// Binary задаёт исполняемый файл FFmpeg для работников группы.
	Binary string
	// OpenFile открывает готовый файл заглушки по сохранённому манифесту.
	OpenFile FileOpener
	// Player заменяет стандартный BuildFileCommand при явном задании построителя.
	Player PlayerBuilder
	// Transcode заменяет стандартный BuildNVIDIACommand при явном задании построителя.
	Transcode TranscodeBuilder
	// Logger принимает события группы и полную диагностику FFmpeg; nil отключает эти записи.
	Logger *slog.Logger
	// Sampler регистрирует периодический сбор измерений группы и работников; nil отключает
	// регистрацию.
	Sampler *monitoring.SessionSampler
	// MetricStaleAfter задаёт допустимый возраст progress и расчётов скорости.
	MetricStaleAfter time.Duration
	// ResubscribeBackoff задаёт паузу повторной подписки LIVE; ControlInterval задаёт период сверки
	// Session и готовности; ShutdownTimeout ограничивает ожидание остановки и закрытие наблюдателей.
	ResubscribeBackoff, ControlInterval, ShutdownTimeout time.Duration
}

// Validate проверяет выбранный профиль и политику переключения, наличие файлового порта и статические
// бюджеты исполнения.
func (c RuntimeConfig) Validate() error {
	if c.Switcher.Validate() != nil || c.OpenFile == nil {
		return core.ErrInvalid
	}
	return c.ValidateTemplate()
}

// ValidateTemplate проверяет статические настройки без фиктивных доменных данных.
func (c RuntimeConfig) ValidateTemplate() error {
	if c.Switcher.ValidateTemplate() != nil || c.FFmpeg.Validate() != nil || c.Container.Validate() != nil || c.Binary == "" || c.ResubscribeBackoff <= 0 || c.ControlInterval <= 0 || c.ShutdownTimeout <= 0 {
		return core.ErrInvalid
	}
	return nil
}

// RegistryConfig связывает реестр групп с точным выполнением Session и серверными бюджетами.
type RegistryConfig struct {
	// Core предоставляет сохранённую Session, её выходы и допуск исполнения.
	Core *core.Service
	// Parent задаёт точную идентичность родительского выполнения Session без OutputID.
	Parent core.ExecutionFence
	// Observer открывает наблюдателей групп того же выполнения Session.
	Observer *core.SessionObserver
	// Runtime задаёт общие зависимости и статические ограничения создаваемых групп.
	Runtime RuntimeConfig
	// MaxGroups ограничивает число одновременно учитываемых групп; MaxReferences ограничивает общее
	// число выданных ссылок на них.
	MaxGroups, MaxReferences uint32
}

// Registry разделяет группы точного выполнения Session и учитывает ссылки, остановку и ревизии паузы.
type Registry struct {
	// mu защищает создание групп, число ссылок и применение управляющих ревизий.
	mu sync.Mutex
	// config хранит проверенные зависимости и бюджеты реестра.
	config RegistryConfig
	// owner разрешает работу групп только текущего родительского выполнения Session.
	owner *core.RuntimeLease
	// groups связывает точный сериализованный ключ профиля, артефакта и политики с группой.
	groups map[string]*Group
	// references считает ещё не закрытые ссылки, выданные реестром.
	references uint32
	// closed запрещает новые ссылки после начала Close реестра.
	closed bool
	// controlRevision хранит последнюю принятую ревизию желаемой паузы Session.
	controlRevision uint64
	// paused хранит желаемую паузу, наследуемую вновь создаваемыми группами.
	paused bool
}

// NewRegistry проверяет точного владельца Session, наблюдателя и бюджеты, затем связывает допуск
// исполнения; группы создаются при Acquire.
func NewRegistry(ctx context.Context, c RegistryConfig) (*Registry, error) {
	if c.Core == nil || c.Observer == nil || !c.Observer.Valid() || c.Observer.SessionFence() != (core.RuntimeSessionFence{SessionID: c.Parent.SessionID, Incarnation: c.Parent.Incarnation, Lifecycle: c.Parent.Lifecycle}) || c.Parent.Validate() != nil || c.Parent.OutputID != "" || c.MaxGroups == 0 || c.MaxReferences == 0 || c.Runtime.OpenFile == nil || c.Runtime.ValidateTemplate() != nil {
		return nil, core.ErrInvalid
	}
	// owner содержит допуск точного выполнения Session; err сообщает об отказе его привязки.
	owner, err := c.Core.BindExecutionOwner(ctx, c.Parent)
	if err != nil {
		return nil, err
	}
	return &Registry{config: c, owner: owner, groups: make(map[string]*Group)}, nil
}

// Reference учитывает одно использование общей группы и единожды освобождает его.
type Reference struct {
	// registry содержит реестр, в котором учитывается эта ссылка.
	registry *Registry
	// group содержит общую группу, удерживаемую данной ссылкой.
	group *Group
	// once исключает повторное уменьшение числа ссылок при повторном Close.
	once sync.Once
	// last отмечает освобождение последней ссылки и необходимость ждать остановки группы.
	last bool
}

// Router возвращает общий выходной Router группы, на которую удерживается ссылка.
func (r *Reference) Router() *media.Router { return r.group.switcher.Router() }

// Group возвращает группу текущей ссылки без создания нового владельца.
func (r *Reference) Group() *Group { return r.group }

// Close освобождает ссылку и при её последнем использовании ждёт остановку группы с фоновым контекстом
// и собственным сроком группы.
func (r *Reference) Close() error { return r.CloseContext(context.Background()) }

// CloseContext единожды уменьшает число ссылок; последняя ссылка отменяет группу и допускает повторное
// ограниченное ожидание её остановки.
func (r *Reference) CloseContext(ctx context.Context) error {
	r.once.Do(func() {
		// Единожды уменьшает число ссылок реестра и группы; последняя ссылка помечает группу закрывающейся и
		// отменяет её работу под блокировкой реестра.

		r.registry.mu.Lock()
		r.registry.references--
		r.group.refs--
		r.last = r.group.refs == 0
		if r.last {
			r.group.closing = true
			r.group.cancel()
		}
		r.registry.mu.Unlock()
	})
	if r.last {
		return r.group.Stop(ctx)
	}
	return nil
}

// Acquire сверяет снимок с текущей Session и её выходами, разделяет точную группу по профилю,
// артефакту и политике и выдаёт учитываемую ссылку. Новые группы наследуют последнюю ревизию паузы;
// закрывающаяся группа не переиспользуется до подтверждённого завершения.
func (r *Registry) Acquire(ctx context.Context, snapshot core.OutputSnapshot, live *media.Router) (*Reference, error) {
	if live == nil || snapshot.Validate() != nil {
		return nil, core.ErrInvalid
	}
	// session содержит текущую сохранённую Session; err сохраняет результаты её чтения, канонизации
	// и сериализации ключа группы.
	session, err := r.config.Core.GetSession(ctx, r.config.Parent.SessionID)
	if err != nil {
		return nil, err
	}
	if session.Incarnation != r.config.Parent.Incarnation || session.Lifecycle != r.config.Parent.Lifecycle || session.Snapshot.Stream.Fallback != snapshot.Fallback {
		return nil, core.ErrStale
	}
	if err = r.checkSnapshot(ctx, session.ID, snapshot); err != nil {
		return nil, err
	}
	// canonical содержит точные канонические байты профиля для ключа разделяемой группы.
	canonical, err := core.CanonicalProfile(snapshot.Profile.Profile)
	if err != nil {
		return nil, err
	}
	// manifest содержит независимую копию манифеста артефакта, используемого данной группой.
	manifest := snapshot.Artifact
	manifest.CanonicalProfile = append([]byte(nil), manifest.CanonicalProfile...)
	// encoded содержит сериализованную идентичность группы с выполнением, профилем, манифестом и
	// политикой.
	encoded, err := json.Marshal(struct {
		// Parent включает точное выполнение Session в ключ разделяемой группы.
		Parent core.ExecutionFence
		// Group включает идентификатор группы снимка в ключ разделения.
		Group core.ID
		// Profile включает канонические байты профиля кодирования в ключ разделения.
		Profile []byte
		// Manifest включает точный артефакт заглушки в ключ разделения.
		Manifest core.ArtifactManifest
		// Policy включает выбранную политику переключения Stream в ключ разделения.
		Policy core.FallbackPolicy
	}{r.config.Parent, snapshot.Group.ID, canonical, manifest, snapshot.Fallback})
	if err != nil {
		return nil, err
	}
	// key содержит строковое представление полного ключа для поиска общей группы.
	key := string(encoded)
	r.mu.Lock()
	defer r.mu.Unlock()
	if session.Revision > r.controlRevision {
		r.controlRevision, r.paused = session.Revision, session.DesiredPaused
	}
	if r.closed || !r.owner.Valid() {
		return nil, core.ErrStale
	}
	if r.references >= r.config.MaxReferences {
		return nil, media.ErrRouterBudget
	}
	// g содержит найденную группу либо создаваемую замену после подтверждённого завершения прежней.
	g := r.groups[key]
	if g != nil && g.closing {
		select {
		// err содержит окончательный результат закрывающейся группы перед допустимым повторным
		// использованием её ключа.
		case <-g.done:
			g.mu.Lock()
			err := g.result
			g.mu.Unlock()
			if err != nil {
				return nil, err
			}
			delete(r.groups, key)
			g = nil
		default:
			return nil, core.ErrNotReady
		}
	}
	if g == nil {
		// retiredKey задаёт ключ прежней закрывающейся группы; retired содержит её состояние для
		// освобождения занятого места.
		for retiredKey, retired := range r.groups {
			if !retired.closing {
				continue
			}
			select {
			// clean показывает, завершилась ли прежняя группа без ошибки очистки и можно ли
			// удалить её запись.
			case <-retired.done:
				retired.mu.Lock()
				clean := retired.result == nil
				retired.mu.Unlock()
				if clean {
					delete(r.groups, retiredKey)
				}
			default:
			}
		}
		if uint64(len(r.groups)) >= uint64(r.config.MaxGroups) {
			return nil, media.ErrRouterBudget
		}
		// observer содержит наблюдателя новой группы; err сообщает об отказе открытия наблюдения
		// или создания группы.
		observer, err := r.config.Observer.OpenGroup(ctx, snapshot.Group.ID, snapshot.Profile.ID)
		if err != nil {
			return nil, err
		}
		// c содержит настройки новой группы с профилем и политикой конкретного снимка.
		c := r.config.Runtime
		c.Switcher.Profile = snapshot.Profile.Profile
		c.Switcher.Watchdog = snapshot.Fallback.Watchdog
		g, err = newGroup(c, r.config.Core, r.config.Parent, r.owner, observer, snapshot.Group.ID, manifest, live, r.paused, r.controlRevision)
		if err != nil {
			_ = observer.Close(ctx)
			return nil, err
		}
		r.groups[key] = g
	} else if g.live != live {
		return nil, core.ErrConflict
	}
	r.references++
	g.refs++
	return &Reference{registry: r, group: g}, nil
}

// Ищет точное кодированное совпадение снимка среди подключённых желаемых выходов Session без ожидающей
// очистки, обходя все страницы.
func (r *Registry) checkSnapshot(ctx context.Context, session core.ID, snapshot core.OutputSnapshot) error {
	// wanted содержит точное кодированное представление проверяемого снимка; err сообщает об отказе
	// кодирования.
	wanted, err := core.EncodeOutputSnapshot(snapshot)
	if err != nil {
		return err
	}
	// request задаёт текущую страницу выходов проверяемой Session.
	request := core.PageRequest{Query: core.ListQuery{Limit: core.MaxPageLimit, SessionID: session}}
	for {
		// page содержит очередную страницу выходов; err сообщает об отказе её чтения.
		page, err := r.config.Core.ListOutputs(ctx, request)
		if err != nil {
			return err
		}
		// o содержит очередной выход для поиска подключённого желаемого точного снимка без
		// ожидающей очистки.
		for _, o := range page.Items {
			if !o.Attached || !o.DesiredRunning || o.CleanupPending {
				continue
			}
			// actual содержит кодированный снимок проверяемого выхода; err сообщает об
			// отказе его кодирования.
			actual, err := core.EncodeOutputSnapshot(o.Snapshot)
			if err != nil {
				return err
			}
			if bytes.Equal(wanted, actual) {
				return nil
			}
		}
		if page.NextCursor == "" {
			return core.ErrStale
		}
		request.Cursor = page.NextCursor
	}
}

// Close запрещает новые ссылки, отменяет все группы и ждёт их остановки, объединяя ошибки без
// автоматического освобождения внешних ссылок.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	// groups сохраняет список отменённых групп для ожидания вне блокировки реестра.
	groups := make([]*Group, 0, len(r.groups))
	// g содержит очередную зарегистрированную группу при отмене всех работников.
	for _, g := range r.groups {
		groups = append(groups, g)
		g.cancel()
	}
	r.mu.Unlock()
	// errs накапливает ошибки ожидания остановки всех групп реестра.
	var errs []error
	// g содержит очередную отменённую группу, завершение которой ожидается.
	for _, g := range groups {
		// err сообщает об отказе или истечении ожидания остановки конкретной группы.
		if err := g.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ApplyControl упорядочивает желание с созданием новых групп по версии Session.
func (r *Registry) ApplyControl(revision uint64, paused bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.owner.Valid() {
		return core.ErrStale
	}
	if revision < r.controlRevision {
		return nil
	}
	if revision == r.controlRevision && paused != r.paused {
		return core.ErrConflict
	}
	r.controlRevision, r.paused = revision, paused
	// errs накапливает ошибки применения новой ревизии паузы к действующим группам.
	var errs []error
	// g содержит очередную группу для применения управляющей ревизии, если она ещё не закрывается.
	for _, g := range r.groups {
		if !g.closing {
			// err содержит результат применения паузы выбранной группе.
			if err := g.applyControl(revision, paused); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Group связывает LIVE, проигрыватель заглушки и переключатель с точным выполнением и наблюдателем
// Session.
type Group struct {
	// refs считает ссылки на группу под блокировкой её Registry.
	refs uint32
	// closing запрещает повторную выдачу группы после освобождения последней ссылки.
	closing bool
	// config хранит зависимости, профиль и бюджеты исполнения группы.
	config RuntimeConfig
	// service читает сохранённую Session для сверки её владельца и желаемой паузы.
	service *core.Service
	// parent содержит точную идентичность родительского выполнения Session.
	parent core.ExecutionFence
	// owner ограничивает жизнь работников и передачу пакетов действующим допуском Session.
	owner *core.RuntimeLease
	// observer публикует состояние группы и открывает наблюдателей её FFmpeg-ролей.
	observer *core.GroupObserver
	// id содержит доменный идентификатор группы для наблюдений и диагностики.
	id core.ID
	// manifest задаёт готовый проверенный артефакт заглушки данной группы.
	manifest core.ArtifactManifest
	// live содержит входной Router с актуальными публикациями LIVE.
	live *media.Router
	// switcher выбирает источник и формирует постоянный выходной Router группы.
	switcher *Switcher
	// cancel отменяет общий контекст проигрывателя, LIVE и наблюдения состояния.
	cancel context.CancelFunc
	// done закрывается после завершения работников и закрытия наблюдателя группы.
	done chan struct{}
	// mu защищает итог очистки и окончательное состояние проигрывателя заглушки.
	mu sync.Mutex
	// result накапливает ошибки физической очистки и закрытия группы.
	result error
	// attempts хранит независимые возрастающие номера попыток LIVE и заглушки.
	attempts [2]atomic.Uint64
	// controlMu упорядочивает применение ревизий желаемой паузы.
	controlMu sync.Mutex
	// controlRevision хранит последнюю применённую ревизию управления Session.
	controlRevision uint64
	// fallbackEnded отмечает окончательное завершение проигрывателя и отзывает текущую готовность
	// заглушки.
	fallbackEnded bool
	// fallbackError сохраняет причину окончательного завершения проигрывателя заглушки.
	fallbackError error
	// metricsMu защищает накопления входных пакетов LIVE и заглушки.
	metricsMu sync.Mutex
	// liveCounters накапливает фактически принятые входные медиапакеты LIVE.
	liveCounters media.CounterSnapshot
	// fallbackCounters накапливает проверенные пакеты выхода проигрывателя заглушки.
	fallbackCounters media.CounterSnapshot
}

// Создаёт переключатель и наблюдения группы для поддержанной пары режима и видеокодека, применяет
// паузу и запускает работников с отдельным контекстом жизни, ограниченным владельцем Session.
func newGroup(c RuntimeConfig, service *core.Service, parent core.ExecutionFence, owner *core.RuntimeLease, observer *core.GroupObserver, id core.ID, manifest core.ArtifactManifest, live *media.Router, paused bool, revision uint64) (*Group, error) {
	// err сообщает об отказе проверки конфигурации выбранной группы.
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Switcher.Profile.Mode == core.ModeCopy && c.Switcher.Profile.Format.Video.Codec != core.CodecAV1 || c.Switcher.Profile.Mode == core.ModeTranscode && c.Switcher.Profile.Format.Video.Codec != core.CodecH264 {
		return nil, core.ErrInvalid
	}
	// sw содержит новый переключатель; err сообщает об отказе его создания.
	sw, err := NewSwitcher(c.Switcher)
	if err != nil {
		return nil, err
	}
	// Срок работы принадлежит выданному владельцу Session; ctx запроса Acquire не удерживается.
	// ctx задаёт самостоятельный срок группы, отзываемый владельцем Session; cancel отменяет её
	// работников.
	ctx, cancel := context.WithCancel(context.Background())
	// g содержит создаваемую группу с отдельным сроком жизни и наблюдателем.
	g := &Group{config: c, service: service, parent: parent, owner: owner, observer: observer, id: id, manifest: manifest, live: live, switcher: sw, cancel: cancel, done: make(chan struct{})}
	// err сообщает об отказе регистрации измерений до запуска работников.
	if err := g.registerMetrics(); err != nil {
		cancel()
		_ = sw.Close()
		return nil, err
	}
	_ = g.applyControl(revision, paused)
	go g.run(ctx)
	return g, nil
}

// Применяет паузу в порядке ревизий и сохраняет желание даже при временно недоступном источнике; более
// старая ревизия пропускается.
func (g *Group) applyControl(revision uint64, paused bool) error {
	g.controlMu.Lock()
	defer g.controlMu.Unlock()
	if revision < g.controlRevision {
		return nil
	}
	g.controlRevision = revision
	// err содержит результат применения желаемой паузы переключателю.
	err := g.switcher.SetPaused(paused, time.Now())
	// Желание уже применено, даже если подходящий источник пока недоступен.
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrFeed) {
		return nil
	}
	return err
}

// Ready проверяет пригодность текущего fallback, а не исторический статус FFmpeg.
func (g *Group) Ready() error {
	if !g.owner.Valid() {
		return core.ErrStale
	}
	select {
	case <-g.done:
		return core.ErrStale
	default:
	}
	g.mu.Lock()
	// ended показывает окончательное завершение заглушки; err сохраняет его причину для отказа
	// текущей готовности.
	ended, err := g.fallbackEnded, g.fallbackError
	g.mu.Unlock()
	if ended {
		return errors.Join(ErrUnavailable, err)
	}
	return g.switcher.FallbackReady(g.attempts[index(Fallback)].Load(), time.Now())
}

// WaitReady ограничивается общим контекстом запуска, включая все перезапуски.
func (g *Group) WaitReady(ctx context.Context) error {
	// ticker задаёт период повторной проверки фактической готовности заглушки.
	ticker := time.NewTicker(g.config.ControlInterval)
	defer ticker.Stop()
	for {
		// err содержит текущий результат проверки готовности группы.
		err := g.Ready()
		if err == nil {
			return nil
		}
		g.mu.Lock()
		// ended сохраняет признак окончательного завершения заглушки для прекращения ожидания.
		ended := g.fallbackEnded
		g.mu.Unlock()
		if ended || errors.Is(err, core.ErrStale) || errors.Is(err, media.ErrRouterClosed) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.done:
			return core.ErrStale
		case <-ticker.C:
		}
	}
}

// Router возвращает постоянный выходной маршрутизатор переключателя группы.
func (g *Group) Router() *media.Router { return g.switcher.Router() }

// Status возвращает согласованный снимок состояния переключателя группы.
func (g *Group) Status() Status { return g.switcher.Status() }

// Done возвращает канал завершения работников и закрытия наблюдателя группы.
func (g *Group) Done() <-chan struct{} { return g.done }

// MediaSnapshots копирует каждый источник под его собственной блокировкой.
func (g *Group) MediaSnapshots() (live, fallback, output media.CounterSnapshot) {
	g.metricsMu.Lock()
	live, fallback = g.liveCounters, g.fallbackCounters
	live.SampledAt, fallback.SampledAt = time.Now(), time.Now()
	g.metricsMu.Unlock()
	return live, fallback, g.switcher.OutputSnapshot()
}

// Учитывает входной пакет в отдельном накоплении LIVE либо заглушки под блокировкой измерений.
func (g *Group) noteInput(feed Feed, p media.Packet) {
	g.metricsMu.Lock()
	defer g.metricsMu.Unlock()
	// counter выбирает накопление входных пакетов LIVE либо заглушки.
	counter := &g.liveCounters
	if feed == Fallback {
		counter = &g.fallbackCounters
	}
	counter.AddPacket(p, time.Now())
}

// Stop отменяет работников и закрывает переключатель, затем ждёт done в пределах контекста и
// ShutdownTimeout; истечение срока оставляет ErrCleanupPending.
func (g *Group) Stop(ctx context.Context) error {
	g.cancel()
	if g.switcher != nil {
		_ = g.switcher.Close()
	}
	select {
	case <-g.done:
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.result
	default:
	}
	// timer ограничивает ожидание остановки работников сроком ShutdownTimeout.
	timer := time.NewTimer(g.config.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-g.done:
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.result
	case <-ctx.Done():
		return errors.Join(ffmpeg.ErrCleanupPending, ctx.Err())
	case <-timer.C:
		return ffmpeg.ErrCleanupPending
	}
}

// Запускает проигрывание заглушки, приём LIVE и наблюдение состояния, периодически сверяет владельца и
// паузу Session и при завершении ждёт всех работников до закрытия наблюдателя.
func (g *Group) run(ctx context.Context) {
	// wg учитывает проигрыватель, потребителя LIVE и наблюдателя состояния до закрытия группы.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		// Воспроизводит подготовленный файл и сообщает WaitGroup о завершении проигрывателя.
		defer wg.Done()
		g.play(ctx)
	}()
	go func() {
		// Принимает LIVE группы и сообщает WaitGroup о завершении потребителя.
		defer wg.Done()
		g.consumeLive(ctx)
	}()
	go func() {
		// Публикует изменения состояния Switcher и сообщает WaitGroup о завершении наблюдателя.
		defer wg.Done()
		g.observeChanges(ctx)
	}()
	// ticker задаёт период сверки владельца, ревизии паузы и контроля источников.
	ticker := time.NewTicker(g.config.ControlInterval)
	// running управляет циклом сверки до отмены, отзыва либо ошибки чтения Session.
	running := true
	for running {
		select {
		case <-ctx.Done():
			running = false
		case <-g.owner.Done():
			g.cancel()
			running = false
		// session содержит сохранённую Session для сверки точного выполнения; err сообщает об
		// отказе её чтения.
		// now задаёт единое время текущей проверки управления и здоровья переключателя.
		case <-ticker.C:
			session, err := g.service.GetSession(ctx, g.parent.SessionID)
			if err != nil {
				g.cancel()
				running = false
				break
			}
			if session.Incarnation != g.parent.Incarnation || session.Lifecycle != g.parent.Lifecycle {
				g.cancel()
				running = false
				break
			}
			now := time.Now()
			_ = g.applyControl(session.Revision, session.DesiredPaused)
			_ = g.switcher.Tick(now)
		}
	}
	ticker.Stop()
	g.cancel()
	wg.Wait()
	if g.config.Sampler != nil {
		g.config.Sampler.Remove(g.observer.Token())
	}
	// err накапливает результаты закрытия переключателя и наблюдателя после завершения работников.
	err := g.switcher.Close()
	// cleanupCtx ограничивает закрытие наблюдателя отдельным сроком; cancel освобождает таймер.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
	defer cancel()
	err = errors.Join(err, g.observer.Close(cleanupCtx))
	g.mu.Lock()
	g.result = errors.Join(g.result, err)
	close(g.done)
	g.mu.Unlock()
}

// Публикует текущий статус и ждёт очередной смены состояния либо отмены, получая статус и канал
// изменения согласованно.
func (g *Group) observeChanges(ctx context.Context) {
	for {
		// status содержит согласованное состояние переключателя; changed сообщает о следующем
		// существенном изменении.
		status, changed := g.switcher.StateChange()
		g.observeStatus(ctx, status)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// Преобразует доступность и выбранный источник в наблюдение группы с ревизией состояния; недоступность
// публикуется как отказ с текущей причиной.
func (g *Group) observeStatus(ctx context.Context, status Status) {
	// state задаёт готовность либо отказ наблюдения; source обозначает фактически выбранный LIVE,
	// заглушку либо отсутствие источника.
	state, source := core.ObservationReady, core.ObservationSourceNone
	// failure содержит текущий текст причины недоступности для оперативного наблюдения группы.
	failure := ""
	if status.Source == Live {
		source = core.ObservationSourceLive
	}
	if status.Source == Fallback {
		source = core.ObservationSourceFallback
	}
	if !status.Available {
		state = core.ObservationFailed
		source = core.ObservationSourceNone
		if status.Err != nil {
			failure = status.Err.Error()
			if g.config.Logger != nil {
				g.config.Logger.Error("group_state", "component", "group", "operation", "switch", "session_id", g.parent.SessionID, "group_id", g.id, "group_instance_id", g.observer.Token(), "source_revision", status.StateRevision, "error", status.Err)
			}
		}
	}
	_ = g.observer.Observe(ctx, core.GroupObservation{State: state, Source: source, Error: failure, SourceRevision: status.StateRevision})
}

// Передаёт пакет переключателю только внутри действующего допуска владельца Session.
func (g *Group) push(ctx context.Context, feed Feed, attempt uint64, p media.Packet, v, a media.CodecConfig) error {
	return g.owner.Do(ctx, func() error {
		// Под действующим допуском владельца передаёт пакет и текущие конфигурации в Switcher с локальным
		// временем получения.
		return g.switcher.Push(feed, attempt, p, v, a, time.Now())
	})
}

// Повторяет ограниченные подписки LIVE, назначая новую попытку; режим copy проверяет AAC и передаёт
// пакеты напрямую, transcode владеет отдельным работником до повторной подписки.
func (g *Group) consumeLive(ctx context.Context) {
	for ctx.Err() == nil && g.owner.Valid() {
		// sub содержит очередную ограниченную подписку LIVE; err сохраняет результаты подписки,
		// чтения и исполнения текущего источника.
		sub, err := g.live.Subscribe(g.config.Switcher.Queue)
		if err != nil {
			if errors.Is(err, media.ErrRouterClosed) {
				return
			}
			if !delay(ctx, g.config.ResubscribeBackoff) {
				return
			}
			continue
		}
		// attempt содержит новый локальный номер LIVE-попытки; allocationErr сообщает об отказе
		// выделения номера или отзыва прежней готовности.
		attempt, allocationErr := g.nextAttempt(Live)
		if allocationErr != nil {
			_ = sub.Close()
			g.fail(Live, allocationErr)
			return
		}
		if g.config.Switcher.Profile.Mode == core.ModeCopy {
			for ctx.Err() == nil {
				// d содержит очередную выдачу LIVE; e сохраняет ошибки чтения, проверки
				// AAC и передачи переключателю.
				d, e := sub.Next(ctx)
				if e != nil {
					err = e
					break
				}
				if !d.IsCurrent() {
					continue
				}
				g.noteInput(Live, d.Packet)
				if e = g.validateLiveAudio(d.AudioConfig); e != nil {
					err = e
					break
				}
				e = g.push(ctx, Live, attempt, d.Packet, d.VideoConfig, d.AudioConfig)
				if e != nil && !errors.Is(e, ErrUnavailable) && !errors.Is(e, ErrFeed) {
					err = e
					break
				}
			}
		} else {
			err = g.transcode(ctx, sub, attempt)
		}
		_ = sub.Close()
		_ = g.switcher.Fail(Live, attempt, err, time.Now())
		if !delay(ctx, g.config.ResubscribeBackoff) {
			return
		}
	}
}

// Ожидает интервал повторной подписки либо отмену контекста, закрывая таймер при возврате.
func delay(ctx context.Context, d time.Duration) bool {
	// timer отсчитывает паузу до повторной подписки LIVE с возможностью отмены.
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Владеет наблюдателем проигрывателя заглушки и Supervisor, открывает проверенный файл для каждой
// попытки и передаёт фактический fMP4 переключателю; окончательное завершение отзывает готовность
// заглушки.
func (g *Group) play(ctx context.Context) {
	// observer содержит наблюдателя роли проигрывателя заглушки; err сохраняет результаты открытия
	// роли, Supervisor и запуска работника.
	observer, err := g.observer.OpenFFmpegRole(ctx, core.FFmpegFallbackPlayer)
	if err != nil {
		g.fail(Fallback, err)
		return
	}
	defer func() {
		// Закрывает наблюдателя роли проигрывателя с отдельным сроком завершения, игнорируя ошибку.

		// c ограничивает закрытие наблюдателя проигрывателя отдельным сроком; cancel освобождает
		// таймер.
		c, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
		defer cancel()
		_ = observer.Close(c)
	}()
	// supervisor выполняет последовательные физические попытки проигрывания готового файла.
	supervisor, err := ffmpeg.New(g.config.FFmpeg)
	if err != nil {
		g.fail(Fallback, err)
		return
	}
	// logger добавляет идентичность роли проигрывателя к диагностике FFmpeg.
	logger := g.ffmpegLogger(observer, core.FFmpegFallbackPlayer)
	// diagnostics подтверждает полное дочитывание очереди диагностики проигрывателя до EOF.
	diagnostics := g.drainDiagnostics(supervisor, logger)
	defer g.joinDiagnostics(supervisor, diagnostics)
	// binding атомарно связывает текущую физическую попытку проигрывателя с доменным наблюдателем.
	var binding atomic.Pointer[attemptBinding]
	// err сообщает об отказе регистрации progress-метрик проигрывателя.
	if err := g.registerFFmpegMetrics(supervisor, observer, &binding); err != nil {
		g.fail(Fallback, err)
		return
	}
	if g.config.Sampler != nil {
		defer g.config.Sampler.Remove(observer.Token())
	}
	// worker задаёт файловый вход, точного владельца и область процессов проигрывателя заглушки.
	worker := ffmpeg.Worker{SessionID: string(g.parent.SessionID), GroupID: string(g.id), Owner: g.owner, ProcessScope: g.config.Scope, Topology: ffmpeg.FileInput}
	worker.Command = func(ctx context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Выдаёт новую fallback-попытку и наблюдение STARTING, запрашивает файл по манифесту и передаёт его
		// как принадлежащий команде FD даже при ошибке построителя.

		// current содержит новый номер fallback-попытки; err сохраняет результаты выделения
		// номера, открытия наблюдателя, файла и команды.
		current, err := g.nextAttempt(Fallback)
		if err != nil {
			return ffmpeg.Command{}, err
		}
		// ao содержит наблюдателя текущей доменной fallback-попытки.
		ao, err := observer.BeginAttempt(ctx, current)
		if err != nil {
			return ffmpeg.Command{}, err
		}
		_ = ao.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting})
		binding.Store(bindFFmpegAttempt(a.ID, current, ao, logger))
		// file содержит открытый по манифесту файл, передаваемый во владение физической попытки
		// даже при ошибке построителя.
		file, err := g.config.OpenFile(ctx, g.manifest)
		if err != nil {
			return ffmpeg.Command{}, err
		}
		// command содержит результат выбранного файлового построителя и принадлежащий попытке
		// дескриптор.
		var command ffmpeg.Command
		if g.config.Player != nil {
			command, err = g.config.Player(ctx, g.config.Binary, file, a)
		} else {
			command, err = ffmpeg.BuildFileCommand(g.config.Binary, file, a)
		}
		command.ExtraFiles = []*os.File{file}
		if err != nil {
			return command, err
		}
		return command, nil
	}
	worker.OnAttemptResult = g.observeAttemptResult(&binding)
	worker.Output = func(ctx context.Context, c net.Conn, progress ffmpeg.Progress) error {
		// Получает текущую привязку попытки и разбирает её fallback-выход; отсутствие привязки возвращает
		// ErrNotReady.

		// current содержит привязку выходного соединения к текущей попытке проигрывателя.
		current := binding.Load()
		if current == nil {
			return core.ErrNotReady
		}
		return g.readOutputObserved(ctx, c, Fallback, current.coreID, g.manifest.VerifiedFormat, progress, current)
	}
	if err = supervisor.Start(ctx, worker); err != nil {
		g.fail(Fallback, err)
		return
	}
	// result содержит окончательный исход и ошибки очистки Supervisor проигрывателя.
	result, _ := supervisor.Wait(context.Background())
	g.mu.Lock()
	g.fallbackEnded, g.fallbackError = true, result.Err
	g.mu.Unlock()
	g.fail(Fallback, result.Err)
	g.addCleanup(result.CleanupErr)
}

// Атомарно выдаёт возрастающий номер попытки и отзывает прежнюю готовность в переключателе до внешних
// действий; переполнение отвергается.
func (g *Group) nextAttempt(feed Feed) (uint64, error) {
	// counter выбирает атомарный счётчик попыток заданного источника.
	counter := &g.attempts[index(feed)]
	for {
		// previous содержит последний выделенный номер для проверки переполнения и атомарного
		// увеличения.
		previous := counter.Load()
		if previous == math.MaxUint64 {
			return 0, ErrFeed
		}
		if counter.CompareAndSwap(previous, previous+1) {
			if g.switcher != nil {
				// err сообщает об отказе BeginAttempt, отзывающего прежнюю готовность до
				// внешних действий.
				if err := g.switcher.BeginAttempt(feed, previous+1); err != nil {
					return 0, err
				}
			}
			return previous + 1, nil
		}
	}
}

// Отмечает окончательное завершение заглушки при соответствующем источнике и передаёт отказ текущей
// попытки переключателю, назначая первый номер при необходимости.
func (g *Group) fail(feed Feed, err error) {
	if feed == Fallback {
		g.mu.Lock()
		g.fallbackEnded, g.fallbackError = true, err
		g.mu.Unlock()
	}
	// attempt содержит текущий номер отказавшего источника, назначаемый при необходимости впервые.
	attempt := g.attempts[index(feed)].Load()
	if attempt == 0 {
		attempt, _ = g.nextAttempt(feed)
	}
	_ = g.switcher.Fail(feed, attempt, err, time.Now())
}

// Добавляет ошибку физической очистки в итог группы под блокировкой.
func (g *Group) addCleanup(err error) {
	if err != nil {
		g.mu.Lock()
		g.result = errors.Join(g.result, err)
		g.mu.Unlock()
	}
}

// Читает и проверяет выход fMP4 без отдельной привязки наблюдателя попытки, сохраняя проверку
// прогресса обеих дорожек.
func (g *Group) readOutput(ctx context.Context, c net.Conn, feed Feed, attempt uint64, verified core.MediaFormat, progress ffmpeg.Progress) error {
	return g.readOutputObserved(ctx, c, feed, attempt, verified, progress, nil)
}

// Проверяет конфигурации и единицы доступа fMP4, сообщает прогресс по DTS и готовность после обеих
// дорожек; любой возврат отзывает текущую попытку источника.
func (g *Group) readOutputObserved(ctx context.Context, c net.Conn, feed Feed, attempt uint64, verified core.MediaFormat, progress ffmpeg.Progress, binding *attemptBinding) (result error) {
	// observation содержит наблюдателя точной FFmpeg-попытки, если передана её привязка.
	var observation *core.FFmpegAttemptObserver
	if binding != nil {
		observation = binding.observer
	}
	defer func() {
		// Сообщает Switcher о завершении данной входной попытки с итоговой ошибкой, игнорируя результат Fail.
		_ = g.switcher.Fail(feed, attempt, result, time.Now())
	}()
	defer func() {
		// Записывает ошибку разбора выхода через журнал точной попытки, если привязка и журнал доступны.

		if binding != nil && binding.logger != nil && result != nil {
			binding.logger.Error("ffmpeg_output", "operation", "read_output", "error", result)
		}
	}()
	// seen отмечает получение проверенных пакетов видео и аудио в текущем выходном соединении.
	seen := [2]bool{}
	// ready исключает повторную публикацию READY одной попытки после появления обеих дорожек.
	ready := false
	// d содержит демультиплексор выходного fMP4; err сообщает об отказе чтения и проверки его
	// начальных заголовков.
	d, err := fmp4.NewDemuxer(c, fmp4.DemuxOptions{Limits: g.config.Container, SourceEpoch: attempt, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Для AV1 использует подтверждённый формат с совпадающими размерами; остальные кодеки восстанавливает
		// из описания fMP4, назначая SourceEpoch текущей попытки.

		if desc.Codec != core.CodecAV1 {
			return fmp4.ResolveCodecConfig(desc, attempt, 1)
		}
		if verified.Validate() != nil || desc.Width != verified.Video.Width || desc.Height != verified.Video.Height {
			return media.CodecConfig{}, codec.ErrUnready
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: attempt, Revision: 1, TimeBase: desc.TimeBase, Video: verified.Video}, desc.Headers)
	}})
	if err != nil {
		return err
	}
	// init содержит подтверждённую пару конфигураций прочитанного fMP4.
	init := d.Init()
	// inspector проверяет структуру единиц доступа и признаки независимого видеостарта.
	var inspector codec.Inspector
	for {
		// p содержит очередной пакет fMP4; err сохраняет результаты его чтения, проверки и
		// передачи переключателю.
		p, err := d.Next()
		if err != nil {
			return err
		}
		if _, err = inspector.Inspect(p); err != nil {
			return err
		}
		if feed == Fallback {
			g.noteInput(Fallback, p)
		}
		// track выбирает обозначение видео либо аудио для сообщения прогресса DTS.
		track := ffmpeg.Video
		if p.Config().Info().Track == media.TrackAudio {
			track = ffmpeg.Audio
		}
		if !progress(track, p.Info().DTS) {
			return ErrFeed
		}
		seen[int(track)-1] = true
		if observation != nil && !ready && seen[0] && seen[1] {
			_ = observation.Observe(ctx, core.FFmpegObservation{State: core.ObservationReady})
			ready = true
		}
		err = g.push(ctx, feed, attempt, p, init.Video, init.Audio)
		if err != nil && !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrFeed) {
			return err
		}
	}
}

// Для LIVE copy сверяет фактический ASC одновременно с входным описанием и аудиоформатом профиля;
// несовпадение возвращает и журналирует ошибку кодека.
func (g *Group) validateLiveAudio(audio media.CodecConfig) error {
	if g.config.Switcher.Profile.EffectiveLiveAudioMode() == core.LiveAudioCopy {
		// actual содержит формат, разобранный из ASC; parseErr сообщает об отказе разбора
		// фактических AAC-заголовков.
		actual, parseErr := codec.AudioFormatASC(audio.Headers())
		// expected содержит аудиоформат профиля, обязательный для LIVE AAC copy.
		expected := g.config.Switcher.Profile.Format.Audio
		if parseErr != nil || actual != audio.Info().Audio || actual != expected {
			// err описывает несовместимость фактического ASC, входного описания и целевого
			// аудиоформата.
			err := fmt.Errorf("%w: LIVE AAC copy: ASC=%+v, входной формат=%+v, ожидаемый формат=%+v", codec.ErrCodec, actual, audio.Info().Audio, expected)
			if parseErr != nil {
				err = fmt.Errorf("%w: %w", err, parseErr)
			}
			if g.config.Logger != nil {
				g.config.Logger.Error("group_live", "component", "group", "operation", "validate_audio", "session_id", g.parent.SessionID, "group_id", g.id, "group_instance_id", g.observer.Token(), "error", err)
			}
			return err
		}
	}
	return nil
}

// Получает актуальное начало LIVE, запускает один работник без внутренних повторов и передаёт вход
// fMP4 до смены конфигурации; выход проходит проверку, завершение ждёт Supervisor и учитывает очистку.
func (g *Group) transcode(ctx context.Context, sub *media.Subscription, attempt uint64) error {
	// first содержит актуальное начало подписки LIVE; err сохраняет результаты получения данных,
	// создания и ожидания работника перекодирования.
	first, err := sub.Next(ctx)
	if err != nil {
		return err
	}
	if !first.IsCurrent() {
		return ErrFeed
	}
	g.noteInput(Live, first.Packet)
	if err = g.validateLiveAudio(first.AudioConfig); err != nil {
		return err
	}
	// observer содержит наблюдателя роли LIVE-перекодирования группы.
	observer, err := g.observer.OpenFFmpegRole(ctx, core.FFmpegLiveTranscode)
	if err != nil {
		return err
	}
	defer func() {
		// Закрывает наблюдателя роли LIVE transcode с отдельным сроком завершения, игнорируя ошибку.

		// c ограничивает закрытие наблюдателя LIVE отдельным сроком; cancel освобождает таймер.
		c, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
		defer cancel()
		_ = observer.Close(c)
	}()
	// Каждая новая входная конфигурация получает отдельный работник; исходный Router остаётся.
	// workerConfig отключает внутренние повторы Supervisor: новая конфигурация LIVE получает
	// отдельного работника.
	workerConfig := g.config.FFmpeg
	workerConfig.MaxRestarts = 0
	// supervisor выполняет единственную физическую попытку перекодирования текущей конфигурации
	// LIVE.
	supervisor, err := ffmpeg.New(workerConfig)
	if err != nil {
		return err
	}
	// logger связывает диагностику перекодирования с точной ролью LIVE.
	logger := g.ffmpegLogger(observer, core.FFmpegLiveTranscode)
	// diagnostics подтверждает полное дочитывание диагностики LIVE после остановки Supervisor.
	diagnostics := g.drainDiagnostics(supervisor, logger)
	defer g.joinDiagnostics(supervisor, diagnostics)
	// input содержит фактический формат видео и аудио первой актуальной выдачи LIVE.
	input := core.MediaFormat{Video: first.VideoConfig.Info().Video, Audio: first.AudioConfig.Info().Audio}
	// binding атомарно связывает физическую попытку LIVE с доменным номером и наблюдателем.
	var binding atomic.Pointer[attemptBinding]
	// err сообщает об отказе регистрации progress-метрик LIVE-перекодирования.
	if err := g.registerFFmpegMetrics(supervisor, observer, &binding); err != nil {
		return err
	}
	if g.config.Sampler != nil {
		defer g.config.Sampler.Remove(observer.Token())
	}
	// worker задаёт дуплексную попытку LIVE с точным владельцем Session и областью процессов.
	worker := ffmpeg.Worker{SessionID: string(g.parent.SessionID), GroupID: string(g.id), Owner: g.owner, ProcessScope: g.config.Scope}
	worker.Command = func(ctx context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Создаёт наблюдение текущей LIVE-попытки и команду перекодирования по исходным конфигурациям и
		// профилю через выбранный построитель либо BuildNVIDIACommand.

		// ao содержит наблюдателя текущей LIVE-попытки; err сообщает об отказе его открытия.
		ao, err := observer.BeginAttempt(ctx, attempt)
		if err != nil {
			return ffmpeg.Command{}, err
		}
		_ = ao.Observe(ctx, core.FFmpegObservation{State: core.ObservationStarting})
		binding.Store(bindFFmpegAttempt(a.ID, attempt, ao, logger))
		// config задаёт аппаратную команду из фактического входа, целевого профиля и исходных
		// шкал обеих дорожек.
		config := ffmpeg.NVIDIAConfig{Binary: g.config.Binary, Input: input, Profile: g.config.Switcher.Profile, VideoTimeBase: first.VideoConfig.Info().TimeBase, AudioTimeBase: first.AudioConfig.Info().TimeBase}
		if g.config.Transcode != nil {
			return g.config.Transcode(ctx, config, a)
		}
		return ffmpeg.BuildNVIDIACommand(config, a)
	}
	worker.OnAttemptResult = g.observeAttemptResult(&binding)
	worker.Input = func(ctx context.Context, c net.Conn, _ ffmpeg.Progress) error {
		// Передаёт LIVE через fMP4 с общим смещением начала, проверяя представимость времени и актуальность
		// выдач; смена любой конфигурации завершает работника с ErrStaleSource.

		// origin содержит общий сдвиг начала для транспорта fMP4; err сохраняет результаты его
		// вычисления, создания muxer и передачи пакетов.
		origin, err := transportOrigin(first)
		if err != nil {
			return err
		}
		// mux записывает входной LIVE в fMP4 с исходными конфигурациями и общим сдвигом начала.
		mux, err := fmp4.NewMuxer(c, fmp4.Init{Video: first.VideoConfig, Audio: first.AudioConfig, Origin: origin}, g.config.Container)
		if err != nil {
			return err
		}
		// validator проверяет представимость временных меток для входного порта FFmpeg при
		// выбранном сдвиге.
		validator := fmp4.NewFFmpegTimingValidatorForOrigin(origin)
		// delivery содержит текущую выдачу LIVE, начиная с уже прочитанного первого пакета.
		delivery := first
		for {
			if !delivery.IsCurrent() {
				return media.ErrStaleSource
			}
			if err = validator.Validate(delivery.Packet); err != nil {
				return err
			}
			if err = mux.WritePacket(delivery.Packet); err != nil {
				return err
			}
			delivery, err = sub.Next(ctx)
			if err != nil {
				return err
			}
			if delivery.IsCurrent() {
				g.noteInput(Live, delivery.Packet)
			}
			if !sameConfig(delivery.VideoConfig, first.VideoConfig) || !sameConfig(delivery.AudioConfig, first.AudioConfig) {
				return media.ErrStaleSource
			}
		}
	}
	worker.Output = func(ctx context.Context, c net.Conn, progress ffmpeg.Progress) error {
		// Разбирает выход LIVE transcode с текущей привязкой наблюдения и сообщает прогресс проверенных AU.

		return g.readOutputObserved(ctx, c, Live, attempt, core.MediaFormat{}, progress, binding.Load())
	}
	if err = supervisor.Start(ctx, worker); err != nil {
		return err
	}
	// result содержит исход и ошибки очистки единственной попытки LIVE-перекодирования.
	result, err := supervisor.Wait(context.Background())
	g.addCleanup(result.CleanupErr)
	return errors.Join(err, result.Err)
}

// Вычисляет точный положительный сдвиг отрицательного начального DTS на общей шкале дорожек;
// неотрицательное начало оставляет без сдвига.
func transportOrigin(d media.Delivery) (fmp4.OriginMapping, error) {
	// scale содержит общую точную шкалу входных дорожек; err сохраняет результаты вычисления шкалы и
	// начального сдвига.
	scale, err := fmp4.MovieTimeScale(d.VideoConfig.Info().TimeBase, d.AudioConfig.Info().TimeBase)
	if err != nil {
		return fmp4.OriginMapping{}, err
	}
	// base задаёт временную базу общего транспортного сдвига.
	base := media.TimeBase{Numerator: 1, Denominator: scale}
	// start содержит начальный DTS в общей шкале для проверки необходимости положительного сдвига.
	start, err := rescale(d.Packet.Info().DTS, d.Packet.Config().Info().TimeBase, base)
	if err != nil {
		return fmp4.OriginMapping{}, err
	}
	if start >= 0 {
		return fmp4.OriginMapping{}, nil
	}
	// offset содержит величину сдвига отрицательного начала до нуля в общей шкале.
	offset, err := difference(0, start)
	return fmp4.OriginMapping{OffsetTicks: offset, TimeBase: base}, err
}

// Joined сообщает завершение работников независимо от долговечной очистки файлов.
func (r *Registry) Joined() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	// g содержит очередную зарегистрированную группу для проверки фактического завершения её
	// работников.
	for _, g := range r.groups {
		select {
		case <-g.done:
		default:
			return false
		}
	}
	return r.closed
}
