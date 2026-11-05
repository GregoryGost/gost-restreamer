package session

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

func replaceAdmissionManager(t *testing.T, f managerFixture, uow core.UnitOfWork) managerFixture {
	t.Helper()
	if err := f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := core.NewService(uow, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	config := f.config
	config.Core = service
	f.ingress, f.preparation = &lifecycleOwner{}, &lifecycleOwner{}
	config.Ingress, config.Preparation = f.ingress, f.preparation
	manager, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.manager = manager
	return f
}

func startAdmissionParent(t *testing.T, f managerFixture) core.CommandReceipt {
	t.Helper()
	start, err := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "admission-parent", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	return start
}

func assertOnlyAdmissionOutput(t *testing.T, f managerFixture, receipt core.CommandReceipt) {
	t.Helper()
	outputs, err := f.service.ListOutputs(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: receipt.SessionID}})
	if err != nil || len(outputs.Items) != 1 || outputs.Items[0].ID != receipt.OutputID {
		t.Fatal("повтор команды создал другой выход", outputs, receipt, err)
	}
	receipts, err := f.service.ListReceipts(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: receipt.SessionID, OutputID: receipt.OutputID}})
	if err != nil || len(receipts.Items) != 1 || receipts.Items[0].ID != receipt.ID {
		t.Fatal("повтор команды создал другую квитанцию", receipts, receipt, err)
	}
}

func TestManagerAddPriorPrecedesFreshConfigurationAndFiles(t *testing.T) {
	f := newManagerFixture(t, false, nil)
	parent := startAdmissionParent(t, f)
	request := core.AddOutputRequest{RequestID: "prior-add", SessionID: parent.SessionID, DestinationID: f.destination.ID}
	accepted, err := f.manager.AddOutput(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	original := waitReceipt(t, f, accepted)
	if original.Status != core.CommandSucceeded {
		t.Fatal(original)
	}
	output, err := f.service.GetOutput(context.Background(), accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	profile := output.Snapshot.Profile
	profile.Profile.VideoEncoding.GOPFrames++
	if _, err = f.service.UpdateProfile(context.Background(), profile, profile.Revision); err != nil {
		t.Fatal(err)
	}
	manifest := output.Snapshot.Artifact
	if err = f.files.RemoveExact(context.Background(), manifest.ManagedPath, manifest.SizeBytes, manifest.ContentHash); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.SelectAddBinding(context.Background(), request.SessionID, request.DestinationID); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("новый выбор неожиданно готов", err)
	}
	request.Binding = core.PreparedBinding{GroupID: "untrusted-binding"}
	duplicate, err := f.manager.AddOutput(context.Background(), request)
	if err != nil || !reflect.DeepEqual(duplicate, original) {
		t.Fatal("повтор изменил квитанцию из-за новой конфигурации", duplicate, original, err)
	}
	conflicting := request
	conflicting.DestinationID = "different-destination"
	if _, err = f.manager.AddOutput(context.Background(), conflicting); !errors.Is(err, core.ErrConflict) {
		t.Fatal("RequestID принял другую идентичность", err)
	}
	assertOnlyAdmissionOutput(t, f, accepted)
}

type ambiguousAdmissionStore struct {
	*storage.Store
	requestID string
	injected  atomic.Bool
}

func (s *ambiguousAdmissionStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	var inserted bool
	err := s.Store.Write(ctx, func(r core.Repositories) error {
		r.Runtime = ambiguousAdmissionRepository{RuntimeRepository: r.Runtime, requestID: s.requestID, inserted: &inserted}
		return callback(r)
	})
	if err == nil && inserted && s.injected.CompareAndSwap(false, true) {
		return context.DeadlineExceeded
	}
	return err
}

type ambiguousAdmissionRepository struct {
	core.RuntimeRepository
	requestID string
	inserted  *bool
}

func (r ambiguousAdmissionRepository) InsertReceipt(ctx context.Context, receipt core.CommandReceipt) error {
	if err := r.RuntimeRepository.InsertReceipt(ctx, receipt); err != nil {
		return err
	}
	if receipt.RequestID == r.requestID {
		*r.inserted = true
	}
	return nil
}

func TestManagerAddResolvesAmbiguousAdmissionWithoutDuplicate(t *testing.T) {
	f := newManagerFixture(t, false, nil)
	wrapped := &ambiguousAdmissionStore{Store: f.store, requestID: "ambiguous-add"}
	f = replaceAdmissionManager(t, f, wrapped)
	parent := startAdmissionParent(t, f)
	request := core.AddOutputRequest{RequestID: wrapped.requestID, SessionID: parent.SessionID, DestinationID: f.destination.ID}
	receipt, err := f.manager.AddOutput(context.Background(), request)
	if err != nil || !wrapped.injected.Load() {
		t.Fatal("неоднозначный COMMIT не сверен", receipt, err)
	}
	if current := waitReceipt(t, f, receipt); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	prior, found, err := f.service.PriorAddOutput(context.Background(), request)
	if err != nil || !found || !sameCommand(prior, receipt) {
		t.Fatal("возвращена чужая квитанция", prior, receipt, found, err)
	}
	assertOnlyAdmissionOutput(t, f, receipt)
}

func TestManagerConcurrentAddPriorSurvivesFreshAdmissionFailure(t *testing.T) {
	for _, failure := range []string{"selection", "file"} {
		t.Run(failure, func(t *testing.T) {
			f := newManagerFixture(t, false, nil)
			wrapped := &delayedPriorStore{Store: f.store, requestID: "concurrent-add", entered: make(chan struct{}), release: make(chan struct{})}
			f = replaceAdmissionManager(t, f, wrapped)
			var released sync.Once
			release := func() { released.Do(func() { close(wrapped.release) }) }
			t.Cleanup(release)
			parent := startAdmissionParent(t, f)
			request := core.AddOutputRequest{RequestID: wrapped.requestID, SessionID: parent.SessionID, DestinationID: f.destination.ID}
			type commandResult struct {
				receipt core.CommandReceipt
				err     error
			}
			result := make(chan commandResult, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() { receipt, err := f.manager.AddOutput(ctx, request); result <- commandResult{receipt, err} }()
			awaitPublication(t, ctx, wrapped.entered)
			winner, err := f.manager.AddOutput(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			original := waitReceipt(t, f, winner)
			if original.Status != core.CommandSucceeded {
				t.Fatal(original)
			}
			output, err := f.service.GetOutput(ctx, winner.OutputID)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "selection" {
				profile := output.Snapshot.Profile
				profile.Profile.VideoEncoding.GOPFrames++
				if _, err = f.service.UpdateProfile(ctx, profile, profile.Revision); err != nil {
					t.Fatal(err)
				}
			} else {
				manifest := output.Snapshot.Artifact
				if err = f.files.RemoveExact(ctx, manifest.ManagedPath, manifest.SizeBytes, manifest.ContentHash); err != nil {
					t.Fatal(err)
				}
			}
			release()
			select {
			case actual := <-result:
				if actual.err != nil || !reflect.DeepEqual(actual.receipt, original) {
					t.Fatal("ошибка нового выбора скрыла параллельно принятую команду", actual.receipt, original, actual.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			assertOnlyAdmissionOutput(t, f, winner)
		})
	}
}

func TestManagerRejectsFreshOutputWithoutAdmissionSideEffects(t *testing.T) {
	for _, test := range []struct {
		name string
		want error
	}{
		{name: "missing_destination", want: core.ErrNotFound},
		{name: "missing_file", want: os.ErrNotExist},
		{name: "wrong_generator", want: core.ErrNotReady},
		{name: "canceled", want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newManagerFixture(t, false, func(c *Config) {
				if test.name == "wrong_generator" {
					c.GeneratorVersion = "other-generator"
				}
			})
			parent := startAdmissionParent(t, f)
			request := core.AddOutputRequest{RequestID: "rejected-add", SessionID: parent.SessionID, DestinationID: f.destination.ID}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch test.name {
			case "missing_destination":
				request.DestinationID = "missing-destination"
			case "missing_file":
				variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.files.RemoveExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			if _, err := f.manager.AddOutput(ctx, request); !errors.Is(err, test.want) {
				t.Fatal("неверный отказ допуска", err, test.want)
			}
			if _, found, err := f.service.PriorAddOutput(context.Background(), request); err != nil || found {
				t.Fatal("отклонённая команда оставила квитанцию", found, err)
			}
			outputs, err := f.service.ListOutputs(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
			if err != nil || len(outputs.Items) != 0 {
				t.Fatal("отклонённая команда оставила выход", outputs, err)
			}
			parentState, err := f.service.GetSession(context.Background(), parent.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.manager.Feed(parentState); err != nil {
				t.Fatal("отказ допуска отозвал здоровую Session", err)
			}
		})
	}
}

func TestManagerCloseWaitsForEnteredAdmissionAndRejectsItsCommit(t *testing.T) {
	for _, kind := range []core.CommandKind{core.CommandStartSession, core.CommandAddOutput} {
		t.Run(string(kind), func(t *testing.T) {
			f := newManagerFixture(t, false, nil)
			wrapped := &delayedPriorStore{Store: f.store, requestID: "closing-admission", entered: make(chan struct{}), release: make(chan struct{})}
			f = replaceAdmissionManager(t, f, wrapped)
			var released sync.Once
			release := func() { released.Do(func() { close(wrapped.release) }) }
			t.Cleanup(release)
			var parent core.CommandReceipt
			if kind == core.CommandAddOutput {
				parent = startAdmissionParent(t, f)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			admitted := make(chan error, 1)
			go func() {
				var err error
				if kind == core.CommandStartSession {
					_, err = f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: wrapped.requestID, StreamID: f.stream.ID})
				} else {
					_, err = f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: wrapped.requestID, SessionID: parent.SessionID, DestinationID: f.destination.ID})
				}
				admitted <- err
			}()
			awaitPublication(t, ctx, wrapped.entered)
			closed := make(chan error, 1)
			go func() { closed <- f.manager.Close(ctx) }()
			waitRetirementCondition(t, ctx, f.preparation.closed.Load)
			select {
			case err := <-closed:
				t.Fatal("Close завершился до ранее вошедшего обращения", err)
			default:
			}
			release()
			select {
			case err := <-admitted:
				if !errors.Is(err, core.ErrClosed) && (kind != core.CommandAddOutput || !errors.Is(err, core.ErrConflict)) {
					t.Fatal("закрывшийся Manager разрешил позднее принятие", err)
				}
			case <-ctx.Done():
				t.Fatal("обращение не завершилось после закрытия допуска", ctx.Err())
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("Close не дождался завершившегося допуска", ctx.Err())
			}
			if err := f.store.View(ctx, func(r core.Repositories) error {
				_, err := r.Runtime.Receipt(ctx, wrapped.requestID)
				if !errors.Is(err, core.ErrNotFound) {
					t.Fatal("позднее обращение оставило квитанцию", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
			if err != nil || len(outputs.Items) != 0 {
				t.Fatal("позднее обращение оставило выход", outputs, err)
			}
		})
	}
}
