package core

import (
	"context"
	"crypto/subtle"
	"errors"
)

// InspectOBS проверяет постоянный key текущего Stream streamID до любых действий запуска.
// В одном View возвращает активную Session и признак её наличия; отсутствие Session
// допустимо, но ошибка чтения Stream или Session передаётся вызывающему. ctx ограничивает чтение.
func (s *Service) InspectOBS(ctx context.Context, streamID ID, key string) (Session, bool, error) {
	// result сохраняет найденный запуск; found отличает отсутствие запуска от отказа чтения.
	var result Session
	var found bool
	// err содержит исход согласованного чтения; переданная функция проверяет ключ до поиска запуска.
	err := s.uow.View(ctx, func(r Repositories) error {
		// Повтор чтения начинает новый результат и не сохраняет запуск прежней попытки.
		result, found = Session{}, false
		// stream содержит текущий постоянный ключ; err сообщает об отказе чтения конфигурации.
		stream, err := r.Config.Stream(ctx, streamID)
		if err != nil {
			return err
		}
		if key == "" || stream.PublicationKey == "" || subtle.ConstantTimeCompare([]byte(stream.PublicationKey), []byte(key)) != 1 {
			return ErrNotReady
		}
		result, err = r.Runtime.ActiveSession(ctx, streamID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return Session{}, false, err
	}
	return result, found, nil
}
