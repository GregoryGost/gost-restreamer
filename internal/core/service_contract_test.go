package core_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type configContractRepository struct {
	core.ConfigRepository
	stream      core.Stream
	profile     core.TranscodeProfile
	group       core.TranscodeGroup
	destination core.Destination
	readErr     error
	writeErr    error
	reads       int
	writes      int
	expected    uint64
}

func (r *configContractRepository) Stream(context.Context, core.ID) (core.Stream, error) {
	r.reads++
	return r.stream, r.readErr
}
func (r *configContractRepository) Profile(context.Context, core.ID) (core.TranscodeProfile, error) {
	r.reads++
	return r.profile, r.readErr
}
func (r *configContractRepository) Group(context.Context, core.ID) (core.TranscodeGroup, error) {
	r.reads++
	return r.group, r.readErr
}
func (r *configContractRepository) Destination(context.Context, core.ID) (core.Destination, error) {
	r.reads++
	return r.destination, r.readErr
}
func (r *configContractRepository) InsertStream(_ context.Context, v core.Stream) error {
	r.writes++
	if r.writeErr == nil {
		r.stream = v
	}
	return r.writeErr
}
func (r *configContractRepository) InsertProfile(_ context.Context, v core.TranscodeProfile) error {
	r.writes++
	if r.writeErr == nil {
		r.profile = v
	}
	return r.writeErr
}
func (r *configContractRepository) InsertGroup(_ context.Context, v core.TranscodeGroup) error {
	r.writes++
	if r.writeErr == nil {
		r.group = v
	}
	return r.writeErr
}
func (r *configContractRepository) InsertDestination(_ context.Context, v core.Destination) error {
	r.writes++
	if r.writeErr == nil {
		r.destination = v
	}
	return r.writeErr
}
func (r *configContractRepository) UpdateStream(ctx context.Context, v core.Stream, expected uint64) error {
	r.expected = expected
	return r.InsertStream(ctx, v)
}
func (r *configContractRepository) UpdateProfile(ctx context.Context, v core.TranscodeProfile, expected uint64) error {
	r.expected = expected
	return r.InsertProfile(ctx, v)
}
func (r *configContractRepository) UpdateGroup(ctx context.Context, v core.TranscodeGroup, expected uint64) error {
	r.expected = expected
	return r.InsertGroup(ctx, v)
}
func (r *configContractRepository) UpdateDestination(ctx context.Context, v core.Destination, expected uint64) error {
	r.expected = expected
	return r.InsertDestination(ctx, v)
}

type configContractUOW struct {
	repository configContractRepository
	writes     int
	err        error
}

func (u *configContractUOW) View(context.Context, func(core.Repositories) error) error {
	return errors.New("неожиданное чтение вне транзакции конфигурации")
}
func (u *configContractUOW) Write(ctx context.Context, fn func(core.Repositories) error) error {
	u.writes++
	if err := ctx.Err(); err != nil {
		return err
	}
	if u.err != nil {
		return u.err
	}
	return fn(core.Repositories{Config: &u.repository})
}

func TestServiceRejectsMissingStorageAndInvalidOwner(t *testing.T) {
	if _, err := core.NewService(nil, "owner"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("Service без хранилища создан: %v", err)
	}
	for _, owner := range []string{"", " \t", "owner\x00", string([]byte{0xff})} {
		if _, err := core.NewService(&configContractUOW{}, owner); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("некорректный владелец %q принят: %v", owner, err)
		}
	}
}

func TestConfigurationValidationPrecedesPersistence(t *testing.T) {
	ctx := context.Background()
	u := &configContractUOW{}
	s, err := core.NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"create_stream", func() error { _, err := s.CreateStream(ctx, core.Stream{}); return err }},
		{"update_stream", func() error { _, err := s.UpdateStream(ctx, core.Stream{}, 1); return err }},
		{"create_profile", func() error { _, err := s.CreateProfile(ctx, core.TranscodeProfile{}); return err }},
		{"update_profile", func() error { _, err := s.UpdateProfile(ctx, core.TranscodeProfile{}, 1); return err }},
		{"create_group", func() error { _, err := s.CreateGroup(ctx, core.TranscodeGroup{}); return err }},
		{"update_group", func() error { _, err := s.UpdateGroup(ctx, core.TranscodeGroup{}, 1); return err }},
		{"create_destination", func() error { _, err := s.CreateDestination(ctx, core.Destination{}); return err }},
		{"update_destination", func() error { _, err := s.UpdateDestination(ctx, core.Destination{}, 1); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("некорректная конфигурация принята: %v", err)
			}
			if u.writes != 0 || u.repository.reads != 0 || u.repository.writes != 0 {
				t.Fatal("невалидная конфигурация дошла до хранилища")
			}
		})
	}
}

func TestConfigurationUpdatesPreserveIdentityAndCheckRevision(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"stream", "profile", "group", "destination"} {
		t.Run(kind, func(t *testing.T) {
			for _, outcome := range []string{"success", "revision_conflict", "read_failure", "write_failure", "revision_overflow", "cancelled"} {
				t.Run(outcome, func(t *testing.T) {
					snapshot, output := contractSnapshots(t)
					u := &configContractUOW{repository: configContractRepository{stream: snapshot.Stream, profile: output.Profile, group: output.Group, destination: output.Destination}}
					s, err := core.NewService(u, "owner")
					if err != nil {
						t.Fatal(err)
					}
					wantErr := error(nil)
					expected := uint64(1)
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
						var cancel context.CancelFunc
						callCtx, cancel = context.WithCancel(ctx)
						cancel()
						wantErr = context.Canceled
					}
					before := u.repository
					var actualRecord core.Record
					switch kind {
					case "stream":
						value := before.stream
						value.Name = "Новое имя"
						value.Revision = 99
						value.CreatedAt = value.CreatedAt.Add(-1)
						actual, err := s.UpdateStream(callCtx, value, expected)
						actualRecord = actual.Record
						if !errors.Is(err, wantErr) {
							t.Fatalf("результат UpdateStream: %v, ожидался %v", err, wantErr)
						}
					case "profile":
						value := before.profile
						value.Name = "Новое имя"
						value.Profile.AudioEncoding.BitrateBPS++
						actual, err := s.UpdateProfile(callCtx, value, expected)
						actualRecord = actual.Record
						if !errors.Is(err, wantErr) {
							t.Fatalf("результат UpdateProfile: %v, ожидался %v", err, wantErr)
						}
					case "group":
						value := before.group
						value.Name = "Новое имя"
						actual, err := s.UpdateGroup(callCtx, value, expected)
						actualRecord = actual.Record
						if !errors.Is(err, wantErr) {
							t.Fatalf("результат UpdateGroup: %v, ожидался %v", err, wantErr)
						}
					case "destination":
						value := before.destination
						value.Name = "Новое имя"
						value.Endpoint = "rtmp://other.invalid/live"
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
					records := map[string]core.Record{"stream": before.stream.Record, "profile": before.profile.Record, "group": before.group.Record, "destination": before.destination.Record}
					original := records[kind]
					if actualRecord.ID != original.ID || !actualRecord.CreatedAt.Equal(original.CreatedAt) || actualRecord.Revision != 2 || actualRecord.UpdatedAt.Before(original.UpdatedAt) || actualRecord.Validate() != nil || u.repository.expected != expected || u.repository.writes != 1 {
						t.Fatalf("обновление нарушило идентичность или версию: %+v", actualRecord)
					}
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

func TestStreamPublicationKeyCannotChangeDuringConfigurationUpdate(t *testing.T) {
	snapshot, _ := contractSnapshots(t)
	u := &configContractUOW{repository: configContractRepository{stream: snapshot.Stream}}
	s, err := core.NewService(u, "owner")
	if err != nil {
		t.Fatal(err)
	}
	candidate := snapshot.Stream
	candidate.PublicationKey = "different-synthetic-key"
	if _, err := s.UpdateStream(context.Background(), candidate, snapshot.Stream.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("постоянный ключ заменён: %v", err)
	}
	if u.repository.writes != 0 || u.repository.stream != snapshot.Stream {
		t.Fatal("отказ смены ключа изменил конфигурацию")
	}
}

func TestServiceLiveAudioDefaultAndReplacementCAS(t *testing.T) {
	ctx := context.Background()
	for _, encoding := range []bool{false, true} {
		p := encodedProfile(core.VideoEncoderNVENC, core.RateControlVBR)
		if !encoding {
			p.Encoding = core.EncodingSettings{}
		}
		u := &configContractUOW{}
		s, err := core.NewService(u, "owner")
		if err != nil {
			t.Fatal(err)
		}
		created, err := s.CreateProfile(ctx, core.TranscodeProfile{Name: "Новый", Profile: p})
		if err != nil || created.Profile.LiveAudioMode != core.LiveAudioCopy || u.repository.profile != created || p.LiveAudioMode != "" {
			t.Fatal("Create не материализовал LIVE copy либо изменил вход", err)
		}
		omitted := created
		omitted.Profile.LiveAudioMode = ""
		for _, expected := range []uint64{created.Revision + 1, created.Revision} {
			writes := u.repository.writes
			_, err := s.UpdateProfile(ctx, omitted, expected)
			if expected != created.Revision {
				if !errors.Is(err, core.ErrConflict) {
					t.Fatal("CAS должен предшествовать проверке удаления селектора", err)
				}
			} else {
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
		if _, err := s.UpdateProfile(ctx, created, created.Revision); err != nil {
			t.Fatal("Явный transcode отклонён", err)
		}
	}
}
