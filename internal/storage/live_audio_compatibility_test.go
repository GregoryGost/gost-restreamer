package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет copy только при создании профиля, приоритет CAS, отказ удаления явного селектора и
// неизменность профиля при переименовании.
// t сообщает об ошибках проверяемого сценария.
func TestLiveAudioServiceCreateOnlyDefaultAndStrictCASReplacement(t *testing.T) {
	// s содержит текущее открытие временной базы с текущей схемой.
	s := openTestStore(t)
	// svc предоставляет доменные операции текущего владельца временного Store. err сообщает об
	// ошибке создания доменного Service для текущего владельца Store.
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// mode выбирает режим видео copy либо transcode.
	for _, mode := range []core.GroupMode{core.ModeCopy, core.ModeTranscode} {
		// p содержит полный профиль, проверяемый на сохранение LIVE-аудиорежима.
		p := storageProfile()
		p.Mode = mode
		p.LiveAudioMode = ""
		if mode == core.ModeTranscode {
			p.Format.Video.Codec = core.CodecH264
			p.Format.Video.CodecProfile = "high"
			p.Encoding = core.DefaultEncoding(mode, core.VideoEncoderNVENC, core.RateControlVBR)
		}
		// created содержит новый профиль с назначенным при создании LiveAudioCopy. err сообщает
		// об ошибке сохранения полного именованного профиля.
		created, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Новый профиль", Profile: p})
		if err != nil || created.Profile.LiveAudioMode != core.LiveAudioCopy || created.Profile.Encoding != p.Encoding {
			t.Fatalf("Неверное значение по умолчанию при создании: %+v %v", created, err)
		}
		// missing содержит замену с удалённым явным LiveAudioMode.
		missing := created
		missing.Profile.LiveAudioMode = ""
		if _, err = svc.UpdateProfile(ctx, missing, created.Revision-1); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("Проверка селектора обошла CAS: %v", err)
		}
		if _, err = svc.UpdateProfile(ctx, missing, created.Revision); !errors.Is(err, core.ErrInvalidProfile) {
			t.Fatalf("Удалён явный селектор: %v", err)
		}
		// stored содержит прочитанный профиль для проверки неизменности и отсутствия назначения
		// значений по умолчанию.
		stored, err := svc.GetProfile(ctx, created.ID)
		if err != nil || stored != created {
			t.Fatalf("Отказ изменил строку: %+v %v", stored, err)
		}
		stored.Name = "Новое имя"
		// changed содержит результат переименования при неизменных параметрах профиля.
		changed, err := svc.UpdateProfile(ctx, stored, stored.Revision)
		if err != nil || changed.Profile != created.Profile {
			t.Fatalf("Имя изменило профиль: %+v %v", changed, err)
		}
	}
}
