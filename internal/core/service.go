package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Service выполняет операции с долговечными данными через UnitOfWork, а допуски и оперативное состояние
// согласует через общую RuntimeAuthority. Incarnation принадлежит владельцу хранилища и меняется при
// каждом его открытии.
type Service struct {
	// uow предоставляет транзакционные репозитории доменных операций.
	uow UnitOfWork
	// incarnation различает открытия хранилища и связывает доменные попытки с его текущим
	// владельцем.
	incarnation string
}

// NewService связывает доменные операции с UnitOfWork и владельцем incarnation; отсутствие зависимости
// и некорректный идентификатор отклоняются.
func NewService(uow UnitOfWork, incarnation string) (*Service, error) {
	if uow == nil {
		return nil, fmt.Errorf("%w: отсутствует UnitOfWork", ErrInvalid)
	}
	// err содержит причину отказа проверки идентификатора владельца открытия хранилища.
	if err := validText("incarnation", incarnation); err != nil {
		return nil, err
	}
	return &Service{uow: uow, incarnation: incarnation}, nil
}

// newID создаёт 128-битный случайный идентификатор в шестнадцатеричном виде и возвращает ошибку
// источника случайности.
func newID() (ID, error) {
	// b принимает 128 случайных бит для нового доменного идентификатора.
	var b [16]byte
	// err содержит причину отказа источника случайности при создании идентификатора.
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("создание идентификатора: %w", err)
	}
	return ID(hex.EncodeToString(b[:])), nil
}

// newRecord создаёт запись с новой идентичностью, ревизией 1 и одинаковыми временами создания и
// изменения в UTC с точностью до микросекунд.
func newRecord() (Record, error) {
	// id содержит новый идентификатор записи, а err — причину отказа его создания.
	id, err := newID()
	if err != nil {
		return Record{}, err
	}
	// now содержит общее начальное время создания и изменения записи в UTC с микросекундной
	// точностью.
	now := time.Now().UTC().Truncate(time.Microsecond)
	return Record{ID: id, Revision: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// revise увеличивает ревизию копии записи и обновляет время изменения; переполнение ревизии
// отклоняется.
func revise(r Record) (Record, error) {
	// next содержит следующую ревизию записи, а err — признак исчерпания счётчика ревизий.
	next, err := NextRevision(r.Revision)
	if err != nil {
		return Record{}, err
	}
	r.Revision = next
	r.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	return r, nil
}

// checkRevision сравнивает сохранённую actual с ожидаемой expected и возвращает ErrConflict
// с причиной конфликта ревизии. known содержит только публичные ID из уже прочитанной записи;
// сами значения ревизий в причину не включаются. При совпадении возвращается nil.
func checkRevision(actual, expected uint64, known FailureContext) error {
	if actual != expected {
		return WithFailureReason(ErrConflict, FailureReason{Code: FailureRevisionConflict, Field: FailureFieldRevision, Context: known})
	}
	return nil
}

// CreateStream проверяет конфигурацию, назначает новую запись и сохраняет Stream в транзакции записи.
func (s *Service) CreateStream(ctx context.Context, value Stream) (Stream, error) {
	// err содержит причину отказа проверки создаваемой конфигурации потока.
	if err := value.Validate(); err != nil {
		return Stream{}, err
	}
	// record содержит новую идентичность и временные метки потока, а err — причину отказа создания
	// записи.
	record, err := newRecord()
	if err != nil {
		return Stream{}, err
	}
	value.Record = record
	err = s.uow.Write(ctx, func(r Repositories) error {
		// Вставляет новую Stream с уже назначенной Record и возвращает ошибку репозитория в UnitOfWork.
		return r.Config.InsertStream(ctx, value)
	})
	return value, err
}

// UpdateStream заменяет конфигурацию при ожидаемой ревизии, сохраняя ключ публикации и текущее
// оформление чата. Успешная замена увеличивает ревизию записи.
// s предоставляет транзакции, ctx ограничивает запись, value задаёт новую конфигурацию,
// expected — ожидаемую ревизию. Конфликт возвращает ID прочитанного потока без изменения данных.
func (s *Service) UpdateStream(ctx context.Context, value Stream, expected uint64) (Stream, error) {
	// err содержит причину отказа проверки заменяющей конфигурации потока.
	if err := value.Validate(); err != nil {
		return Stream{}, err
	}
	// err содержит причину отказа транзакционного изменения потока; r предоставляет репозитории
	// функции, которая сверяет и сохраняет конфигурацию в той же транзакции.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет ожидаемую Revision и неизменность PublicationKey, затем переносит
		// текущее оформление, назначает следующую Record и сохраняет Stream.

		// current содержит сохранённую конфигурацию потока для проверки ревизии, а err — причину
		// отказа её чтения.
		current, err := r.Config.Stream(ctx, value.ID)
		if err != nil {
			return err
		}
		if err = checkRevision(current.Revision, expected, FailureContext{StreamID: current.ID}); err != nil {
			return err
		}
		if value.PublicationKey != current.PublicationKey {
			return fmt.Errorf("%w: ключ публикации неизменяем", ErrConflict)
		}
		value.ChatAppearance = current.ChatAppearance
		value.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		return r.Config.UpdateStream(ctx, value, expected)
	})
	return value, err
}

// UpdateChatAppearance заменяет только оформление чата потока с идентификатором id и увеличивает
// его ревизию. Проверка expected и запись выполняются в одной транзакции ctx. При отсутствии потока
// или несовпадении ревизии изменение отклоняется.
func (s *Service) UpdateChatAppearance(ctx context.Context, id ID, appearance ChatAppearance, expected uint64) (Stream, error) {
	// err сообщает о недопустимом идентификаторе потока до чтения записи.
	if err := id.Validate(); err != nil {
		return Stream{}, err
	}
	if expected == 0 {
		return Stream{}, fmt.Errorf("%w: ожидалась положительная ревизия потока", ErrInvalid)
	}
	// err сообщает о недопустимом оформлении до открытия транзакции.
	if err := appearance.Validate(); err != nil {
		return Stream{}, err
	}
	// updated принимает конфигурацию с новой ревизией после условной записи.
	var updated Stream
	// err содержит итог транзакции, включая возможный отказ фиксации.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// Читает через r текущий поток, сверяет его ревизию и сохраняет оформление вместе с новой ревизией.
		// current содержит конфигурацию потока, а err — причину отказа чтения или записи.
		current, err := r.Config.Stream(ctx, id)
		if err != nil {
			return err
		}
		if err = checkRevision(current.Revision, expected, FailureContext{StreamID: current.ID}); err != nil {
			return err
		}
		current.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		current.ChatAppearance = &appearance
		if err = r.Config.UpdateStream(ctx, current, expected); err != nil {
			return err
		}
		updated = current
		return nil
	})
	if err != nil {
		return Stream{}, err
	}
	return updated, nil
}

// DeleteStream удаляет Stream с ожидаемой ревизией. GuardWrite отзывает допуск тестовой ленты
// этого потока после подтверждения удаления.
func (s *Service) DeleteStream(ctx context.Context, id ID, expected uint64) error {
	// target адресует допуск удаляемого потока после успешной операции репозитория.
	var target chatTestWriteTarget
	return s.uow.Write(context.WithValue(ctx, chatTestWriteKey{}, &target), func(r Repositories) error {
		// Удаляет Stream с проверкой ревизии и передаёт его ID в GuardWrite только для текущей
		// успешной попытки удаления.
		target = chatTestWriteTarget{}
		// err сообщает об отказе удаления, например при конфликте ревизии или наличии связей.
		if err := r.Config.DeleteStream(ctx, id, expected); err != nil {
			return err
		}
		target.streamID = id
		return nil
	})
}

// CreateProfile задаёт copy для незаданного режима LIVE-аудио нового профиля, проверяет профиль и
// сохраняет новую запись.
func (s *Service) CreateProfile(ctx context.Context, value TranscodeProfile) (TranscodeProfile, error) {
	if value.Profile.LiveAudioMode == "" {
		value.Profile.LiveAudioMode = LiveAudioCopy
	}
	// err содержит причину отказа проверки создаваемой конфигурации профиля.
	if err := value.Validate(); err != nil {
		return TranscodeProfile{}, err
	}
	// record содержит новую идентичность и временные метки профиля, а err — причину отказа создания
	// записи.
	record, err := newRecord()
	if err != nil {
		return TranscodeProfile{}, err
	}
	value.Record = record
	err = s.uow.Write(ctx, func(r Repositories) error {
		// Вставляет новый TranscodeProfile с уже назначенной Record и возвращает ошибку репозитория.
		return r.Config.InsertProfile(ctx, value)
	})
	return value, err
}

// UpdateProfile проверяет ожидаемую ревизию и заменяет профиль без назначения значений по умолчанию;
// полный материализованный профиль требует явного режима LIVE-аудио.
// s предоставляет транзакции, ctx ограничивает операцию хранилища; value содержит заменяемую
// конфигурацию профиля, expected — её ожидаемую ревизию. Конфликт возвращает ID прочитанного профиля.
func (s *Service) UpdateProfile(ctx context.Context, value TranscodeProfile, expected uint64) (TranscodeProfile, error) {
	// err сообщает об ошибочном имени до обращения к хранилищу.
	if err := validText("TranscodeProfile.Name", value.Name); err != nil {
		return TranscodeProfile{}, err
	}
	// Пустой LIVE-селектор проверяется после CAS, чтобы устаревшая полная замена
	// сначала получала конфликт ревизии. Другие явные профили проверяются до транзакции.
	if value.Profile.LiveAudioMode != "" {
		// err сообщает нарушение полной явной конфигурации.
		if err := value.Profile.Validate(); err != nil {
			return TranscodeProfile{}, err
		}
	}
	// err содержит причину отказа транзакционного изменения профиля.
	// r предоставляет репозитории текущей транзакции условной замены профиля.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет Revision и сохраняет следующую ревизию полного профиля;
		// переданная функция не подставляет отсутствующие параметры запроса.

		// current содержит сохранённую конфигурацию профиля для проверки ревизии, а err —
		// причину отказа её чтения.
		current, err := r.Config.Profile(ctx, value.ID)
		if err != nil {
			return err
		}
		if err = checkRevision(current.Revision, expected, FailureContext{ProfileID: current.ID}); err != nil {
			return err
		}
		if value.Profile.LiveAudioMode == "" {
			return invalidEncoding("live_audio_mode", "Полная замена профиля требует явного live_audio_mode: copy или transcode.")
		}
		if err = value.Validate(); err != nil {
			return err
		}
		value.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		return r.Config.UpdateProfile(ctx, value, expected)
	})
	return value, err
}

// DeleteProfile удаляет профиль через репозиторий с проверкой ожидаемой ревизии в транзакции записи.
func (s *Service) DeleteProfile(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// Удаляет TranscodeProfile через репозиторий с проверкой ожидаемой Revision в UnitOfWork.
		return r.Config.DeleteProfile(ctx, id, expected)
	})
}

// CreateGroup проверяет группу, назначает новую запись и сохраняет её связи через репозиторий.
func (s *Service) CreateGroup(ctx context.Context, value TranscodeGroup) (TranscodeGroup, error) {
	// err содержит причину отказа проверки создаваемой конфигурации группы.
	if err := value.Validate(); err != nil {
		return TranscodeGroup{}, err
	}
	// record содержит новую идентичность и временные метки группы, а err — причину отказа создания
	// записи.
	record, err := newRecord()
	if err != nil {
		return TranscodeGroup{}, err
	}
	value.Record = record
	err = s.uow.Write(ctx, func(r Repositories) error {
		// Вставляет новую TranscodeGroup с уже назначенной Record и возвращает ошибку репозитория.
		return r.Config.InsertGroup(ctx, value)
	})
	return value, err
}

// UpdateGroup заменяет проверенную конфигурацию группы при ожидаемой ревизии и увеличивает ревизию
// записи.
// s предоставляет транзакции, ctx ограничивает запись, value задаёт новую конфигурацию,
// expected — ожидаемую ревизию. Контекст конфликта берётся из прочитанной группы и её связей.
func (s *Service) UpdateGroup(ctx context.Context, value TranscodeGroup, expected uint64) (TranscodeGroup, error) {
	// err содержит причину отказа проверки заменяющей конфигурации группы.
	if err := value.Validate(); err != nil {
		return TranscodeGroup{}, err
	}
	// err содержит причину отказа транзакционного изменения группы; r предоставляет репозитории
	// функции, которая сверяет и сохраняет конфигурацию в той же транзакции.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет Revision группы, назначает следующую Record и сохраняет TranscodeGroup.

		// current содержит сохранённую конфигурацию группы для проверки ревизии, а err — причину
		// отказа её чтения.
		current, err := r.Config.Group(ctx, value.ID)
		if err != nil {
			return err
		}
		if err = checkRevision(current.Revision, expected, FailureContext{GroupID: current.ID, StreamID: current.StreamID, ProfileID: current.ProfileID}); err != nil {
			return err
		}
		value.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		return r.Config.UpdateGroup(ctx, value, expected)
	})
	return value, err
}

// DeleteGroup удаляет группу через репозиторий с ожидаемой ревизией в транзакции записи.
func (s *Service) DeleteGroup(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// Удаляет TranscodeGroup через репозиторий с проверкой ожидаемой Revision в UnitOfWork.
		return r.Config.DeleteGroup(ctx, id, expected)
	})
}

// CreateDestination проверяет настройки назначения, назначает новую запись и сохраняет её в транзакции
// записи.
func (s *Service) CreateDestination(ctx context.Context, value Destination) (Destination, error) {
	// err содержит причину отказа проверки создаваемой конфигурации назначения.
	if err := value.Validate(); err != nil {
		return Destination{}, err
	}
	// record содержит новую идентичность и временные метки назначения, а err — причину отказа
	// создания записи.
	record, err := newRecord()
	if err != nil {
		return Destination{}, err
	}
	value.Record = record
	err = s.uow.Write(ctx, func(r Repositories) error {
		// Вставляет новую Destination с уже назначенной Record и возвращает ошибку репозитория.
		return r.Config.InsertDestination(ctx, value)
	})
	return value, err
}

// UpdateDestination заменяет проверенные настройки назначения при ожидаемой ревизии и увеличивает
// ревизию записи.
// s предоставляет транзакции, ctx ограничивает запись, value задаёт новые настройки,
// expected — ожидаемую ревизию. Контекст конфликта берётся из прочитанного назначения и его связей.
func (s *Service) UpdateDestination(ctx context.Context, value Destination, expected uint64) (Destination, error) {
	// err содержит причину отказа проверки заменяющей конфигурации назначения.
	if err := value.Validate(); err != nil {
		return Destination{}, err
	}
	// err содержит причину отказа транзакционного изменения назначения; r предоставляет репозитории
	// функции, которая сверяет и сохраняет настройки в той же транзакции.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет Revision назначения, назначает следующую Record и сохраняет Destination.

		// current содержит сохранённую конфигурацию назначения для проверки ревизии, а err —
		// причину отказа её чтения.
		current, err := r.Config.Destination(ctx, value.ID)
		if err != nil {
			return err
		}
		if err = checkRevision(current.Revision, expected, FailureContext{DestinationID: current.ID, StreamID: current.StreamID, GroupID: current.GroupID}); err != nil {
			return err
		}
		value.Record, err = revise(current.Record)
		if err != nil {
			return err
		}
		return r.Config.UpdateDestination(ctx, value, expected)
	})
	return value, err
}

// DeleteDestination удаляет назначение через репозиторий с ожидаемой ревизией в транзакции записи.
func (s *Service) DeleteDestination(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// Удаляет Destination через репозиторий с проверкой ожидаемой Revision в UnitOfWork.
		return r.Config.DeleteDestination(ctx, id, expected)
	})
}
