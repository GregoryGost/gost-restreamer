package session

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
)

// TestManagerLatestDesiredControlReachesLateGroupWithoutLiveSource проверяет применение последней
// ревизии паузы к поздней группе без LIVE и отклонение устаревшего или противоречивого управления.
func TestManagerLatestDesiredControlReachesLateGroupWithoutLiveSource(t *testing.T) {
	// entered сообщает о входе задержанного построителя; released разрешает запуск проигрывателя
	// после проверки управления.
	entered, released := make(chan struct{}), make(chan struct{})
	// enteredOnce ограничивает сигнал входа одним закрытием; releasedOnce ограничивает одним
	// закрытием разрешение продолжения.
	var enteredOnce, releasedOnce sync.Once
	// release разрешает задержанному построителю продолжить запуск ровно один раз.
	release := func() {
		// Разрешает задержанному проигрывателю продолжить запуск, защищая канал от повторного закрытия.

		releasedOnce.Do(func() {
			// Однократно закрывает канал разрешения построителя команды проигрывателя.
			close(released)
		})
	}
	// f предоставляет сессию с поздно готовящейся группой для проверки ревизий желаемой паузы.
	f := newManagerFixture(t, false, func(c *Config) {
		// Устанавливает согласованные сроки и задерживает построитель проигрывателя, чтобы управление пришло
		// до готовности новой группы.

		c.Group.FFmpeg.StartupTimeout = c.StartupTimeout
		// original сохраняет исходный построитель проигрывателя для запуска после управляемой
		// задержки.
		original := c.Group.Player
		c.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
			// Обозначает вход в построитель и ждёт разрешения теста либо отмены контекста перед построением
			// команды.

			enteredOnce.Do(func() {
				// Однократно сообщает тесту, что построитель проигрывателя уже начал работу.
				close(entered)
			})
			select {
			case <-released:
				return original(ctx, binary, file, attempt)
			case <-ctx.Done():
				return ffmpeg.Command{}, ctx.Err()
			}
		}
	})
	t.Cleanup(release)
	// parent содержит успешную квитанцию родительской сессии без начальных назначений.
	parent := startAdmissionParent(t, f)
	// ctx ограничивает сценарий управления восемью секундами; cancel освобождает ресурсы этого
	// срока.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// pause содержит команду сохранения желаемой паузы; err сообщает отказ её приёма.
	pause, err := f.manager.PauseSession(ctx, core.SessionCommand{RequestID: "desired-pause", SessionID: parent.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешное завершение команды паузы до появления новой группы.
	if current := waitReceipt(t, f, pause); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// paused содержит сохранённое желание паузы и его ревизию.
	paused, err := f.service.GetSession(ctx, parent.SessionID)
	if err != nil || !paused.DesiredPaused {
		t.Fatal("пауза не сохранила желание", paused, err)
	}
	// added содержит принятую команду добавления поздней группы к приостановленной сессии.
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "late-controlled-output", SessionID: parent.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	awaitPublication(t, ctx, entered)
	f.manager.mu.Lock()
	// node указывает локального владельца родительской сессии.
	node := f.manager.sessions[parent.SessionID]
	f.manager.mu.Unlock()
	// currentGroup сохраняет группу нового участника для наблюдения применения управления до
	// готовности источника.
	var currentGroup *group.Group
	waitRetirementCondition(t, ctx, func() bool {
		// Проверяет наличие ссылки нового членства и сохраняет её группу для проверки текущей паузы.

		node.mu.Lock()
		// member указывает участника добавляемого назначения, чья ссылка группы ожидается.
		member := node.members[added.OutputID]
		node.mu.Unlock()
		if member == nil {
			return false
		}
		member.mu.Lock()
		defer member.mu.Unlock()
		if member.reference == nil {
			return false
		}
		currentGroup = member.reference.Group()
		return true
	})
	// status содержит состояние поздней группы после применения первоначального желания паузы.
	if status := currentGroup.Status(); !status.Paused || status.Available {
		t.Fatal("новая группа не получила паузу до появления источника", status)
	}
	// resume содержит команду снятия желаемой паузы до появления LIVE.
	resume, err := f.manager.ResumeSession(ctx, core.SessionCommand{RequestID: "desired-resume", SessionID: parent.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит завершение возобновления при ещё не готовой группе.
	if current := waitReceipt(t, f, resume); current.Status != core.CommandSucceeded {
		t.Fatal("отсутствие LIVE помешало применить желание", current)
	}
	// resumed содержит новое желание и повышенную ревизию после снятия паузы.
	resumed, err := f.service.GetSession(ctx, parent.SessionID)
	if err != nil || resumed.DesiredPaused || resumed.Revision <= paused.Revision {
		t.Fatal("возобновление не повысило ревизию желания", resumed, paused, err)
	}
	// status содержит состояние группы после возобновления при ещё недоступном источнике.
	if status := currentGroup.Status(); status.Paused || status.Available {
		t.Fatal("успешное желание не применилось к ещё не готовой группе", status)
	}
	if _, err = f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); err != nil {
		t.Fatal("пауза или возобновление отозвали OBS", err)
	}
	node.mu.Lock()
	// registry содержит реестр групп для подачи устаревшей и противоречащей ревизий управления.
	registry := node.registry
	node.mu.Unlock()
	if err = registry.ApplyControl(paused.Revision, true); err != nil {
		t.Fatal(err)
	}
	// status содержит состояние группы после повторной подачи прежней ревизии паузы.
	if status := currentGroup.Status(); status.Paused {
		t.Fatal("поздняя старая ревизия отменила возобновление", status)
	}
	if err = registry.ApplyControl(resumed.Revision, true); !errors.Is(err, core.ErrConflict) {
		t.Fatal("одна ревизия приняла противоречащее желание", err)
	}
	// status содержит состояние группы после отклонённого противоречия в текущей ревизии.
	if status := currentGroup.Status(); status.Paused {
		t.Fatal("отклонённое желание изменило группу", status)
	}
	release()
	// current содержит результат добавления после разрешения запуска проигрывателя.
	if current := waitReceipt(t, f, added); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// status содержит состояние готовой группы для проверки источника заглушки и последнего желания
	// паузы.
	if status := currentGroup.Status(); status.Paused || !status.Available || status.Source != group.Fallback {
		t.Fatal("после готовности изменилось последнее желание или выдуман LIVE", status)
	}
	// duplicate содержит ответ повторной старой команды паузы после более нового возобновления.
	duplicate, err := f.manager.PauseSession(ctx, core.SessionCommand{RequestID: pause.RequestID, SessionID: parent.SessionID})
	if err != nil || duplicate.ID != pause.ID {
		t.Fatal("повтор старой команды потерял квитанцию", duplicate, err)
	}
	// after содержит состояние сессии после повторной старой паузы для проверки неизменности нового
	// желания.
	after, err := f.service.GetSession(ctx, parent.SessionID)
	if err != nil || after.DesiredPaused || after.Revision != resumed.Revision || currentGroup.Status().Paused {
		t.Fatal("повтор старой паузы изменил новое желание", after, resumed, err)
	}
}

// TestManagerClosedRejectsEveryCommandWithoutDurableAdmission проверяет ErrClosed для всех
// перечисленных команд закрытого Manager и отсутствие их сохранённых квитанций.
func TestManagerClosedRejectsEveryCommandWithoutDurableAdmission(t *testing.T) {
	// f предоставляет работающую сессию для проверки команд после закрытия менеджера.
	f := newManagerFixture(t, false, nil)
	// parent содержит квитанцию родительского запуска до закрытия менеджера.
	parent := startAdmissionParent(t, f)
	// err сообщает отказ закрытия менеджера перед проверкой запрета всех команд.
	if err := f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// session задаёт общий идентификатор отклоняемых команд управления сессией.
	session := core.SessionCommand{RequestID: "closed-session-command", SessionID: parent.SessionID}
	// output задаёт общий идентификатор отклоняемых команд несуществующего назначения.
	output := core.OutputCommand{RequestID: "closed-output-command", OutputID: "uncreated-output"}
	// test задаёт очередную команду закрытого менеджера и её имя.
	for _, test := range []struct {
		// name задаёт имя проверяемой команды закрытого менеджера.
		name string
		// call выполняет выбранную команду через уже закрытый менеджер.
		call func(context.Context) (core.CommandReceipt, error)
	}{
		{name: "add", call: func(ctx context.Context) (core.CommandReceipt, error) {
			// Пытается добавить выход через уже закрытый Manager.

			return f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "closed-add", SessionID: parent.SessionID, DestinationID: f.destination.ID})
		}},
		{name: "pause", call: func(ctx context.Context) (core.CommandReceipt, error) {
			// Пытается поставить сессию на паузу через уже закрытый Manager.
			return f.manager.PauseSession(ctx, session)
		}},
		{name: "resume", call: func(ctx context.Context) (core.CommandReceipt, error) {
			// Пытается снять паузу сессии через уже закрытый Manager.
			return f.manager.ResumeSession(ctx, session)
		}},
		{name: "stop_session", call: func(ctx context.Context) (core.CommandReceipt, error) {
			// Пытается остановить сессию через уже закрытый Manager.
			return f.manager.StopSession(ctx, session)
		}},
		{name: "start_output", call: func(ctx context.Context) (core.CommandReceipt, error) {
			// Пытается запустить выход через уже закрытый Manager.
			return f.manager.StartOutput(ctx, output)
		}},
		{name: "stop_output", call: func(ctx context.Context) (core.CommandReceipt, error) {
			// Пытается остановить выход через уже закрытый Manager.
			return f.manager.StopOutput(ctx, output)
		}},
		{name: "detach", call: func(ctx context.Context) (core.CommandReceipt, error) {
			// Пытается отсоединить выход через уже закрытый Manager.
			return f.manager.DetachOutput(ctx, output)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Проверяет, что выбранная команда закрытого Manager возвращает ErrClosed и пустую квитанцию.

			// receipt содержит ответ отклоняемой команды; err проверяется на ErrClosed.
			receipt, err := test.call(context.Background())
			if !errors.Is(err, core.ErrClosed) || receipt.ID != "" {
				t.Fatal("закрытый Manager принял команду", receipt, err)
			}
		})
	}
	// receipts содержит сохранённые команды родительской сессии; err сообщает отказ чтения для
	// проверки отсутствия поздних квитанций.
	receipts, err := f.service.ListReceipts(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	// receipt содержит очередную сохранённую команду, проверяемую на идентификаторы отклонённых
	// запросов.
	for _, receipt := range receipts.Items {
		if receipt.RequestID == session.RequestID || receipt.RequestID == output.RequestID || receipt.RequestID == "closed-add" {
			t.Fatal("отклонённая команда оставила долговечную квитанцию", receipt)
		}
	}
}
