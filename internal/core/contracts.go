package core

import (
	"context"
	"time"
)

// UnitOfWork ограничивает время жизни репозиториев одним вызовом. Write может
// повторить целиком откатившуюся функцию; внутри разрешены только операции БД.
// Ошибка Write может сопровождать уже выполненный COMMIT. Перед повтором внешнего
// действия результат команды проверяется по сохранённой квитанции.
type UnitOfWork interface {

	// View предоставляет репозитории согласованного чтения только на время вызова переданной функции;
	// запись через них запрещена.
	View(context.Context, func(Repositories) error) error

	// Write выполняет переданную функцию в транзакции и может повторить только полностью откатившуюся
	// операцию БД. Ошибка может сопровождать уже состоявшийся COMMIT, поэтому внешний результат
	// проверяется по квитанции.
	Write(context.Context, func(Repositories) error) error
}

// Repositories предоставляет согласованные репозитории только на время операции UnitOfWork.
type Repositories struct {
	// Config предоставляет доступ к текущей конфигурации потоков, групп, профилей и назначений.
	Config ConfigRepository
	// Runtime предоставляет доступ к долговечному управлению сессиями, командами и удержаниями.
	Runtime RuntimeRepository
	// Artifacts предоставляет доступ к поколениям заглушек и журналу принадлежащих им файлов.
	Artifacts ArtifactRepository
}

// ConfigRepository задаёт операции чтения и условного изменения текущей конфигурации.
type ConfigRepository interface {

	// Stream читает проверенную конфигурацию Stream по ID в текущей транзакции.
	Stream(context.Context, ID) (Stream, error)

	// Streams читает ограниченный упорядоченный список конфигураций Stream с разрешёнными фильтрами.
	Streams(context.Context, ListQuery) ([]Stream, error)

	// InsertStream проверяет и сохраняет новую конфигурацию Stream и её индексируемые поля.
	InsertStream(context.Context, Stream) error

	// UpdateStream заменяет конфигурацию при ожидаемой ревизии, запрещая изменение ключа публикации.
	UpdateStream(context.Context, Stream, uint64) error

	// DeleteStream удаляет Stream при ожидаемой ревизии; ограничения связей и удержаний проверяет
	// хранилище.
	DeleteStream(context.Context, ID, uint64) error

	// Profile читает полный сохранённый профиль по ID без назначения значений по умолчанию.
	Profile(context.Context, ID) (TranscodeProfile, error)

	// Profiles читает ограниченную страницу сохраняемых профилей.
	Profiles(context.Context, ListQuery) ([]TranscodeProfile, error)

	// InsertProfile проверяет и сохраняет полный профиль вместе с его записью.
	InsertProfile(context.Context, TranscodeProfile) error

	// UpdateProfile заменяет проверенный профиль при следующей ревизии после ожидаемой.
	UpdateProfile(context.Context, TranscodeProfile, uint64) error

	// DeleteProfile удаляет профиль при ожидаемой ревизии с сохранением ограничений сохраняемых ссылок.
	DeleteProfile(context.Context, ID, uint64) error

	// Group читает конфигурацию группы и её ссылки по ID.
	Group(context.Context, ID) (TranscodeGroup, error)

	// Groups читает ограниченный упорядоченный список групп с фильтрами Stream и профиля.
	Groups(context.Context, ListQuery) ([]TranscodeGroup, error)

	// InsertGroup проверяет и сохраняет новую группу и её ссылки.
	InsertGroup(context.Context, TranscodeGroup) error

	// UpdateGroup заменяет проверенную группу при ожидаемой ревизии и неизменном времени создания.
	UpdateGroup(context.Context, TranscodeGroup, uint64) error

	// DeleteGroup удаляет группу при ожидаемой ревизии; используемые связи защищены ограничениями БД.
	DeleteGroup(context.Context, ID, uint64) error

	// Destination читает настройки назначения по ID.
	Destination(context.Context, ID) (Destination, error)

	// Destinations читает ограниченный упорядоченный список назначений с разрешёнными фильтрами.
	Destinations(context.Context, ListQuery) ([]Destination, error)

	// InsertDestination проверяет и сохраняет назначение; принадлежность группы Stream дополнительно
	// проверяет хранилище.
	InsertDestination(context.Context, Destination) error

	// UpdateDestination заменяет проверенные настройки назначения при ожидаемой ревизии.
	UpdateDestination(context.Context, Destination, uint64) error

	// DeleteDestination удаляет назначение при ожидаемой ревизии, сохраняя ограничения удержаний.
	DeleteDestination(context.Context, ID, uint64) error
}

// ArtifactRepository задаёт операции хранения заглушек, поколений, вариантов и намерений очистки.
type ArtifactRepository interface {

	// Generation читает полные метаданные и протокол подготовки поколения по ID.
	Generation(context.Context, ID) (FallbackGeneration, error)

	// Generations читает ограниченный список поколений с разрешёнными фильтрами.
	Generations(context.Context, ListQuery) ([]FallbackGeneration, error)

	// InsertGeneration проверяет и сохраняет новое поколение с полным комплектом требуемых профилей.
	InsertGeneration(context.Context, FallbackGeneration) error

	// UpdateGeneration обновляет протокол подготовки при ожидаемой ревизии, сохраняя происхождение и
	// полный набор профилей; терминальное содержимое неизменяемо.
	UpdateGeneration(context.Context, FallbackGeneration, uint64) error

	// DeleteGeneration удаляет метаданные поколения при ожидаемой ревизии; удержания защищаются
	// хранилищем, файлы удаляются отдельно.
	DeleteGeneration(context.Context, ID, uint64) error

	// InsertOwnedFile проверяет и сохраняет происхождение управляемого пути поколения независимо от
	// наличия файла на диске.
	InsertOwnedFile(context.Context, OwnedFile) error

	// UpdateOwnedFile заполняет хеш и размер исходно пустой записи либо обновляет её Record при ожидаемой
	// ревизии; происхождение и уже записанное содержимое неизменяемы.
	UpdateOwnedFile(context.Context, OwnedFile, uint64) error

	// OwnedFiles читает ограниченную страницу записей происхождения управляемых путей.
	OwnedFiles(context.Context, ListQuery) ([]OwnedFile, error)

	// Asset читает стабильный объект заглушки и указатели поколений по ID.
	Asset(context.Context, ID) (FallbackAsset, error)

	// Assets читает ограниченную страницу метаданных заглушек.
	Assets(context.Context, ListQuery) ([]FallbackAsset, error)

	// InsertAsset проверяет и сохраняет стабильный объект заглушки.
	InsertAsset(context.Context, FallbackAsset) error

	// UpdateAsset заменяет метаданные заглушки при ожидаемой ревизии, включая указатели текущего и
	// ожидающего поколений.
	UpdateAsset(context.Context, FallbackAsset, uint64) error

	// DeleteAsset удаляет метаданные заглушки при ожидаемой ревизии; связанные конфигурации и удержания
	// защищает БД.
	DeleteAsset(context.Context, ID, uint64) error

	// Variant читает метаданные конкретного варианта с проверкой сохранённой нагрузки.
	Variant(context.Context, ID) (FallbackVariant, error)

	// Variants читает ограниченный список вариантов с фильтрами поколения и заглушки.
	Variants(context.Context, ListQuery) ([]FallbackVariant, error)

	// InsertVariant проверяет и сохраняет вариант с точным профилем и принадлежностью поколению.
	InsertVariant(context.Context, FallbackVariant) error

	// UpdateVariant обновляет вариант при ожидаемой ревизии, сохраняя его происхождение и неизменность
	// завершённого содержимого.
	UpdateVariant(context.Context, FallbackVariant, uint64) error

	// DeleteVariant удаляет метаданные варианта при ожидаемой ревизии; физический файл этим не удаляется.
	DeleteVariant(context.Context, ID, uint64) error

	// InsertCleanup проверяет и сохраняет намерение точной очистки ресурсов исполнения либо файла.
	InsertCleanup(context.Context, CleanupIntent) error

	// Cleanup читает сохраняемое намерение очистки по ID.
	Cleanup(context.Context, ID) (CleanupIntent, error)

	// Cleanups читает ограниченный список намерений очистки, включая неподтверждённые.
	Cleanups(context.Context, ListQuery) ([]CleanupIntent, error)

	// UpdateCleanup обновляет подтверждение очистки при ожидаемой ревизии, запрещая изменение
	// происхождения и владельца.
	UpdateCleanup(context.Context, CleanupIntent, uint64) error
}

// RuntimeRepository задаёт хранение жизненного цикла запусков, команд, событий и удержаний.
type RuntimeRepository interface {

	// GenerationHeld проверяет наличие хотя бы одного удержания поколения среди Session и Output в текущем
	// согласованном чтении.
	GenerationHeld(context.Context, ID) (bool, error)

	// Session читает долговечное состояние и снимок запуска по ID.
	Session(context.Context, ID) (Session, error)

	// Sessions читает ограниченную страницу долговечных запусков с разрешёнными фильтрами.
	Sessions(context.Context, ListQuery) ([]Session, error)

	// ActiveSession читает единственную STARTING, RUNNING или STOPPING-сессию Stream; отсутствие
	// переводится в ErrNotFound.
	ActiveSession(context.Context, ID) (Session, error)

	// InsertSession проверяет и сохраняет новый запуск; единственность нетерминальной Session защищает
	// хранилище.
	InsertSession(context.Context, Session) error

	// UpdateSession обновляет управление запуском при ожидаемой ревизии, сохраняя Stream, incarnation и
	// исходный снимок.
	UpdateSession(context.Context, Session, uint64) error

	// Output читает сохраняемое намерение и снимок выхода без оперативного состояния соединения.
	Output(context.Context, ID) (SessionOutput, error)

	// Outputs читает ограниченную страницу сохраняемых выходов; неподдерживаемые фильтры отклоняются.
	Outputs(context.Context, ListQuery) ([]SessionOutput, error)

	// InsertOutput проверяет и сохраняет новый выход с его самостоятельным снимком.
	InsertOutput(context.Context, SessionOutput) error

	// UpdateOutput обновляет намерение, поколения и очистку выхода при ожидаемой ревизии, сохраняя
	// SessionID и снимок.
	UpdateOutput(context.Context, SessionOutput, uint64) error

	// Receipt читает квитанцию по исходному RequestID с проверкой её сохраняемых метаданных.
	Receipt(context.Context, string) (CommandReceipt, error)

	// ReceiptByID читает и проверяет квитанцию по внутреннему ID.
	ReceiptByID(context.Context, ID) (CommandReceipt, error)

	// Receipts читает ограниченный список квитанций, включая фильтр незавершённых команд.
	Receipts(context.Context, ListQuery) ([]CommandReceipt, error)

	// InsertReceipt сохраняет квитанцию и её индексируемую идентичность в текущей транзакции записи.
	InsertReceipt(context.Context, CommandReceipt) error

	// UpdateReceipt обновляет состояние квитанции при ожидаемой ревизии, сохраняя исходную идентичность
	// запроса и владельца.
	UpdateReceipt(context.Context, CommandReceipt, uint64) error

	// InsertEvent проверяет и сохраняет неизменяемое событие протокола управления.
	InsertEvent(context.Context, Event) error

	// Events читает ограниченную упорядоченную страницу долговечных событий управления.
	Events(context.Context, ListQuery) ([]Event, error)

	// InsertHold сохраняет ссылки удержания Session либо конкретного Output; вид таблицы выбирается по
	// наличию OutputID.
	InsertHold(context.Context, ReferenceHold) error

	// ReleaseHolds удаляет удержания указанного выхода, а при пустом OutputID — все удержания выходов и
	// самой Session; пустой SessionID отклоняется.
	ReleaseHolds(context.Context, ID, ID) error
}

// ListQuery задаёт только известные фильтры и возрастающую границу (CreatedAt, ID).
// Репозиторий материализует не более Limit строк. Пустой OutputID в ReleaseHolds
// освобождает все удержания Session; непустой — только конкретный выход.
type ListQuery struct {
	// Limit ограничивает число материализуемых строк страницы.
	Limit int
	// StreamID ограничивает список конкретным потоком.
	StreamID ID
	// SessionID ограничивает список конкретной сессией.
	SessionID ID
	// OutputID ограничивает список конкретным выходом.
	OutputID ID
	// AssetID ограничивает список конкретной заглушкой.
	AssetID ID
	// GenerationID ограничивает список конкретным поколением заглушки.
	GenerationID ID
	// GroupID ограничивает список конкретной группой.
	GroupID ID
	// ProfileID ограничивает список конкретным профилем.
	ProfileID ID
	// State задаёт разрешённый для вида списка фильтр сохраняемого состояния.
	State string
	// Incarnation выбирает записи указанного владельца открытия хранилища.
	Incarnation string
	// ExcludeIncarnation исключает записи указанного владельца открытия хранилища.
	ExcludeIncarnation string
	// PendingOnly выбирает только незавершённые записи соответствующего вида списка.
	PendingOnly bool
	// From задаёт включительную нижнюю границу времени создания.
	From *time.Time
	// To задаёт исключительную верхнюю границу времени создания.
	To *time.Time
	// After задаёт исключительную границу следующей страницы по времени и ID.
	After *ListBoundary
}

// ListBoundary фиксирует последнюю пару времени создания и идентификатора для продолжения списка.
type ListBoundary struct {
	// CreatedAt содержит время создания последней строки предыдущей страницы.
	CreatedAt time.Time
	// ID разрешает равенство времён создания порядком идентификаторов.
	ID ID
}

// MaxListLimit ограничивает размер одной страницы доменного списка.
const MaxListLimit = 500

// Validate проверяет идентификаторы, текстовые фильтры, диапазон времени и границу страницы; Limit
// должен быть от 1 до MaxListLimit, а From — строго раньше To.
func (q ListQuery) Validate() error {
	// id содержит очередной непустой идентификатор фильтра для проверки.
	for _, id := range []ID{q.StreamID, q.SessionID, q.OutputID, q.AssetID, q.GenerationID, q.GroupID, q.ProfileID} {
		if id != "" {
			// err содержит причину отказа проверки идентификатора фильтра.
			if err := id.Validate(); err != nil {
				return err
			}
		}
	}
	// value содержит очередной текстовый фильтр состояния или владельца.
	for _, value := range []string{q.State, q.Incarnation, q.ExcludeIncarnation} {
		if value != "" {
			// err содержит причину отказа проверки текста фильтра.
			if err := validText("ListQuery", value); err != nil {
				return err
			}
		}
	}
	// value содержит проверяемую границу временного диапазона.
	for _, value := range []*time.Time{q.From, q.To} {
		if value != nil {
			// err содержит причину непредставимости временной границы.
			if _, err := NormalizeTimestamp(*value); err != nil {
				return err
			}
		}
	}
	if q.After != nil {
		// err содержит причину отказа проверки идентификатора границы страницы.
		if err := q.After.ID.Validate(); err != nil {
			return err
		}
		// err содержит причину непредставимости времени границы страницы.
		if _, err := NormalizeTimestamp(q.After.CreatedAt); err != nil {
			return err
		}
	}
	if q.Limit < 1 || q.Limit > MaxListLimit {
		return ErrInvalid
	}
	if q.From != nil && q.To != nil && !q.From.Before(*q.To) {
		return ErrInvalid
	}
	return nil
}
