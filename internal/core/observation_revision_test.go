package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Проверяет продвижение метки каждого принятого повторного наблюдения без дублирования событий и без
// текста ошибки в нагрузке; переполнение ревизии сохраняет снимок.
func TestRepeatedObservationsAdvanceStampWithoutDuplicateEvents(t *testing.T) {
	// kind выбирает вид наблюдателя, для которого проверяется поведение.
	for _, kind := range []string{"session", "group", "output", "output_terminal", "ffmpeg"} {
		t.Run(kind, func(t *testing.T) {
			// Для выбранного вида наблюдателя проверяет продвижение метки повторного значения без повторного
			// события состояния.

			synctest.Test(t, func(t *testing.T) {
				// В изолированном времени проверяет три одинаковых наблюдения, отсутствие диагностического текста в
				// событии и неизменность снимка при переполнении Revision.

				// ctx задаёт контекст операций проверяемого сценария оперативных
				// наблюдений.
				ctx := context.Background()
				// s предоставляет доменные операции с общим реестром наблюдений. u
				// предоставляет изменяемые тестовые репозитории и учитывает обращения к
				// БД. parent идентифицирует точное выполнение родительской сессии.
				// output идентифицирует точную попытку тестового выхода.
				s, u, parent, output := telemetryTestService(t, telemetryTestLimits())
				// session содержит наблюдателя текущего выполнения сессии. err сообщает
				// об ошибке регистрации наблюдателя точной сессии.
				session, err := s.OpenSessionObserver(ctx, parent)
				if err != nil {
					t.Fatal(err)
				}
				// failure задаёт синтетическую диагностику, которая не должна попадать в
				// событие.
				const failure = "синтетическая закрытая диагностика"
				// handle сохраняет общий дескриптор выбранного наблюдателя для проверки
				// token и ревизии.
				handle := session.observerHandle
				// observe повторяет одно и то же наблюдение выбранного владельца.
				observe := func() error {
					// Публикует неизменное READY-наблюдение Session с заданной диагностикой.
					return session.Observe(ctx, SessionObservation{State: ObservationReady, Error: failure})
				}
				switch kind {
				case "group", "ffmpeg":
					// group содержит наблюдателя отдельной оперативной группы. err
					// сообщает об ошибке регистрации независимой оперативной группы.
					group, err := session.OpenGroup(ctx, "group", "profile")
					if err != nil {
						t.Fatal(err)
					}
					handle = group.observerHandle
					observe = func() error {
						// Публикует неизменное READY-наблюдение группы с источником LIVE и заданной диагностикой.

						return group.Observe(ctx, GroupObservation{State: ObservationReady, Source: ObservationSourceLive, Error: failure})
					}
					if kind == "ffmpeg" {
						// worker содержит наблюдателя работника FFmpeg внутри
						// группы. err сообщает об ошибке регистрации работника
						// FFmpeg с выбранной ролью.
						worker, err := group.OpenFFmpegRole(ctx, FFmpegLiveTranscode)
						if err != nil {
							t.Fatal(err)
						}
						// attempt содержит наблюдателя текущей попытки FFmpeg.
						attempt, err := worker.BeginAttempt(ctx, 1)
						if err != nil {
							t.Fatal(err)
						}
						handle = attempt.observerHandle
						observe = func() error {
							// Публикует неизменное READY-наблюдение текущей попытки FFmpeg с заданной диагностикой.
							return attempt.Observe(ctx, FFmpegObservation{State: ObservationReady, Error: failure})
						}
					}
				case "output", "output_terminal":
					// owner содержит наблюдателя точной попытки выхода. err сообщает
					// об ошибке регистрации наблюдателя точной попытки выхода.
					owner, err := s.OpenOutputObserver(ctx, output)
					if err != nil {
						t.Fatal(err)
					}
					handle = owner.observerHandle
					observe = func() error {
						// Публикует неизменное PUBLISHING-наблюдение выхода с заданной диагностикой.
						return owner.Observe(ctx, OutputObservation{State: OutputPublishing, Error: failure})
					}
					if kind == "output_terminal" {
						observe = func() error {
							// Публикует неизменное терминальное FAILED-наблюдение выхода с заданной диагностикой.

							return owner.ObserveTerminal(ctx, OutputObservation{State: OutputFailed, Error: failure})
						}
					}
				}
				// views сохраняет число чтений БД перед проверяемой операцией RAM.
				views := u.views
				// anchor фиксирует согласованный снимок и исходную позицию событий.
				anchor, err := s.RuntimeAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				// i задаёт ожидаемую ревизию после очередного принятого одинакового
				// наблюдения.
				for i := uint64(1); i <= 3; i++ {
					time.Sleep(time.Second)
					// err сообщает об ошибке повторной записи того же наблюдения.
					if err := observe(); err != nil {
						t.Fatal(err)
					}
					// snapshot содержит согласованную копию текущих наблюдений
					// сессии. err сообщает об ошибке чтения текущих наблюдений
					// сессии.
					snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
					if err != nil {
						t.Fatal(err)
					}
					// stamp содержит метку выбранного вида наблюдения из
					// согласованного снимка.
					stamp := snapshot.Session.Stamp
					switch kind {
					case "group":
						stamp = snapshot.Groups[0].Stamp
					case "output", "output_terminal":
						stamp = snapshot.Outputs[0].Stamp
					case "ffmpeg":
						stamp = snapshot.FFmpeg[0].Stamp
					}
					if stamp.Revision != i || !stamp.ObservedAt.Equal(time.Now()) || stamp.Token != handle.Token() {
						t.Fatalf("принятое наблюдение не обновило метку: %+v", stamp)
					}
				}
				// page содержит ограниченную страницу оперативных событий после
				// исходного курсора.
				page, err := s.RuntimeEvents(ctx, anchor.Cursor, 16)
				if err != nil || page.Gap || len(page.Events) != 1 {
					t.Fatalf("неизменное состояние повторило событие: %+v, %v", page, err)
				}
				// event содержит выбранное оперативное событие для проверки владельца и
				// безопасной нагрузки.
				event := page.Events[0]
				if event.Kind != RuntimeStateEvent || event.Owner != ownerKind(handle.node.kind) || event.Stamp.Revision != 1 || event.Stamp.Token != handle.Token() || event.Session != observationSessionFence(parent) || event.Reason != ReasonObservationFailed {
					t.Fatalf("событие потеряло точного владельца: %+v", event)
				}
				if kind == "ffmpeg" && (event.Stamp.AttemptID != 1 || event.Role != FFmpegLiveTranscode || event.GroupInstanceID != handle.node.parent) {
					t.Fatalf("событие потеряло попытку FFmpeg: %+v", event)
				}
				if (kind == "output" || kind == "output_terminal") && event.Execution != output {
					t.Fatalf("событие потеряло ExecutionFence: %+v", event)
				}
				// encoded содержит JSON оперативных событий для проверки отсутствия
				// текста диагностики.
				encoded, err := json.Marshal(event)
				if err != nil || strings.Contains(string(encoded), failure) {
					t.Fatal("безопасное событие содержит текст диагностики", err)
				}
				handle.node.stamp.Revision = math.MaxUint64
				// before сохраняет согласованный снимок до проверяемого действия.
				before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
				if err != nil {
					t.Fatal(err)
				}
				if err = observe(); !errors.Is(err, ErrConflict) {
					t.Fatal("повторное наблюдение обошло исчерпание ревизии", err)
				}
				// after содержит согласованный снимок после проверяемого действия для
				// проверки сохранности.
				after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
				if err != nil || !reflect.DeepEqual(before, after) || u.views != views || u.writes != 0 {
					t.Fatal("отказ переполнения изменил снимок или обратился к БД", err)
				}
			})
		})
	}
}

// Проверяет независимость ревизий состояния, источника и метрик группы, отклонение поздних наблюдений
// и одно событие для неизменного значения.
func TestGroupObservationSourceRevisionAndMetricsRemainIndependent(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. parent идентифицирует точное
	// выполнение родительской сессии.
	s, _, parent, _ := telemetryTestService(t, telemetryTestLimits())
	// session содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	session, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// group содержит наблюдателя отдельной оперативной группы.
	group, err := session.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	// media задаёт синтетическую медиавыборку для независимой проверки состояния и метрик.
	media := testMedia(time.Now().UTC())
	// metrics содержит согласованный комплект измерений трёх медиапутей группы.
	metrics := GroupMetrics{LiveInput: media, FallbackInput: media, Output: media}
	if err = group.ObserveMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	// anchor фиксирует согласованный снимок и исходную позицию событий.
	anchor, err := s.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// value содержит повторяемое наблюдение группы с выбранным источником.
	value := GroupObservation{State: ObservationReady, Source: ObservationSourceLive}
	// revision задаёт проверяемую ревизию выбора источника группы.
	for revision := uint64(1); revision <= 3; revision++ {
		value.SourceRevision = revision
		if err = group.Observe(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	// before сохраняет согласованный снимок до проверяемого действия.
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || before.Groups[0].Stamp.Revision != 3 || *before.Groups[0].Value != value || *before.Groups[0].Metrics != metrics || before.Groups[0].MetricsStamp.Revision != 1 {
		t.Fatal("новая ревизия источника потеряна или изменила метрики", before, err)
	}
	// revision задаёт проверяемую ревизию выбора источника группы.
	for _, revision := range []uint64{0, 1, 2, 3} {
		// stale содержит копию наблюдения с устаревшей ревизией источника.
		stale := value
		stale.SourceRevision = revision
		if err = group.Observe(ctx, stale); !errors.Is(err, ErrStale) {
			t.Fatal("старая ревизия источника принята", revision, err)
		}
	}
	// after содержит согласованный снимок после проверяемого действия для проверки сохранности.
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("позднее наблюдение изменило снимок или курсор", err)
	}
	// page содержит ограниченную страницу оперативных событий после исходного курсора.
	page, err := s.RuntimeEvents(ctx, anchor.Cursor, 16)
	if err != nil || len(page.Events) != 1 || page.Events[0].SourceRevision != 1 || page.Events[0].Stamp.Revision != 1 {
		t.Fatal("ревизия источника повторила событие", page, err)
	}
	value.SourceRevision, value.Source, value.State = 4, ObservationSourceFallback, ObservationRetrying
	if err = group.Observe(ctx, value); err != nil {
		t.Fatal(err)
	}
	// next содержит события после фактического изменения источника и состояния группы.
	next, err := s.RuntimeEvents(ctx, page.Cursor, 16)
	if err != nil || len(next.Events) != 1 || next.Events[0].SourceRevision != 4 || next.Events[0].Stamp.Revision != 4 || next.Events[0].Source != ObservationSourceFallback {
		t.Fatal("переход после повторов потерял актуальную ревизию", next, err)
	}
}

// Проверяет, что ошибка текста, бюджета или переполнение не продвигают SourceRevision и не мешают
// последующему допустимому наблюдению.
func TestRejectedGroupObservationPreservesSourceRevision(t *testing.T) {
	// rejection выбирает отказ текста, бюджета или переполнения ревизии.
	for _, rejection := range []string{"invalid", "budget", "overflow"} {
		t.Run(rejection, func(t *testing.T) {
			// Проверяет, что выбранный отказ из-за нулевого байта, бюджета или переполнения не продвигает
			// SourceRevision и не меняет снимок и курсор.

			// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
			ctx := context.Background()
			// s предоставляет доменные операции с общим реестром наблюдений. parent
			// идентифицирует точное выполнение родительской сессии.
			s, _, parent, _ := telemetryTestService(t, telemetryTestLimits())
			// session содержит наблюдателя текущего выполнения сессии. err сообщает об
			// ошибке регистрации наблюдателя точной сессии.
			session, err := s.OpenSessionObserver(ctx, parent)
			if err != nil {
				t.Fatal(err)
			}
			// group содержит наблюдателя отдельной оперативной группы.
			group, err := session.OpenGroup(ctx, "group", "profile")
			if err != nil {
				t.Fatal(err)
			}
			// value содержит повторяемое наблюдение группы с выбранным источником.
			value := GroupObservation{State: ObservationReady, Source: ObservationSourceLive, SourceRevision: 1}
			if err = group.Observe(ctx, value); err != nil {
				t.Fatal(err)
			}
			// rejected содержит кандидат наблюдения, который должен быть отклонён.
			rejected := value
			rejected.SourceRevision = 2
			// want задаёт ожидаемую категорию отказа проверяемого сценария.
			want := ErrInvalid
			switch rejection {
			case "invalid":
				rejected.Error = "недопустимый\x00текст"
			case "budget":
				rejected.Error = strings.Repeat("x", 257)
				want = ErrObservationLimit
			case "overflow":
				group.node.stamp.Revision = math.MaxUint64
				want = ErrConflict
			}
			// before сохраняет согласованный снимок до проверяемого действия.
			before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
			if err != nil {
				t.Fatal(err)
			}
			if err = group.Observe(ctx, rejected); !errors.Is(err, want) {
				t.Fatal("неверный результат отклонённого наблюдения", err)
			}
			// after содержит согласованный снимок после проверяемого действия для проверки
			// сохранности.
			after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("отказ изменил значение, ревизию источника или курсор", err)
			}
			if rejection == "overflow" {
				group.node.stamp.Revision = 1
			}
			value.SourceRevision = 2
			if err = group.Observe(ctx, value); err != nil {
				t.Fatal("отказ преждевременно продвинул внутреннюю ревизию источника", err)
			}
		})
	}
}
