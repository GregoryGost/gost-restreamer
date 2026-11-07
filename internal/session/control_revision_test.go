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

func TestManagerLatestDesiredControlReachesLateGroupWithoutLiveSource(t *testing.T) {
	entered, released := make(chan struct{}), make(chan struct{})
	var enteredOnce, releasedOnce sync.Once
	release := func() { releasedOnce.Do(func() { close(released) }) }
	f := newManagerFixture(t, false, func(c *Config) {
		c.Group.FFmpeg.StartupTimeout = c.StartupTimeout
		original := c.Group.Player
		c.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-released:
				return original(ctx, binary, file, attempt)
			case <-ctx.Done():
				return ffmpeg.Command{}, ctx.Err()
			}
		}
	})
	t.Cleanup(release)
	parent := startAdmissionParent(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	pause, err := f.manager.PauseSession(ctx, core.SessionCommand{RequestID: "desired-pause", SessionID: parent.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, pause); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	paused, err := f.service.GetSession(ctx, parent.SessionID)
	if err != nil || !paused.DesiredPaused {
		t.Fatal("пауза не сохранила желание", paused, err)
	}
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "late-controlled-output", SessionID: parent.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	awaitPublication(t, ctx, entered)
	f.manager.mu.Lock()
	node := f.manager.sessions[parent.SessionID]
	f.manager.mu.Unlock()
	var currentGroup *group.Group
	waitRetirementCondition(t, ctx, func() bool {
		node.mu.Lock()
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
	if status := currentGroup.Status(); !status.Paused || status.Available {
		t.Fatal("новая группа не получила паузу до появления источника", status)
	}
	resume, err := f.manager.ResumeSession(ctx, core.SessionCommand{RequestID: "desired-resume", SessionID: parent.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, resume); current.Status != core.CommandSucceeded {
		t.Fatal("отсутствие LIVE помешало применить желание", current)
	}
	resumed, err := f.service.GetSession(ctx, parent.SessionID)
	if err != nil || resumed.DesiredPaused || resumed.Revision <= paused.Revision {
		t.Fatal("возобновление не повысило ревизию желания", resumed, paused, err)
	}
	if status := currentGroup.Status(); status.Paused || status.Available {
		t.Fatal("успешное желание не применилось к ещё не готовой группе", status)
	}
	if _, err = f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); err != nil {
		t.Fatal("пауза или возобновление отозвали OBS", err)
	}
	node.mu.Lock()
	registry := node.registry
	node.mu.Unlock()
	if err = registry.ApplyControl(paused.Revision, true); err != nil {
		t.Fatal(err)
	}
	if status := currentGroup.Status(); status.Paused {
		t.Fatal("поздняя старая ревизия отменила возобновление", status)
	}
	if err = registry.ApplyControl(resumed.Revision, true); !errors.Is(err, core.ErrConflict) {
		t.Fatal("одна ревизия приняла противоречащее желание", err)
	}
	if status := currentGroup.Status(); status.Paused {
		t.Fatal("отклонённое желание изменило группу", status)
	}
	release()
	if current := waitReceipt(t, f, added); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	if status := currentGroup.Status(); status.Paused || !status.Available || status.Source != group.Fallback {
		t.Fatal("после готовности изменилось последнее желание или выдуман LIVE", status)
	}
	duplicate, err := f.manager.PauseSession(ctx, core.SessionCommand{RequestID: pause.RequestID, SessionID: parent.SessionID})
	if err != nil || duplicate.ID != pause.ID {
		t.Fatal("повтор старой команды потерял квитанцию", duplicate, err)
	}
	after, err := f.service.GetSession(ctx, parent.SessionID)
	if err != nil || after.DesiredPaused || after.Revision != resumed.Revision || currentGroup.Status().Paused {
		t.Fatal("повтор старой паузы изменил новое желание", after, resumed, err)
	}
}

func TestManagerClosedRejectsEveryCommandWithoutDurableAdmission(t *testing.T) {
	f := newManagerFixture(t, false, nil)
	parent := startAdmissionParent(t, f)
	if err := f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	session := core.SessionCommand{RequestID: "closed-session-command", SessionID: parent.SessionID}
	output := core.OutputCommand{RequestID: "closed-output-command", OutputID: "uncreated-output"}
	for _, test := range []struct {
		name string
		call func(context.Context) (core.CommandReceipt, error)
	}{
		{name: "add", call: func(ctx context.Context) (core.CommandReceipt, error) {
			return f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "closed-add", SessionID: parent.SessionID, DestinationID: f.destination.ID})
		}},
		{name: "pause", call: func(ctx context.Context) (core.CommandReceipt, error) { return f.manager.PauseSession(ctx, session) }},
		{name: "resume", call: func(ctx context.Context) (core.CommandReceipt, error) { return f.manager.ResumeSession(ctx, session) }},
		{name: "stop_session", call: func(ctx context.Context) (core.CommandReceipt, error) { return f.manager.StopSession(ctx, session) }},
		{name: "start_output", call: func(ctx context.Context) (core.CommandReceipt, error) { return f.manager.StartOutput(ctx, output) }},
		{name: "stop_output", call: func(ctx context.Context) (core.CommandReceipt, error) { return f.manager.StopOutput(ctx, output) }},
		{name: "detach", call: func(ctx context.Context) (core.CommandReceipt, error) { return f.manager.DetachOutput(ctx, output) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt, err := test.call(context.Background())
			if !errors.Is(err, core.ErrClosed) || receipt.ID != "" {
				t.Fatal("закрытый Manager принял команду", receipt, err)
			}
		})
	}
	receipts, err := f.service.ListReceipts(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts.Items {
		if receipt.RequestID == session.RequestID || receipt.RequestID == output.RequestID || receipt.RequestID == "closed-add" {
			t.Fatal("отклонённая команда оставила долговечную квитанцию", receipt)
		}
	}
}
