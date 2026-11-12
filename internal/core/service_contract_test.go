package core_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// configContractRepository подставляет текущую конфигурацию и контролируемые отказы для проверки
// доменного CRUD.
type configContractRepository struct {
	// ConfigRepository сохраняет неиспользуемую часть контракта конфигурационного репозитория.
	core.ConfigRepository
	// stream хранит текущий тестовый поток.
	stream core.Stream
	// profile хранит текущий тестовый именованный профиль.
	profile core.TranscodeProfile
	// group хранит текущую тестовую группу.
	group core.TranscodeGroup
	// destination хранит текущие тестовые настройки назначения.
	destination core.Destination
	// readErr задаёт возвращаемый отказ чтения конфигурации.
	readErr error
	// writeErr задаёт возвращаемый отказ сохранения конфигурации.
	writeErr error
	// reads считает обращения к чтению конфигурации.
	reads int
	// writes считает попытки сохранения конфигурации.
	writes int
	// expected запоминает ожидаемую ревизию последнего переданного обновления.
	expected uint64
}

// Возвращает заданный тестовый Stream или ошибку чтения и учитывает обращение.
func (r *configContractRepository) Stream(context.Context, core.ID) (core.Stream, error) {
	r.reads++
	return r.stream, r.readErr
}

// Возвращает заданный тестовый профиль или ошибку чтения и учитывает обращение.
func (r *configContractRepository) Profile(context.Context, core.ID) (core.TranscodeProfile, error) {
	r.reads++
	return r.profile, r.readErr
}

// Возвращает заданную тестовую группу или ошибку чтения и учитывает обращение.
func (r *configContractRepository) Group(context.Context, core.ID) (core.TranscodeGroup, error) {
	r.reads++
	return r.group, r.readErr
}

// Возвращает заданное тестовое назначение или ошибку чтения и учитывает обращение.
func (r *configContractRepository) Destination(context.Context, core.ID) (core.Destination, error) {
	r.reads++
	return r.destination, r.readErr
}

// Учитывает попытку записи Stream и сохраняет значение только без заданной ошибки записи.
func (r *configContractRepository) InsertStream(_ context.Context, v core.Stream) error {
	r.writes++
	if r.writeErr == nil {
		r.stream = v
	}
	return r.writeErr
}

// Учитывает попытку записи профиля и сохраняет значение только без заданной ошибки записи.
func (r *configContractRepository) InsertProfile(_ context.Context, v core.TranscodeProfile) error {
	r.writes++
	if r.writeErr == nil {
		r.profile = v
	}
	return r.writeErr
}

// Учитывает попытку записи группы и сохраняет значение только без заданной ошибки записи.
func (r *configContractRepository) InsertGroup(_ context.Context, v core.TranscodeGroup) error {
	r.writes++
	if r.writeErr == nil {
		r.group = v
	}
	return r.writeErr
}

// Учитывает попытку записи назначения и сохраняет значение только без заданной ошибки записи.
func (r *configContractRepository) InsertDestination(_ context.Context, v core.Destination) error {
	r.writes++
	if r.writeErr == nil {
		r.destination = v
	}
	return r.writeErr
}

// Запоминает ожидаемую ревизию Stream и передаёт значение тестовой операции записи.
func (r *configContractRepository) UpdateStream(ctx context.Context, v core.Stream, expected uint64) error {
	r.expected = expected
	return r.InsertStream(ctx, v)
}

// Запоминает ожидаемую ревизию профиля и передаёт значение тестовой операции записи.
func (r *configContractRepository) UpdateProfile(ctx context.Context, v core.TranscodeProfile, expected uint64) error {
	r.expected = expected
	return r.InsertProfile(ctx, v)
}

// Запоминает ожидаемую ревизию группы и передаёт значение тестовой операции записи.
func (r *configContractRepository) UpdateGroup(ctx context.Context, v core.TranscodeGroup, expected uint64) error {
	r.expected = expected
	return r.InsertGroup(ctx, v)
}

// Запоминает ожидаемую ревизию назначения и передаёт значение тестовой операции записи.
func (r *configContractRepository) UpdateDestination(ctx context.Context, v core.Destination, expected uint64) error {
	r.expected = expected
	return r.InsertDestination(ctx, v)
}

// configContractUOW передаёт тестовый конфигурационный репозиторий только записывающим операциям.
type configContractUOW struct {
	// repository хранит подменяемую конфигурацию и счётчики обращений.
	repository configContractRepository
	// writes считает попытки сохранения конфигурации.
	writes int
	// err задаёт контролируемый отказ UnitOfWork до вызова репозитория.
	err error
}

// Отклоняет неожиданное обращение к читающей транзакции в тестах изменения конфигурации.
func (u *configContractUOW) View(context.Context, func(core.Repositories) error) error {
	return errors.New("неожиданное чтение вне транзакции конфигурации")
}

// Учитывает транзакцию и вызывает тестовую операцию с ConfigRepository, если контекст не отменён и
// ошибка заранее не задана.
func (u *configContractUOW) Write(ctx context.Context, fn func(core.Repositories) error) error {
	u.writes++
	// err содержит отмену контекста до изменения конфигурации.
	if err := ctx.Err(); err != nil {
		return err
	}
	if u.err != nil {
		return u.err
	}
	return fn(core.Repositories{Config: &u.repository})
}

// Проверяет отказ создания Service без UnitOfWork либо с пустой или некорректной incarnation.
func TestServiceRejectsMissingStorageAndInvalidOwner(t *testing.T) {
	// err сообщает об ошибке создания доменного сервиса.
	if _, err := core.NewService(nil, "owner"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("Service без хранилища создан: %v", err)
	}
	// owner задаёт очередную недопустимую идентичность владельца Service.
	for _, owner := range []string{"", " \t", "owner\x00", string([]byte{0xff})} {
		// err сообщает об ошибке создания доменного сервиса.
		if _, err := core.NewService(&configContractUOW{}, owner); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("некорректный владелец %q принят: %v", owner, err)
		}
	}
}

// Проверяет, что недопустимая конфигурация отклоняется до транзакции и любых обращений к репозиторию.
func TestConfigurationValidationPrecedesPersistence(t *testing.T) {
	// ctx задаёт контекст проверяемых конфигурационных операций.
	ctx := context.Background()
	// u предоставляет контролируемый UnitOfWork конфигурационных операций.
	u := &configContractUOW{}
	// s предоставляет доменные операции текущей конфигурации. err сообщает об ошибке создания
	// доменного сервиса.
	s, err := core.NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	// operation выбирает операцию создания или изменения некорректной конфигурации.
	for _, operation := range []struct {
		// name задаёт имя проверяемой операции создания или изменения конфигурации.
		name string
		// run вызывает выбранную операцию с недопустимой конфигурацией и возвращает её ошибку.
		run func() error
	}{
		{"create_stream", func() error {
			// Возвращает ошибку создания пустой Stream для проверки валидации до хранилища.
			// err сообщает об ошибке создания конфигурации потока.
			_, err := s.CreateStream(ctx, core.Stream{})
			return err
		}},
		{"update_stream", func() error {
			// Возвращает ошибку обновления пустой Stream для проверки валидации до хранилища.
			// err сообщает об ошибке условного изменения конфигурации потока.
			_, err := s.UpdateStream(ctx, core.Stream{}, 1)
			return err
		}},
		{"create_profile", func() error {
			// Возвращает ошибку создания пустого TranscodeProfile для проверки валидации до хранилища.
			// err сообщает об ошибке создания именованного профиля.
			_, err := s.CreateProfile(ctx, core.TranscodeProfile{})
			return err
		}},
		{"update_profile", func() error {
			// Возвращает ошибку обновления пустого TranscodeProfile для проверки валидации до хранилища.
			// err сообщает об ошибке условного изменения именованного профиля.
			_, err := s.UpdateProfile(ctx, core.TranscodeProfile{}, 1)
			return err
		}},
		{"create_group", func() error {
			// Возвращает ошибку создания пустой TranscodeGroup для проверки валидации до хранилища.
			// err сообщает об ошибке создания конфигурационной группы.
			_, err := s.CreateGroup(ctx, core.TranscodeGroup{})
			return err
		}},
		{"update_group", func() error {
			// Возвращает ошибку обновления пустой TranscodeGroup для проверки валидации до хранилища.
			// err сообщает об ошибке условного изменения группы.
			_, err := s.UpdateGroup(ctx, core.TranscodeGroup{}, 1)
			return err
		}},
		{"create_destination", func() error {
			// Возвращает ошибку создания пустой Destination для проверки валидации до хранилища.
			// err сообщает об ошибке создания назначения.
			_, err := s.CreateDestination(ctx, core.Destination{})
			return err
		}},
		{"update_destination", func() error {
			// Возвращает ошибку обновления пустой Destination для проверки валидации до хранилища.
			// err сообщает об ошибке условного изменения назначения.
			_, err := s.UpdateDestination(ctx, core.Destination{}, 1)
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			// Проверяет ErrInvalid выбранной операции без обращений к UnitOfWork и репозиторию.

			// err содержит результат выбранной операции с недопустимой конфигурацией.
			if err := operation.run(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("некорректная конфигурация принята: %v", err)
			}
			if u.writes != 0 || u.repository.reads != 0 || u.repository.writes != 0 {
				t.Fatal("невалидная конфигурация дошла до хранилища")
			}
		})
	}
}

// Проверяет сохранение ID и времени создания при обновлении, следующую ревизию и неизменность данных
// при конфликте, отмене и ошибках чтения или записи.
func TestConfigurationUpdatesPreserveIdentityAndCheckRevision(t *testing.T) {
	// ctx задаёт контекст проверяемых конфигурационных операций.
	ctx := context.Background()
	// kind выбирает поток, профиль, группу или назначение для одинаковой проверки обновления.
	for _, kind := range []string{"stream", "profile", "group", "destination"} {
		t.Run(kind, func(t *testing.T) {
			// Для выбранной сущности проверяет успешное обновление и отказы CAS, чтения, записи, переполнения
			// Revision и отмены контекста.

			// outcome выбирает успех, конфликт, отказ хранилища, переполнение или отмену
			// обновления.
			for _, outcome := range []string{"success", "revision_conflict", "read_failure", "write_failure", "revision_overflow", "cancelled"} {
				t.Run(outcome, func(t *testing.T) {
					// Проверяет выбранный исход обновления: при успехе сохраняются ID и CreatedAt и растёт Revision, при
					// отказе конфигурация остаётся прежней.

					// snapshot предоставляет исходные настройки потока для
					// конфигурационной проверки. output предоставляет согласованные
					// исходные профиль, группу и назначение.
					snapshot, output := contractSnapshots(t)
					// u предоставляет контролируемый UnitOfWork конфигурационных
					// операций.
					u := &configContractUOW{repository: configContractRepository{stream: snapshot.Stream, profile: output.Profile, group: output.Group, destination: output.Destination}}
					// s предоставляет доменные операции текущей конфигурации. err
					// сообщает об ошибке создания доменного сервиса.
					s, err := core.NewService(u, "owner")
					if err != nil {
						t.Fatal(err)
					}
					// wantErr задаёт ожидаемый результат выбранного сценария
					// обновления.
					wantErr := error(nil)
					// expected задаёт ожидаемую ревизию текущей попытки обновления.
					expected := uint64(1)
					// callCtx задаёт контекст конкретной попытки обновления, включая
					// отменённый сценарий.
					callCtx := ctx
					switch outcome {
					case "revision_conflict":
						expected, wantErr = 2, core.ErrConflict
					case "read_failure":
						u.repository.readErr, wantErr = core.ErrNotFound, core.ErrNotFound
					case "write_failure":
						u.repository.writeErr, wantErr = core.ErrBusy, core.ErrBusy
					case "revision_overflow":
						expected, wantErr = math.MaxUint64, core.ErrConflict
						u.repository.stream.Revision, u.repository.profile.Revision = expected, expected
						u.repository.group.Revision, u.repository.destination.Revision = expected, expected
					case "cancelled":
						// cancel отменяет контекст перед условным обновлением
						// конфигурации.
						var cancel context.CancelFunc
						callCtx, cancel = context.WithCancel(ctx)
						cancel()
						wantErr = context.Canceled
					}
					// before сохраняет исходные значения тестового репозитория для
					// проверки отсутствия частичной записи.
					before := u.repository
					// actualRecord принимает общую запись возвращённой обновлённой
					// сущности.
					var actualRecord core.Record
					switch kind {
					case "stream":
						// value содержит отдельный кандидат изменения выбранной
						// конфигурации.
						value := before.stream
						value.Name = "Новое имя"
						value.Revision = 99
						value.CreatedAt = value.CreatedAt.Add(-1)
						// actual содержит возвращённую конфигурацию после попытки
						// условного обновления. err сообщает об ошибке условного
						// изменения конфигурации потока.
						actual, err := s.UpdateStream(callCtx, value, expected)
						actualRecord = actual.Record
						if !errors.Is(err, wantErr) {
							t.Fatalf("результат UpdateStream: %v, ожидался %v", err, wantErr)
						}
					case "profile":
						// value содержит отдельный кандидат изменения выбранной
						// конфигурации.
						value := before.profile
						value.Name = "Новое имя"
						value.Profile.AudioEncoding.BitrateBPS++
						// actual содержит возвращённую конфигурацию после попытки
						// условного обновления. err сообщает об ошибке условного
						// изменения именованного профиля.
						actual, err := s.UpdateProfile(callCtx, value, expected)
						actualRecord = actual.Record
						if !errors.Is(err, wantErr) {
							t.Fatalf("результат UpdateProfile: %v, ожидался %v", err, wantErr)
						}
					case "group":
						// value содержит отдельный кандидат изменения выбранной
						// конфигурации.
						value := before.group
						value.Name = "Новое имя"
						// actual содержит возвращённую конфигурацию после попытки
						// условного обновления. err сообщает об ошибке условного
						// изменения группы.
						actual, err := s.UpdateGroup(callCtx, value, expected)
						actualRecord = actual.Record
						if !errors.Is(err, wantErr) {
							t.Fatalf("результат UpdateGroup: %v, ожидался %v", err, wantErr)
						}
					case "destination":
						// value содержит отдельный кандидат изменения выбранной
						// конфигурации.
						value := before.destination
						value.Name = "Новое имя"
						value.Endpoint = "rtmp://other.invalid/live"
						// actual содержит возвращённую конфигурацию после попытки
						// условного обновления. err сообщает об ошибке условного
						// изменения назначения.
						actual, err := s.UpdateDestination(callCtx, value, expected)
						actualRecord = actual.Record
						if !errors.Is(err, wantErr) {
							t.Fatalf("результат UpdateDestination: %v, ожидался %v", err, wantErr)
						}
					}
					if wantErr != nil {
						if !reflect.DeepEqual(before.stream, u.repository.stream) || !reflect.DeepEqual(before.profile, u.repository.profile) || before.group != u.repository.group || before.destination != u.repository.destination {
							t.Fatal("неуспешное обновление изменило конфигурацию")
						}
						if outcome != "write_failure" && u.repository.writes != 0 {
							t.Fatal("отклонённое обновление передано на запись")
						}
						return
					}
					// records сопоставляет видам конфигурации исходные Record для
					// проверки идентичности.
					records := map[string]core.Record{"stream": before.stream.Record, "profile": before.profile.Record, "group": before.group.Record, "destination": before.destination.Record}
					// original содержит исходную Record выбранного вида
					// конфигурации.
					original := records[kind]
					if actualRecord.ID != original.ID || !actualRecord.CreatedAt.Equal(original.CreatedAt) || actualRecord.Revision != 2 || actualRecord.UpdatedAt.Before(original.UpdatedAt) || actualRecord.Validate() != nil || u.repository.expected != expected || u.repository.writes != 1 {
						t.Fatalf("обновление нарушило идентичность или версию: %+v", actualRecord)
					}
					// names сопоставляет видам конфигурации фактически сохранённые
					// имена.
					names := map[string]string{"stream": u.repository.stream.Name, "profile": u.repository.profile.Name, "group": u.repository.group.Name, "destination": u.repository.destination.Name}
					if names[kind] != "Новое имя" {
						t.Fatal("успешное обновление не сохранило значение")
					}
					if kind == "profile" && u.repository.profile.Profile.AudioEncoding.BitrateBPS != before.profile.Profile.AudioEncoding.BitrateBPS+1 {
						t.Fatal("обновление профиля потеряло изменение параметров кодирования")
					}
					if kind == "destination" && u.repository.destination.Endpoint != "rtmp://other.invalid/live" {
						t.Fatal("обновление назначения потеряло новый endpoint")
					}
				})
			}
		})
	}
}

// Проверяет отказ смены постоянного ключа Stream без передачи изменения на запись.
func TestStreamPublicationKeyCannotChangeDuringConfigurationUpdate(t *testing.T) {
	// snapshot предоставляет исходные настройки потока для конфигурационной проверки.
	snapshot, _ := contractSnapshots(t)
	// u предоставляет контролируемый UnitOfWork конфигурационных операций.
	u := &configContractUOW{repository: configContractRepository{stream: snapshot.Stream}}
	// s предоставляет доменные операции текущей конфигурации. err сообщает об ошибке создания
	// доменного сервиса.
	s, err := core.NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	// candidate содержит копию потока с подменённым постоянным ключом публикации.
	candidate := snapshot.Stream
	candidate.PublicationKey = "different-synthetic-key"
	// err сообщает об ошибке условного изменения конфигурации потока.
	if _, err := s.UpdateStream(context.Background(), candidate, snapshot.Stream.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("постоянный ключ заменён: %v", err)
	}
	if u.repository.writes != 0 || u.repository.stream != snapshot.Stream {
		t.Fatal("отказ смены ключа изменил конфигурацию")
	}
}

// Проверяет назначение copy только новому профилю, приоритет CAS и запрет удаления явного селектора
// при полной замене.
func TestServiceLiveAudioDefaultAndReplacementCAS(t *testing.T) {
	// ctx задаёт контекст проверяемых конфигурационных операций.
	ctx := context.Background()
	// encoding выбирает полный профиль v2 или прежний профиль без Encoding.
	for _, encoding := range []bool{false, true} {
		// p содержит исходный профиль без явного LIVE-аудиорежима.
		p := encodedProfile(core.VideoEncoderNVENC, core.RateControlVBR)
		if !encoding {
			p.Encoding = core.EncodingSettings{}
		}
		// u предоставляет контролируемый UnitOfWork конфигурационных операций.
		u := &configContractUOW{}
		// s предоставляет доменные операции текущей конфигурации. err сообщает об ошибке
		// создания доменного сервиса.
		s, err := core.NewService(u, "owner")
		if err != nil {
			t.Fatal(err)
		}
		// created содержит новый именованный профиль с назначенным LIVE-аудиорежимом copy.
		created, err := s.CreateProfile(ctx, core.TranscodeProfile{Name: "Новый", Profile: p})
		if err != nil || created.Profile.LiveAudioMode != core.LiveAudioCopy || u.repository.profile != created || p.LiveAudioMode != "" {
			t.Fatal("Create не материализовал LIVE copy либо изменил вход", err)
		}
		// omitted содержит копию созданного профиля с удалённым явным LIVE-аудиоселектором.
		omitted := created
		omitted.Profile.LiveAudioMode = ""
		// expected задаёт актуальную или устаревшую ревизию замены LIVE-аудиоселектора.
		for _, expected := range []uint64{created.Revision + 1, created.Revision} {
			// writes сохраняет исходное число записей для проверки отсутствия изменения при
			// отказе.
			writes := u.repository.writes
			// err сообщает об ошибке условного изменения именованного профиля.
			_, err := s.UpdateProfile(ctx, omitted, expected)
			if expected != created.Revision {
				if !errors.Is(err, core.ErrConflict) {
					t.Fatal("CAS должен предшествовать проверке удаления селектора", err)
				}
			} else {
				// typed принимает типизированный отказ удаления явного LIVE-аудиорежима.
				var typed *core.ProfileError
				if !errors.As(err, &typed) || typed.Field() != "profile.live_audio_mode" {
					t.Fatal("Удаление селектора принято", err)
				}
			}
			if u.repository.writes != writes || u.repository.profile != created {
				t.Fatal("Отклонённая замена изменила профиль")
			}
		}
		created.Profile.LiveAudioMode = core.LiveAudioTranscode
		// err сообщает об ошибке условного изменения именованного профиля.
		if _, err := s.UpdateProfile(ctx, created, created.Revision); err != nil {
			t.Fatal("Явный transcode отклонён", err)
		}
	}
}
