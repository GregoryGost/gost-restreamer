package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Создаёт пять Stream с известным порядком ID и совпадающими либо соседними микросекундными временами
// для проверки страниц.
func historyFixture(t *testing.T) (*Store, *core.Service, time.Time) {
	t.Helper()
	// s содержит временную SQLite с пятью упорядоченными потоками.
	s := openTestStore(t)
	// svc предоставляет доменные операции текущего владельца временного Store. err сообщает об
	// ошибке создания доменного Service для текущего владельца Store.
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// base задаёт время первых трёх потоков и начало проверяемого диапазона.
	base := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	err = s.Write(context.Background(), func(r core.Repositories) error {
		// В одной транзакции создаёт пять Streams с совпадающими и соседними CreatedAt для проверки порядка по
		// времени и ID.

		// i указывает позицию потока в эталонной последовательности. id задаёт идентификатор
		// потока для разрешения равных времён.
		for i, id := range []core.ID{"a", "b", "c", "d", "e"} {
			// created задаёт время очередного потока с микросекундным смещением.
			created := base
			if i > 2 {
				created = base.Add(time.Duration(i-2) * time.Microsecond)
			}
			// value содержит очередной поток с известными временем и ID.
			value := core.Stream{ID: id, Revision: 1, CreatedAt: created, UpdatedAt: created, Name: "Поток", PublicationKey: "synthetic-key-" + string(id)}
			// err сообщает об ошибке вставки конфигурации потока.
			if err := r.Config.InsertStream(context.Background(), value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, svc, base
}

// Проверяет устойчивый порядок по времени и ID, привязку курсора к виду и фильтрам и эквивалентность
// временных зон.
func TestCursorUsesStableTieBreakAndExactKindAndFilters(t *testing.T) {
	// svc предоставляет доменные операции текущего владельца временного Store. base задаёт время
	// первых трёх потоков и начало проверяемого диапазона.
	_, svc, base := historyFixture(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// to задаёт исключительную верхнюю временную границу выборки.
	to := base.Add(2 * time.Microsecond)
	// request задаёт страницу потоков с ограниченным диапазоном и проверяемым курсором.
	request := core.PageRequest{Query: core.ListQuery{Limit: 2, From: &base, To: &to}}
	// first содержит первую страницу и курсор продолжения. err сообщает об ошибке чтения страницы
	// потоков.
	first, err := svc.ListStreams(ctx, request)
	if err != nil || len(first.Items) != 2 || first.Items[0].ID != "a" || first.Items[1].ID != "b" || first.NextCursor == "" {
		t.Fatalf("первая страница: %+v, %v", first, err)
	}
	request.Cursor = first.NextCursor
	// second содержит вторую страницу того же диапазона.
	second, err := svc.ListStreams(ctx, request)
	if err != nil || len(second.Items) != 2 || second.Items[0].ID != "c" || second.Items[1].ID != "d" || second.NextCursor != "" {
		t.Fatalf("вторая страница: %+v, %v", second, err)
	}
	_, err = svc.ListProfiles(ctx, request)
	requireDomainError(t, err, core.ErrInvalidCursor)
	// changed содержит запрос с изменёнными фильтрами и прежним курсором.
	changed := request
	changed.Query.To = nil
	_, err = svc.ListStreams(ctx, changed)
	requireDomainError(t, err, core.ErrInvalidCursor)
	// zone задаёт часовой пояс UTC+3 для проверки эквивалентных моментов.
	zone := time.FixedZone("offset", 3*60*60)
	// fromZone представляет начало диапазона в другом часовом поясе. toZone представляет конец
	// диапазона в другом часовом поясе.
	fromZone, toZone := base.In(zone), to.In(zone)
	request.Query.From, request.Query.To = &fromZone, &toZone
	// repeated содержит повторную страницу того же диапазона в другом часовом поясе.
	repeated, err := svc.ListStreams(ctx, request)
	if err != nil || !reflect.DeepEqual(second, repeated) {
		t.Fatalf("тот же диапазон в другой зоне изменил страницу: %+v, %v", repeated, err)
	}
	// invalid задаёт повреждённое представление курсора.
	for _, invalid := range []string{"%", "e30", first.NextCursor + "!"} {
		request.Cursor = invalid
		_, err = svc.ListStreams(ctx, request)
		requireDomainError(t, err, core.ErrInvalidCursor)
	}
	// data содержит JSON курсора для подмены версии.
	data, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	// cursor содержит поля раскодированного курсора для искусственного изменения версии.
	var cursor map[string]json.RawMessage
	// err сообщает об ошибке разбора JSON проверяемого курсора.
	if err := json.Unmarshal(data, &cursor); err != nil {
		t.Fatal(err)
	}
	cursor["Version"] = json.RawMessage("999")
	data, err = json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	request.Cursor = base64.RawURLEncoding.EncodeToString(data)
	_, err = svc.ListStreams(ctx, request)
	requireDomainError(t, err, core.ErrInvalidCursor)
}

// Проверяет точную семантику From ≤ created_at < To для наносекундных границ поверх микросекундного
// хранения через репозиторий и Service.
func TestDateRangesPreserveSubmicrosecondExclusiveBoundaries(t *testing.T) {
	// s содержит временную SQLite с пятью упорядоченными потоками. svc предоставляет доменные
	// операции текущего владельца временного Store. base задаёт время первых трёх потоков и начало
	// проверяемого диапазона.
	s, svc, base := historyFixture(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// from задаёт включительную нижнюю временную границу выборки.
	from := base.Add(time.Nanosecond)
	// to задаёт исключительную верхнюю временную границу выборки.
	to := base.Add(time.Microsecond + time.Nanosecond)
	// boundary содержит вариант полуинтервала и ожидаемые идентификаторы потоков.
	for _, boundary := range []struct {
		// name задаёт имя сценария временных границ.
		name string
		// from задаёт включительную нижнюю временную границу выборки. to задаёт исключительную
		// верхнюю временную границу выборки.
		from, to *time.Time
		// ids содержит ожидаемые либо фактически выбранные ID потоков в порядке страницы.
		ids []core.ID
	}{
		{"from", &from, nil, []core.ID{"d", "e"}},
		{"to", nil, &to, []core.ID{"a", "b", "c", "d"}},
		{"both", &from, &to, []core.ID{"d"}},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			// Проверяет точные ID в полуинтервале выбранных границ с долями микросекунды через репозиторий и
			// Service.

			// query задаёт проверяемые границы и ограничение выборки потоков.
			query := core.ListQuery{Limit: 10, From: boundary.from, To: boundary.to}
			// values принимает потоки, выбранные репозиторием по точным границам.
			var values []core.Stream
			// err сообщает об ошибке выборки потоков по проверяемым точным временным
			// границам.
			if err := s.View(ctx, func(r core.Repositories) error {
				// Читает Streams по выбранным границам времени для прямой проверки фильтра репозитория.
				// err сообщает об ошибке чтения потоков по выбранным временным границам.
				var err error
				values, err = r.Config.Streams(ctx, query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// ids содержит ожидаемые либо фактически выбранные ID потоков в порядке
			// страницы.
			ids := make([]core.ID, 0, len(values))
			// value содержит очередной поток с известными временем и ID.
			for _, value := range values {
				ids = append(ids, value.ID)
			}
			if !reflect.DeepEqual(ids, boundary.ids) {
				t.Fatalf("repository [from,to) = %v, нужно %v", ids, boundary.ids)
			}
			// page содержит страницу Service для сравнения с прямой выборкой репозитория.
			// err сообщает об ошибке чтения страницы потоков.
			page, err := svc.ListStreams(ctx, core.PageRequest{Query: query})
			if err != nil {
				t.Fatal(err)
			}
			ids = ids[:0]
			// value содержит очередной поток с известными временем и ID.
			for _, value := range page.Items {
				ids = append(ids, value.ID)
			}
			if !reflect.DeepEqual(ids, boundary.ids) {
				t.Fatalf("service [from,to) = %v, нужно %v", ids, boundary.ids)
			}
		})
	}
}

// Проверяет отказ неограниченных страниц, недопустимых диапазонов, прямого After и неподдерживаемых
// фильтров.
func TestListRejectsUnboundedAndInvalidQueries(t *testing.T) {
	// svc предоставляет доменные операции текущего владельца временного Store. base задаёт время
	// первых трёх потоков и начало проверяемого диапазона.
	_, svc, base := historyFixture(t)
	// query задаёт проверяемые границы и ограничение выборки потоков.
	for _, query := range []core.ListQuery{{Limit: 0}, {Limit: core.MaxListLimit + 1}, {Limit: 1, From: &base, To: &base}, {Limit: 1, After: &core.ListBoundary{CreatedAt: base, ID: "a"}}, {Limit: 1, StreamID: "unsupported-filter"}} {
		// err сообщает об ошибке чтения страницы потоков.
		_, err := svc.ListStreams(context.Background(), core.PageRequest{Query: query})
		if !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неограниченный или недопустимый фильтр принят: %+v, %v", query, err)
		}
	}
}

// Проверяет допуск OBS по сохранённому ключу только после RUNNING, сохранение допуска при паузе и
// запрет после принятого Stop.
func TestAuthorizeOBSUsesActiveSessionAndStopsAdmission(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// err сообщает об ошибке проверки допуска OBS по текущему запуску и ключу.
	if _, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); err == nil {
		t.Fatal("OBS принят до RUNNING")
	}
	executeCommand(t, f.service, start.RequestID)
	// err сообщает об ошибке проверки допуска OBS по текущему запуску и ключу.
	if _, err := f.service.AuthorizeOBS(ctx, f.stream.ID, "wrong-key"); err == nil {
		t.Fatal("OBS принят с чужим ключом")
	}
	// err сообщает об ошибке принятия ручной паузы.
	if _, err := f.service.PauseSession(ctx, core.SessionCommand{RequestID: "pause-for-admission", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	// authorized содержит текущую сессию, допущенную к публикации OBS. err сообщает об ошибке
	// проверки допуска OBS по текущему запуску и ключу.
	authorized, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil || authorized.ID != start.SessionID {
		t.Fatalf("пауза отозвала допустимую публикацию: %+v, %v", authorized, err)
	}
	// err сообщает об ошибке принятия остановки сессии.
	if _, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-admission", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке проверки допуска OBS по текущему запуску и ключу.
	if _, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); err == nil {
		t.Fatal("принятый stop не отозвал OBS admission")
	}
}
