package core

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// MaxPageLimit ограничивает запрошенную страницу так, чтобы оставалось место для одной проверочной
// строки.
const MaxPageLimit = MaxListLimit - 1

// PageRequest задаёт фильтры и необязательный курсор сохраняемого списка.
type PageRequest struct {
	// Query задаёт фильтры и желаемое число элементов страницы.
	Query ListQuery
	// Cursor задаёт непрозрачное продолжение прежней страницы либо пустое значение для начала.
	Cursor string
}

// Page возвращает элементы сохраняемого списка и продолжение при наличии следующей страницы.
type Page[T any] struct {
	// Items содержит материализованные элементы текущей страницы.
	Items []T
	// NextCursor содержит курсор следующей страницы либо пустую строку после последней.
	NextCursor string
}

// pageCursor связывает вид списка, нормализованные фильтры и исключительную границу продолжения.
type pageCursor struct {
	// Version фиксирует версию представления курсора.
	Version int
	// Kind фиксирует вид списка, для которого выдан курсор.
	Kind string
	// Filter сохраняет фильтры запроса без размера страницы и прежней границы.
	Filter ListQuery
	// Boundary сохраняет последнюю выданную пару времени создания и ID.
	Boundary ListBoundary
}

// NormalizeTimestamp проверяет диапазон микросекунд Unix и убирает локальную
// зону и доли микросекунды перед сохранением или сравнением.
func NormalizeTimestamp(value time.Time) (time.Time, error) {
	// normalized содержит время в UTC с точностью до микросекунды.
	normalized := value.UTC().Truncate(time.Microsecond)
	// decoded восстанавливает время из Unix-микросекунд для проверки представимости.
	decoded := time.UnixMicro(normalized.UnixMicro()).UTC()
	if !decoded.Equal(normalized) {
		return time.Time{}, fmt.Errorf("%w: время вне диапазона микросекунд", ErrInvalid)
	}
	return decoded, nil
}

// normalizeQuery проверяет фильтры, переводит границы диапазона в UTC и нормализует время курсора до
// микросекунд, не меняя переданные указатели.
func normalizeQuery(q ListQuery) (ListQuery, error) {
	// err содержит итог проверки нормализованных фильтров списка.
	var err error
	if q.From != nil {
		// e сообщает об ошибке проверки представимости нижней временной границы.
		if _, e := NormalizeTimestamp(*q.From); e != nil {
			return ListQuery{}, e
		}
		// value содержит отдельную UTC-копию нижней границы без изменения указателя вызывающего.
		value := q.From.UTC()
		q.From = &value
	}
	if q.To != nil {
		// e сообщает об ошибке проверки представимости верхней временной границы.
		if _, e := NormalizeTimestamp(*q.To); e != nil {
			return ListQuery{}, e
		}
		// value содержит отдельную UTC-копию верхней границы без изменения указателя
		// вызывающего.
		value := q.To.UTC()
		q.To = &value
	}
	if q.After != nil {
		// value содержит нормализованное время границы страницы, а e — результат проверки его
		// представимости.
		value, e := NormalizeTimestamp(q.After.CreatedAt)
		if e != nil {
			return ListQuery{}, e
		}
		if e = q.After.ID.Validate(); e != nil {
			return ListQuery{}, e
		}
		q.After = &ListBoundary{value, q.After.ID}
	}
	err = q.Validate()
	return q, err
}

// pageQuery проверяет страницу и соответствие курсора виду списка и фильтрам; добавочная строка
// позволяет определить наличие следующей страницы.
func pageQuery(kind string, request PageRequest) (ListQuery, error) {
	if request.Query.After != nil || request.Query.Limit < 1 || request.Query.Limit > MaxPageLimit {
		return ListQuery{}, ErrInvalid
	}
	// q содержит нормализованные фильтры списка, а err — результат их проверки.
	q, err := normalizeQuery(request.Query)
	if err != nil {
		return ListQuery{}, err
	}
	if request.Cursor != "" {
		if len(request.Cursor) > 32768 {
			return ListQuery{}, ErrInvalidCursor
		}
		// data содержит декодированные байты курсора, а err — результат разбора Base64URL.
		data, err := base64.RawURLEncoding.DecodeString(request.Cursor)
		if err != nil {
			return ListQuery{}, ErrInvalidCursor
		}
		// c принимает вид списка, фильтры и границу из переданного курсора.
		var c pageCursor
		if err = strictJSON(data, &c); err != nil || c.Version != MetadataVersion || c.Kind != kind {
			return ListQuery{}, ErrInvalidCursor
		}
		// filter содержит текущие фильтры без размера страницы для сравнения с курсором.
		filter := q
		filter.Limit = 0
		// expected содержит точное JSON-представление ожидаемых фильтров.
		expected, err := json.Marshal(filter)
		if err != nil {
			return ListQuery{}, err
		}
		// actual содержит точное JSON-представление фильтров сохранённого курсора.
		actual, err := json.Marshal(c.Filter)
		if err != nil {
			return ListQuery{}, err
		}
		if !bytes.Equal(expected, actual) {
			return ListQuery{}, ErrInvalidCursor
		}
		if c.Boundary.ID.Validate() != nil {
			return ListQuery{}, ErrInvalidCursor
		}
		// boundaryTime содержит нормализованное время границы для проверки точности курсора.
		boundaryTime, err := NormalizeTimestamp(c.Boundary.CreatedAt)
		if err != nil || !boundaryTime.Equal(c.Boundary.CreatedAt) {
			return ListQuery{}, ErrInvalidCursor
		}
		q.After = &c.Boundary
	}
	q.Limit++
	return q, nil
}

// nextCursor кодирует вид списка, фильтры без размера страницы и последнюю выданную границу в курсор
// Base64URL.
func nextCursor(kind string, q ListQuery, boundary ListBoundary) (string, error) {
	q.Limit = 0
	q.After = nil
	// data содержит JSON нового курсора, а err — результат его кодирования.
	data, err := json.Marshal(pageCursor{MetadataVersion, kind, q, boundary})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// readPage читает ограниченную страницу в одной транзакции и создаёт следующий курсор только при
// наличии добавочной строки.
func readPage[T any](ctx context.Context, s *Service, kind string, request PageRequest, read func(Repositories, ListQuery) ([]T, error), boundary func(T) ListBoundary) (Page[T], error) {
	// q содержит проверенные фильтры с добавочной строкой, а err — результат проверки страницы.
	q, err := pageQuery(kind, request)
	if err != nil {
		return Page[T]{}, err
	}
	var values []T
	err = s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет values результатом выбранного запроса с дополнительной строкой для
		// определения следующей страницы.
		// err сообщает об ошибке чтения страницы выбранного сохраняемого списка.
		var err error
		values, err = read(r, q)
		return err
	})
	if err != nil {
		return Page[T]{}, err
	}
	// result содержит возвращаемую страницу после удаления добавочной строки и назначения
	// продолжения.
	result := Page[T]{Items: values}
	if len(values) > request.Query.Limit {
		result.Items = values[:request.Query.Limit]
		result.NextCursor, err = nextCursor(kind, q, boundary(result.Items[len(result.Items)-1]))
		if err != nil {
			return Page[T]{}, err
		}
	}
	return result, nil
}

// AuthorizeOBS возвращает текущую сессию Stream только в RUNNING при совпадении ключа со снимком
// запуска; сравнение ключей использует ConstantTimeCompare.
func (s *Service) AuthorizeOBS(ctx context.Context, streamID ID, key string) (Session, error) {
	// result принимает текущую RUNNING-сессию при совпадении сохранённого ключа OBS.
	var result Session
	// err сообщает об ошибке согласованной проверки допуска OBS.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении допускает OBS только для SessionRunning и совпадающего ключа из снимка,
		// сравниваемого ConstantTimeCompare.

		// session содержит текущий запуск потока, а err — результат его чтения.
		session, err := r.Runtime.ActiveSession(ctx, streamID)
		if err != nil {
			return err
		}
		if session.State != SessionRunning || subtle.ConstantTimeCompare([]byte(session.Snapshot.Stream.PublicationKey), []byte(key)) != 1 {
			return ErrNotReady
		}
		result = session
		return nil
	})
	return result, err
}

// GetStream читает текущую конфигурацию Stream по ID в транзакции чтения.
func (s *Service) GetStream(ctx context.Context, id ID) (Stream, error) {
	// result принимает текущую конфигурацию потока, прочитанную по ID.
	var result Stream
	// err содержит итог согласованного чтения конфигурации потока.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением Stream по ID и возвращает ошибку репозитория.
		// err передаёт результат чтения конфигурации потока из репозитория.
		var err error
		result, err = r.Config.Stream(ctx, id)
		return err
	})
	return result, err
}

// ListStreams возвращает страницу конфигураций Stream с курсором по времени создания и ID.
func (s *Service) ListStreams(ctx context.Context, request PageRequest) (Page[Stream], error) {
	return readPage(ctx, s, "streams", request, func(r Repositories, q ListQuery) ([]Stream, error) {
		// Читает материализованный список потоков по уже проверенным фильтрам и границе страницы.
		return r.Config.Streams(ctx, q)
	}, func(value Stream) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// GetProfile читает сохранённый полный профиль по ID без назначения значений по умолчанию.
func (s *Service) GetProfile(ctx context.Context, id ID) (TranscodeProfile, error) {
	var result TranscodeProfile
	// err содержит итог согласованного чтения полного профиля.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением TranscodeProfile по ID и возвращает ошибку
		// репозитория.
		// err передаёт результат чтения полного профиля из репозитория.
		var err error
		result, err = r.Config.Profile(ctx, id)
		return err
	})
	return result, err
}

// ListProfiles возвращает страницу сохраняемых профилей с проверкой фильтров и курсора.
func (s *Service) ListProfiles(ctx context.Context, request PageRequest) (Page[TranscodeProfile], error) {
	return readPage(ctx, s, "profiles", request, func(r Repositories, q ListQuery) ([]TranscodeProfile, error) {
		// Читает материализованный список профилей по уже проверенным фильтрам и границе страницы.
		return r.Config.Profiles(ctx, q)
	}, func(value TranscodeProfile) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// GetGroup читает текущую конфигурацию группы по ID.
func (s *Service) GetGroup(ctx context.Context, id ID) (TranscodeGroup, error) {
	var result TranscodeGroup
	// err содержит итог согласованного чтения конфигурации группы.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением TranscodeGroup по ID и возвращает ошибку
		// репозитория.
		// err передаёт результат чтения конфигурации группы из репозитория.
		var err error
		result, err = r.Config.Group(ctx, id)
		return err
	})
	return result, err
}

// ListGroups возвращает страницу конфигураций групп с проверкой фильтров и курсора.
func (s *Service) ListGroups(ctx context.Context, request PageRequest) (Page[TranscodeGroup], error) {
	return readPage(ctx, s, "groups", request, func(r Repositories, q ListQuery) ([]TranscodeGroup, error) {
		// Читает материализованный список групп по уже проверенным фильтрам и границе страницы.
		return r.Config.Groups(ctx, q)
	}, func(value TranscodeGroup) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// GetDestination читает текущие настройки назначения по ID.
func (s *Service) GetDestination(ctx context.Context, id ID) (Destination, error) {
	var result Destination
	// err содержит итог согласованного чтения настроек назначения.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением Destination по ID и возвращает ошибку репозитория.
		// err передаёт результат чтения настроек назначения из репозитория.
		var err error
		result, err = r.Config.Destination(ctx, id)
		return err
	})
	return result, err
}

// ListDestinations возвращает страницу текущих назначений с проверкой фильтров и курсора.
func (s *Service) ListDestinations(ctx context.Context, request PageRequest) (Page[Destination], error) {
	return readPage(ctx, s, "destinations", request, func(r Repositories, q ListQuery) ([]Destination, error) {
		// Читает материализованный список назначений по уже проверенным фильтрам и границе страницы.
		return r.Config.Destinations(ctx, q)
	}, func(value Destination) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// GetAsset читает сохраняемые метаданные заглушки и указатели её поколений.
func (s *Service) GetAsset(ctx context.Context, id ID) (FallbackAsset, error) {
	var result FallbackAsset
	// err содержит итог согласованного чтения метаданных заглушки.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением FallbackAsset по ID и возвращает ошибку
		// репозитория.
		// err передаёт результат чтения метаданных заглушки из репозитория.
		var err error
		result, err = r.Artifacts.Asset(ctx, id)
		return err
	})
	return result, err
}

// ListAssets возвращает страницу метаданных заглушек с проверкой фильтров и курсора.
func (s *Service) ListAssets(ctx context.Context, request PageRequest) (Page[FallbackAsset], error) {
	return readPage(ctx, s, "assets", request, func(r Repositories, q ListQuery) ([]FallbackAsset, error) {
		// Читает материализованный список заглушек по уже проверенным фильтрам и границе страницы.
		return r.Artifacts.Assets(ctx, q)
	}, func(value FallbackAsset) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// GetVariant читает сохраняемые метаданные конкретного подготовленного варианта.
func (s *Service) GetVariant(ctx context.Context, id ID) (FallbackVariant, error) {
	var result FallbackVariant
	// err содержит итог согласованного чтения метаданных варианта.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением FallbackVariant по ID и возвращает ошибку
		// репозитория.
		// err передаёт результат чтения метаданных варианта из репозитория.
		var err error
		result, err = r.Artifacts.Variant(ctx, id)
		return err
	})
	return result, err
}

// ListVariants возвращает страницу вариантов заглушек с проверкой фильтров и курсора.
func (s *Service) ListVariants(ctx context.Context, request PageRequest) (Page[FallbackVariant], error) {
	return readPage(ctx, s, "variants", request, func(r Repositories, q ListQuery) ([]FallbackVariant, error) {
		// Читает материализованный список вариантов заглушки по уже проверенным фильтрам и границе страницы.
		return r.Artifacts.Variants(ctx, q)
	}, func(value FallbackVariant) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// GetSession читает долговечное состояние и снимок запуска, не подставляя оперативные наблюдения.
func (s *Service) GetSession(ctx context.Context, id ID) (Session, error) {
	var result Session
	// err содержит итог согласованного чтения сохраняемой сессии.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением Session по ID и возвращает ошибку репозитория.
		// err передаёт результат чтения сохраняемой сессии из репозитория.
		var err error
		result, err = r.Runtime.Session(ctx, id)
		return err
	})
	return result, err
}

// ListSessions возвращает страницу долговечной истории запусков с проверкой фильтров и курсора.
func (s *Service) ListSessions(ctx context.Context, request PageRequest) (Page[Session], error) {
	return readPage(ctx, s, "sessions", request, func(r Repositories, q ListQuery) ([]Session, error) {
		// Читает материализованный список сессий по уже проверенным фильтрам и границе страницы.
		return r.Runtime.Sessions(ctx, q)
	}, func(value Session) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// GetOutput читает сохраняемый снимок и намерение выхода; текущее состояние соединения в результат не
// входит.
func (s *Service) GetOutput(ctx context.Context, id ID) (SessionOutput, error) {
	var result SessionOutput
	// err содержит итог согласованного чтения сохраняемого выхода.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением SessionOutput по ID и возвращает ошибку
		// репозитория.
		// err передаёт результат чтения сохраняемого выхода из репозитория.
		var err error
		result, err = r.Runtime.Output(ctx, id)
		return err
	})
	return result, err
}

// ListOutputs возвращает страницу сохраняемых выходов; фильтр State отклоняется, поскольку наблюдаемое
// состояние хранится отдельно в RAM.
func (s *Service) ListOutputs(ctx context.Context, request PageRequest) (Page[SessionOutput], error) {
	if request.Query.State != "" {
		return Page[SessionOutput]{}, ErrInvalid
	}
	return readPage(ctx, s, "outputs", request, func(r Repositories, q ListQuery) ([]SessionOutput, error) {
		// Читает материализованный список сохранённых выходов по уже проверенным фильтрам и границе страницы.
		return r.Runtime.Outputs(ctx, q)
	}, func(value SessionOutput) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// GetReceipt читает квитанцию команды по её внутреннему ID.
func (s *Service) GetReceipt(ctx context.Context, id ID) (CommandReceipt, error) {
	var result CommandReceipt
	// err содержит итог согласованного чтения квитанции по внутреннему ID.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением CommandReceipt по ID и возвращает ошибку
		// репозитория.
		// err передаёт результат чтения квитанции по внутреннему ID из репозитория.
		var err error
		result, err = r.Runtime.ReceiptByID(ctx, id)
		return err
	})
	return result, err
}

// ListReceipts возвращает страницу сохраняемых квитанций команд с проверкой фильтров и курсора.
func (s *Service) ListReceipts(ctx context.Context, request PageRequest) (Page[CommandReceipt], error) {
	return readPage(ctx, s, "receipts", request, func(r Repositories, q ListQuery) ([]CommandReceipt, error) {
		// Читает материализованный список квитанций команд по уже проверенным фильтрам и границе страницы.
		return r.Runtime.Receipts(ctx, q)
	}, func(value CommandReceipt) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// GetCleanup читает сохраняемое намерение очистки по ID.
func (s *Service) GetCleanup(ctx context.Context, id ID) (CleanupIntent, error) {
	var result CleanupIntent
	// err содержит итог согласованного чтения намерения очистки.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением CleanupIntent по ID и возвращает ошибку
		// репозитория.
		// err передаёт результат чтения намерения очистки из репозитория.
		var err error
		result, err = r.Artifacts.Cleanup(ctx, id)
		return err
	})
	return result, err
}

// ListCleanups возвращает страницу намерений очистки с проверкой фильтров и курсора.
func (s *Service) ListCleanups(ctx context.Context, request PageRequest) (Page[CleanupIntent], error) {
	return readPage(ctx, s, "cleanups", request, func(r Repositories, q ListQuery) ([]CleanupIntent, error) {
		// Читает материализованный список намерений очистки по уже проверенным фильтрам и границе страницы.
		return r.Artifacts.Cleanups(ctx, q)
	}, func(value CleanupIntent) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// ListEvents возвращает страницу долговечных событий протокола управления, без оперативной телеметрии.
func (s *Service) ListEvents(ctx context.Context, request PageRequest) (Page[Event], error) {
	return readPage(ctx, s, "events", request, func(r Repositories, q ListQuery) ([]Event, error) {
		// Читает материализованный список служебных событий по уже проверенным фильтрам и границе страницы.
		return r.Runtime.Events(ctx, q)
	}, func(value Event) ListBoundary {
		// Возвращает пару CreatedAt и ID последней выданной записи для построения исключающего курсора.
		return ListBoundary{value.CreatedAt, value.ID}
	})
}

// FindReceipt находит сохраняемую квитанцию по исходному RequestID.
func (s *Service) FindReceipt(ctx context.Context, requestID string) (CommandReceipt, error) {
	var result CommandReceipt
	// err содержит итог согласованного чтения квитанции по RequestID.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result значением CommandReceipt по RequestID и возвращает ошибку
		// репозитория.
		// err передаёт результат чтения квитанции по RequestID из репозитория.
		var err error
		result, err = r.Runtime.Receipt(ctx, requestID)
		return err
	})
	return result, err
}
