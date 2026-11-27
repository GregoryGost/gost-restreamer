package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/chat/twitch"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestTwitchReplacementSaveOutcome различает две ситуации после выдачи новой пары.
// При устаревшей ревизии новая пара не записывается. При потере ответа после COMMIT
// публичная попытка не обещает отказ или отмену до подтверждения квитанции.
func TestTwitchReplacementSaveOutcome(t *testing.T) {
	// uncertain выбирает, была ли запись отклонена по ревизии или подтверждение потерялось.
	for _, uncertain := range []bool{false, true} {
		// name определяет, какой результат SQLite наблюдает владелец попытки.
		name := "конфликт ревизии"
		if uncertain {
			name = "неоднозначный COMMIT"
		}
		// Подтест открывает отдельную SQLite и завершает весь опрос до её закрытия.
		t.Run(name, func(t *testing.T) {
			// ctx и cancel ограничивают сетевую работу, сохранение и наблюдение за итогом.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// store и service используют рабочую схему и реальные короткие транзакции.
			store, service := openChatTestStore(t, ctx)
			// initial содержит старые полномочия и ожидаемую ревизию ручной замены.
			initial := createCredentialTestAccount(t, ctx, service, "replacement-before-save")
			// credentials хранит штатный порт и при необходимости имитирует недоступную послезаписную сверку.
			var credentials CredentialStore = service
			if uncertain {
				credentials = &credentialReceiptTestStore{Service: service, unreadable: true}
			}
			// limits задаёт обычные конечные пределы записи и опроса.
			limits := core.DefaultChatLimits()
			// owner использует штатный допуск к аккаунту, err сообщает об отказе подготовки.
			owner, err := NewCredentialOwner(credentials, limits, nil)
			chatTestOK(t, err)
			t.Cleanup(func() {
				// cleanup и stop дают владельцу срок завершиться после остановки Manager.
				cleanup, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				chatTestOK(t, owner.Close(cleanup))
			})
			// adapter отдаёт поля подтверждения и ждёт решения этого теста для первой проверки.
			adapter := &managerTwitchDeviceFixture{
				ctx: ctx, interval: time.Millisecond, ttl: time.Minute, polls: make(chan managerTwitchPoll, 1),
			}
			// manager использует изолированный владелец полномочий вместе с настоящей SQLite.
			manager, err := New(ctx, service, store, limits, []core.ChatAdapterPort{adapter}, owner)
			chatTestOK(t, err)
			t.Cleanup(func() {
				// cleanup и stop позволяют закрыть Manager до зависимых ресурсов даже при отказе.
				cleanup, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				chatTestOK(t, manager.Close(cleanup))
			})
			// attempt показывает границу между началом внешней проверки и записью новой пары.
			attempt, err := manager.ConnectTwitch(ctx, core.ChatAuthorizationRequest{
				Provider: core.ChatTwitch, AccountID: initial.Account.ID,
				ClientID: managerTwitchClientID, ExpectedRevision: initial.Account.Revision,
			})
			chatTestOK(t, err)
			// poll удерживает уже допущенную операцию, пока тест не разрешит подтверждение.
			poll := chatTestReceive(t, ctx, adapter.polls)
			if !uncertain {
				// renamed меняет только конфигурационную ревизию и делает старый CAS непригодным.
				renamed := initial.Account
				renamed.Name = "Переименовано в ожидании входа"
				// err сообщает об отказе создания конкурентного изменения.
				_, err := service.UpdateChatAccount(ctx, renamed, renamed.Revision)
				chatTestOK(t, err)
			}
			// bundle представляет выданную пару, которую Manager попытается принять после проверки.
			bundle := credentialTestBundle("replacement-after-save")
			poll.reply <- twitch.DevicePoll{State: twitch.DeviceComplete, Bundle: &bundle}

			if uncertain {
				// observed получает публичное состояние после того, как существующий owner потерял доказательство COMMIT.
				observed := waitTwitchUnknownSave(t, ctx, manager, attempt.AttemptID)
				if observed.State != core.ChatAuthorizationPending || observed.UserCode != "" || observed.VerificationURI != "" {
					t.Fatalf("Неоднозначная запись раскрыла код или получила окончательный статус: %+v", observed)
				}
				// err проверяет, что неопределённость не превращается в подтверждённую отмену.
				if err := manager.CancelAuthorization(ctx, attempt.AttemptID); !errors.Is(err, ErrCredentialCommitUnknown) {
					t.Fatalf("Отмена после возможного COMMIT вернула %v", err)
				}
			} else {
				waitManagerTwitchAuthorization(t, ctx, manager, attempt.AttemptID, core.ChatAuthorizationFailed)
			}
			// saved читает полную строку настоящей SQLite мимо имитации потери ответа.
			saved, err := service.GetChatCredentialResult(ctx, initial.Account.ID)
			chatTestOK(t, err)
			if uncertain {
				if saved.Credentials.CredentialWriteID != attempt.AttemptID ||
					saved.Credentials.Bundle.Twitch.RefreshToken != bundle.Twitch.RefreshToken {
					t.Fatal("Фикстура не сохранила пару перед потерей подтверждения")
				}
			} else if saved.Credentials.CredentialRevision != initial.Credentials.CredentialRevision ||
				saved.Credentials.CredentialWriteID != initial.Credentials.CredentialWriteID {
				t.Fatal("Устаревший результат изменил действующие полномочия")
			}
		})
	}
}

// waitTwitchUnknownSave ждёт безопасного сигнала о неподтверждённой записи попытки id.
func waitTwitchUnknownSave(t *testing.T, ctx context.Context, manager *Manager, id core.ID) core.ChatAuthorization {
	t.Helper()
	// tick задаёт срок очередного чтения состояния, пока фоновый опрос завершает запись.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		// view и err представляют наблюдаемое состояние попытки и доказанность сохранения.
		view, err := manager.Authorization(ctx, id)
		if errors.Is(err, ErrCredentialCommitUnknown) {
			return view
		}
		chatTestOK(t, err)
		if view.State != core.ChatAuthorizationPending {
			t.Fatalf("Неподтверждённая запись преждевременно завершилась: %+v", view)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
