package rtmp

import (
	"context"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// admit вызывает выбранный порт допуска для streamID/key в пределах ctx. Без Admit
// сохраняет строгую проверку уже работающей Session и получение её Feed.
func (s *Server) admit(ctx context.Context, streamID core.ID, key string) (core.Session, *media.Router, error) {
	if s.opts.Admit != nil {
		return s.opts.Admit(ctx, streamID, key)
	}
	// session содержит уже работающую авторизованную Session; err сообщает об отказе проверки.
	session, err := s.opts.Core.AuthorizeOBS(ctx, streamID, key)
	if err != nil {
		return core.Session{}, nil, err
	}
	// router принадлежит разрешённому запуску; err сохраняет отказ строгого Feed.
	router, err := s.opts.Feed(session)
	return session, router, err
}
