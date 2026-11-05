package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type publicationBoundary struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newPublicationBoundary() publicationBoundary {
	return publicationBoundary{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *publicationBoundary) unblock() { b.once.Do(func() { close(b.release) }) }
func (b *publicationBoundary) hold()    { close(b.entered); <-b.release }

type publicationStore struct {
	*storage.Store
	issued, beforeCommit, afterCommit publicationBoundary
	reject, ambiguous                 bool
}

func (s *publicationStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	var issued, completed bool
	err := s.Store.Write(ctx, func(r core.Repositories) error {
		r.Runtime = publicationRepository{RuntimeRepository: r.Runtime, issued: &issued, completed: &completed}
		if err := callback(r); err != nil {
			return err
		}
		if completed {
			s.beforeCommit.hold()
			if s.reject {
				return core.ErrInvalid
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if issued {
		s.issued.hold()
	}
	if completed {
		// Store.Write уже завершил COMMIT и освободил RuntimeAuthority.
		s.afterCommit.hold()
		if s.ambiguous {
			return context.DeadlineExceeded
		}
	}
	return nil
}

type publicationRepository struct {
	core.RuntimeRepository
	issued, completed *bool
}

func (r publicationRepository) UpdateReceipt(ctx context.Context, receipt core.CommandReceipt, expected uint64) error {
	if err := r.RuntimeRepository.UpdateReceipt(ctx, receipt, expected); err != nil {
		return err
	}
	if receipt.Kind == core.CommandStartSession {
		*r.issued = receipt.Status == core.CommandIssued
		*r.completed = receipt.Status == core.CommandSucceeded
	}
	return nil
}

type publicationFeedResult struct {
	router *media.Router
	err    error
}

func awaitPublication(t *testing.T, ctx context.Context, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func awaitPublicationFeed(t *testing.T, ctx context.Context, result <-chan publicationFeedResult) publicationFeedResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return publicationFeedResult{}
	}
}

func TestManagerPublishesFeedWithDurableStartup(t *testing.T) {
	for _, test := range []struct {
		name                    string
		reject, ambiguous, stop bool
	}{
		{name: "committed"},
		{name: "ambiguous", ambiguous: true},
		{name: "rejected", reject: true},
		{name: "revoked", stop: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newManagerFixture(t, false, nil)
			if err := f.manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			wrapped := &publicationStore{Store: f.store, issued: newPublicationBoundary(), beforeCommit: newPublicationBoundary(), afterCommit: newPublicationBoundary(), reject: test.reject, ambiguous: test.ambiguous}
			service, err := core.NewService(wrapped, f.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			config := f.config
			config.Core = service
			manager, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := manager.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			t.Cleanup(func() { wrapped.issued.unblock(); wrapped.beforeCommit.unblock(); wrapped.afterCommit.unblock() })
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			receipt, err := manager.StartSession(ctx, core.StartSessionRequest{RequestID: "publish-feed", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			awaitPublication(t, ctx, wrapped.issued.entered)
			manager.mu.Lock()
			node := manager.sessions[receipt.SessionID]
			manager.mu.Unlock()
			wrapped.issued.unblock()
			awaitPublication(t, ctx, wrapped.beforeCommit.entered)
			starting, err := service.GetSession(ctx, receipt.SessionID)
			if err != nil || starting.State != core.SessionStarting {
				t.Fatal("до COMMIT нет состояния STARTING", starting, err)
			}
			pending, err := service.GetReceipt(ctx, receipt.ID)
			if err != nil || pending.Status != core.CommandIssued {
				t.Fatal("до COMMIT изменена квитанция", pending, err)
			}
			early := make(chan publicationFeedResult, 1)
			go func() { router, e := manager.Feed(starting); early <- publicationFeedResult{router, e} }()
			wrapped.beforeCommit.unblock()
			if test.reject {
				actual := awaitPublicationFeed(t, ctx, early)
				if actual.router != nil || !errors.Is(actual.err, core.ErrStale) {
					t.Fatal("отклонённый startup допустил Feed", actual)
				}
				f.manager, f.service = manager, service
				if current := waitReceipt(t, f, receipt); current.Status != core.CommandFailed {
					t.Fatal(current)
				}
				return
			}
			awaitPublication(t, ctx, wrapped.afterCommit.entered)
			running, err := service.GetSession(ctx, receipt.SessionID)
			if err != nil || running.State != core.SessionRunning {
				t.Fatal("COMMIT не опубликовал RUNNING", running, err)
			}
			succeeded, err := service.GetReceipt(ctx, receipt.ID)
			if err != nil || succeeded.Status != core.CommandSucceeded {
				t.Fatal("COMMIT не завершил квитанцию", succeeded, err)
			}
			if test.stop {
				if _, err = service.StopSession(ctx, core.SessionCommand{RequestID: "revoke-published", SessionID: receipt.SessionID}); err != nil {
					t.Fatal(err)
				}
			}
			result := make(chan publicationFeedResult, 1)
			// TryLock выбирает расписание, но не определяет ожидаемый результат Feed.
			// Свободный mutex позволяет проверить Feed до возврата Complete; занятый требует параллельного вызова.
			if node.mu.TryLock() {
				node.mu.Unlock()
				router, e := manager.Feed(running)
				result <- publicationFeedResult{router, e}
				wrapped.afterCommit.unblock()
			} else {
				go func() { router, e := manager.Feed(running); result <- publicationFeedResult{router, e} }()
				wrapped.afterCommit.unblock()
			}
			actual := awaitPublicationFeed(t, ctx, result)
			if test.stop {
				if actual.router != nil || !errors.Is(actual.err, core.ErrStale) {
					t.Fatal("отозванный startup допустил Feed", actual)
				}
			} else if actual.err != nil || actual.router == nil {
				t.Fatal("успешный COMMIT ещё не допускает Feed", actual.router, actual.err)
			}
			if actual := awaitPublicationFeed(t, ctx, early); actual.router != nil || !errors.Is(actual.err, core.ErrStale) {
				t.Fatal("STARTING допустил Feed", actual)
			}
		})
	}
}
