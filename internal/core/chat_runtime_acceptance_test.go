package core

import (
	"context"
	"errors"
	"testing"
)

// TestChatRuntimeFirstPublicationSurvivesRuntimeReplayGap проверяет признак первой публикации
// по снимкам до и после замещения событий. t сообщает ошибки точного допуска выхода.
func TestChatRuntimeFirstPublicationSurvivesRuntimeReplayGap(t *testing.T) {
	// ctx задаёт контекст операций с оперативным реестром.
	ctx := context.Background()
	// limits оставляет в журнале два события, чтобы до новой сверки произошёл разрыв.
	limits := telemetryTestLimits()
	limits.Events.MaxEvents, limits.Events.MaxPageSize = 2, 2
	// service предоставляет общий оперативный реестр, parent и child адресуют его точные исполнения.
	service, _, parent, child := telemetryTestService(t, limits)
	// session и output представляют текущих наблюдателей, err сообщает об ошибке их регистрации.
	session, err := service.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	output, err := service.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	// before фиксирует точного родителя, ещё не публиковавший выход и позицию до смены состояний.
	before, err := service.RuntimeAnchor(ctx)
	if err != nil || len(before.Sessions) != 1 || len(before.Sessions[0].Outputs) != 1 || before.Sessions[0].Session == nil {
		t.Fatalf("Исходные наблюдения отсутствуют: %+v, %v", before, err)
	}
	// owner и proof удерживают выданные свидетельства точного родителя и выхода между сверками.
	owner := before.Sessions[0].Session.ChatOwner
	proof := before.Sessions[0].Outputs[0].ChatPublication
	if !owner.Active() || proof == nil || proof.Published() {
		t.Fatal("Регистрация наблюдателя изменила допуск до первой публикации")
	}
	// page содержит сигнал изменения из чтения журнала для сопоставления с исходным снимком.
	page, err := service.RuntimeEvents(ctx, before.Cursor, 2)
	if err != nil || page.ChatChanged != before.ChatChanged {
		t.Fatalf("Сигнал чатов не согласован с курсором: %+v, %v", page, err)
	}
	session.NotifyChatRunning()
	requireObservationDone(t, before.ChatChanged)

	// steps перечисляет допустимые переходы до, во время и после успешной передачи медиа.
	steps := []struct {
		// state задаёт наблюдение текущей попытки, published — ожидаемый допуск чата после него.
		state     OutputState
		published bool
	}{
		{OutputConnecting, false},
		{OutputRetrying, false},
		{OutputPublishing, true},
		{OutputRetrying, true},
		{OutputConnecting, true},
	}
	// live получает результаты одной согласованной проверки для выхода и отсутствующего доказательства.
	live := make([]bool, 2)
	// step содержит очередное допустимое наблюдение той же попытки.
	for _, step := range steps {
		if err = output.Observe(ctx, OutputObservation{State: step.state}); err != nil {
			t.Fatal(err)
		}
		if err = service.CheckChatPublications(ctx, owner, []*ChatOutputPublication{proof, nil}, live); err != nil || live[0] != step.published || live[1] {
			t.Fatalf("Состояние %s дало допуск %v, ожидается %t только для текущего выхода: %v", step.state, live, step.published, err)
		}
	}
	page, err = service.RuntimeEvents(ctx, before.Cursor, 2)
	if err != nil || !page.Gap {
		t.Fatalf("Старый курсор не выявил разрыв журнала: %+v, %v", page, err)
	}
	// foreign обозначает заведомо чужую эпоху для проверки сигнала полной повторной сверки.
	foreign := before.Cursor
	foreign.Epoch = "foreign-epoch"
	page, err = service.RuntimeEvents(ctx, foreign, 2)
	if err != nil || !page.Reset {
		t.Fatalf("Чужая эпоха не потребовала полной сверки: %+v, %v", page, err)
	}
	// after содержит точную попытку в CONNECTING после вытеснения первого PUBLISHING.
	after, err := service.RuntimeAnchor(ctx)
	if err != nil || len(after.Sessions) != 1 || len(after.Sessions[0].Outputs) != 1 {
		t.Fatalf("Текущее наблюдение потеряно при разрыве событий: %+v, %v", after, err)
	}
	// current связывает текущее состояние CONNECTING с сохранённым признаком первой публикации.
	current := after.Sessions[0].Outputs[0]
	if current.Value == nil || current.Value.State != OutputConnecting || !current.ChatPublication.Published() {
		t.Fatalf("Разрыв журнала стёр успешную публикацию текущей попытки: %+v", current)
	}
}

// TestChatRuntimeRevocationDoesNotAwaitPersistentStop проверяет отмену допуска независимо от SQLite.
// t проверяет терминальное наблюдение, удаление, запрет исполнения и замену родителя.
func TestChatRuntimeRevocationDoesNotAwaitPersistentStop(t *testing.T) {
	// ctx задаёт контекст синхронных операций регистрации, допуска и отзыва.
	ctx := context.Background()
	// scenarios связывает каждую форму остановки с ожидаемым временем жизни родительского владельца.
	scenarios := []struct {
		// name различает причину отзыва. parentRevoked ожидается только после замены родителя.
		name          string
		parentRevoked bool
	}{
		{"терминальное наблюдение", false},
		{"удаление наблюдателя", false},
		{"замена наблюдателя выхода", false},
		{"запрет исполнения", false},
		{"замена родителя", true},
	}
	// scenario содержит причину отзыва и ожидаемую пригодность родителя.
	for _, scenario := range scenarios {
		// t относится к изолированному реестру одной формы отзыва.
		t.Run(scenario.name, func(t *testing.T) {
			// service предоставляет операции реестра, store хранит управление в RUNNING,
			// parent и child адресуют точные исполнения с успешными квитанциями.
			service, store, parent, child := telemetryTestService(t, telemetryTestLimits())
			// session и output представляют живых наблюдателей, err сообщает о регистрации или отзыве.
			session, err := service.OpenSessionObserver(ctx, parent)
			if err != nil {
				t.Fatal(err)
			}
			output, err := service.OpenOutputObserver(ctx, child)
			if err != nil {
				t.Fatal(err)
			}
			// lease и reservation связывают допуск с точной физической попыткой без сетевых операций.
			lease, err := service.BindExecutionOwner(ctx, child)
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := service.ReserveOutputExecution(ctx, lease, store.repository.outputs[child.OutputID].Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err = reservation.Activate(ctx, output); err != nil {
				t.Fatal(err)
			}
			if err = output.Observe(ctx, OutputObservation{State: OutputPublishing}); err != nil {
				t.Fatal(err)
			}
			// anchor удерживает точный допуск, доступный уже поставленному в очередь сообщению.
			anchor, err := service.RuntimeAnchor(ctx)
			if err != nil || len(anchor.Sessions) != 1 || len(anchor.Sessions[0].Outputs) != 1 || anchor.Sessions[0].Session == nil {
				t.Fatalf("Ожидаются точные наблюдения родителя и выхода: %+v, %v", anchor, err)
			}
			// owner остаётся активным после остановки одного выхода. proof относится лишь к этому выходу.
			owner := anchor.Sessions[0].Session.ChatOwner
			proof := anchor.Sessions[0].Outputs[0].ChatPublication
			if !owner.Active() || !proof.Published() {
				t.Fatal("Проверка отзыва началась без действующего допуска")
			}
			switch scenario.name {
			case "терминальное наблюдение":
				err = output.ObserveTerminal(ctx, OutputObservation{State: OutputFailed})
			case "удаление наблюдателя":
				err = output.Close(ctx)
			case "замена наблюдателя выхода":
				_, err = service.OpenOutputObserver(ctx, child)
			case "запрет исполнения":
				reservation.Seal()
			case "замена родителя":
				_, err = service.OpenSessionObserver(ctx, parent)
			}
			if err != nil {
				t.Fatal(err)
			}
			requireObservationDone(t, anchor.ChatChanged)
			if store.repository.session.State != SessionRunning || store.writes != 0 {
				t.Fatal("Оперативный отзыв неожиданно изменил сохраняемый RUNNING")
			}
			// live получает согласованный результат для отозванного выхода.
			live := []bool{true}
			err = service.CheckChatPublications(ctx, owner, []*ChatOutputPublication{proof}, live)
			if scenario.parentRevoked {
				if owner.Active() || session.Valid() || !errors.Is(err, ErrStale) {
					t.Fatalf("Прежний родитель сохранил допуск после замены: %v", err)
				}
				return
			}
			if !owner.Active() || proof.Published() || err != nil || live[0] {
				t.Fatalf("Отзыв выхода нарушил допуск родителя либо сохранил публикацию: допуск %v, %v", live, err)
			}
			if err = service.CheckChatPublications(ctx, owner, nil, nil); err != nil {
				t.Fatalf("Лента живого родителя с нулём подходящих выходов должна сохраниться: %v", err)
			}
			// Даже если позднее наблюдение дошло до ещё живого объекта, оно не возвращает допуск чата.
			err = output.Observe(ctx, OutputObservation{State: OutputPublishing})
			if err != nil && !errors.Is(err, ErrStale) {
				t.Fatal(err)
			}
			if proof.Published() {
				t.Fatal("Позднее PUBLISHING восстановило отозванный допуск")
			}
			if scenario.name == "замена наблюдателя выхода" {
				// next содержит новое наблюдение того же выхода после регистрации.
				next, err := service.RuntimeSnapshot(ctx, observationSessionFence(parent))
				if err != nil || len(next.Outputs) != 1 || next.Outputs[0].ChatPublication == proof || next.Outputs[0].ChatPublication.Published() {
					t.Fatalf("Новый наблюдатель унаследовал публикацию прежнего: %+v, %v", next.Outputs, err)
				}
			}
		})
	}
}
