package core

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// obsInspectionUOW предоставляет неизменяемый снимок для проверки первоначального допуска OBS.
type obsInspectionUOW struct {
	// config содержит текущий ключ потока и возможный отказ чтения конфигурации.
	config obsInspectionConfig
	// runtime содержит выбранную Session либо точную ошибку её чтения.
	runtime obsInspectionRuntime
	// views и writes считают обращения к читающей и изменяющей границам.
	views, writes int
}

// View выполняет callback ровно над одним набором проверочных репозиториев.
func (u *obsInspectionUOW) View(_ context.Context, callback func(Repositories) error) error {
	u.views++
	return callback(Repositories{Config: &u.config, Runtime: &u.runtime})
}

// Write отмечает запрещённый для первоначального допуска побочный эффект и возвращает отказ.
func (u *obsInspectionUOW) Write(context.Context, func(Repositories) error) error {
	u.writes++
	return ErrInvalid
}

// obsInspectionConfig предоставляет только требуемое первоначальным допуском чтение Stream.
type obsInspectionConfig struct {
	// ConfigRepository сохраняет полный интерфейс без реализации неиспользуемых операций.
	ConfigRepository
	// stream содержит текущий постоянный ключ проверяемого потока.
	stream Stream
	// err моделирует отказ чтения конфигурации.
	err error
}

// Stream возвращает текущий Stream либо точный отказ чтения его конфигурации.
func (r *obsInspectionConfig) Stream(context.Context, ID) (Stream, error) {
	return r.stream, r.err
}

// obsInspectionRuntime наблюдает обращение к Session после проверки текущего постоянного ключа.
type obsInspectionRuntime struct {
	// RuntimeRepository сохраняет интерфейс операций, не входящих в читающий допуск.
	RuntimeRepository
	// session содержит точный найденный запуск, включая STARTING или STOPPING.
	session Session
	// err задаёт отсутствие Session либо отказ чтения.
	err error
	// calls отмечает, дошло ли чтение до поиска Session.
	calls int
}

// ActiveSession возвращает заданный исход и отмечает факт чтения Session.
func (r *obsInspectionRuntime) ActiveSession(context.Context, ID) (Session, error) {
	r.calls++
	return r.session, r.err
}

// TestOBSInspectionSeparatesAbsenceAuthorizationAndReadFailures проверяет читающий контракт допуска:
// неверный текущий ключ запрещает поиск Session, ошибки не становятся отсутствием, а найденный
// точный запуск возвращается без изменения БД независимо от его активной стадии.
func TestOBSInspectionSeparatesAbsenceAuthorizationAndReadFailures(t *testing.T) {
	// readFailure представляет отказ хранилища, который нельзя скрыть новым автозапуском.
	readFailure := errors.New("чтение временно недоступно")
	// test задаёт текущий ключ, стадию запуска и независимое ожидаемое поведение читающей операции.
	for _, test := range []struct {
		// name обозначает проверяемый контракт допуска.
		name string
		// stored и supplied содержат текущий и предъявленный ключи.
		stored, supplied string
		// state задаёт найденную активную стадию либо пустое значение при отсутствии.
		state SessionState
		// streamErr и sessionErr моделируют независимые от авторизации отказы чтения.
		streamErr, sessionErr error
		// wantErr задаёт ожидаемый точный отказ; found указывает наличие выбранного запуска.
		wantErr error
		found   bool
		// reads указывает допустимое число обращений к ActiveSession.
		reads int
	}{
		{name: "нет запуска", stored: "key", supplied: "key", sessionErr: ErrNotFound, reads: 1},
		{name: "STARTING", stored: "key", supplied: "key", state: SessionStarting, found: true, reads: 1},
		{name: "RUNNING", stored: "key", supplied: "key", state: SessionRunning, found: true, reads: 1},
		{name: "STOPPING", stored: "key", supplied: "key", state: SessionStopping, found: true, reads: 1},
		{name: "неверный ключ", stored: "key", supplied: "wrong", wantErr: ErrNotReady},
		{name: "пустой предъявленный ключ", stored: "key", wantErr: ErrNotReady},
		{name: "пустой текущий ключ", supplied: "key", wantErr: ErrNotReady},
		{name: "отказ Stream", stored: "key", supplied: "key", streamErr: readFailure, wantErr: readFailure},
		{name: "нет Stream", supplied: "key", streamErr: ErrNotFound, wantErr: ErrNotFound},
		{name: "отказ Session", stored: "key", supplied: "key", sessionErr: readFailure, wantErr: readFailure, reads: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Проверяет единственное согласованное чтение, точный результат и отсутствие Write.
			// wanted сохраняет независимую идентичность найденного запуска вместе с его стадией.
			wanted := Session{Record: Record{ID: "selected-session"}, StreamID: "stream", Incarnation: "incarnation", Lifecycle: 7, State: test.state}
			// uow предоставляет согласованные текущую конфигурацию и результат поиска Session.
			uow := &obsInspectionUOW{config: obsInspectionConfig{stream: Stream{PublicationKey: test.stored}, err: test.streamErr}, runtime: obsInspectionRuntime{session: wanted, err: test.sessionErr}}
			// service представляет доменную границу без фоновых исполнителей; err сообщает отказ создания.
			service, err := NewService(uow, "incarnation")
			if err != nil {
				t.Fatal(err)
			}
			// actual сохраняет выбранную Session; found различает отсутствие; err сообщает отказ допуска.
			actual, found, err := service.InspectOBS(context.Background(), "stream", test.supplied)
			if err != test.wantErr || found != test.found || uow.views != 1 || uow.writes != 0 || uow.runtime.calls != test.reads {
				t.Fatal("нарушен читающий контракт", actual, found, err, uow.views, uow.writes, uow.runtime.calls)
			}
			if test.found && !reflect.DeepEqual(actual, wanted) {
				t.Fatal("изменена идентичность выбранной Session", actual, wanted)
			}
		})
	}
}
