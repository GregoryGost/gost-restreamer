package core

import (
	"context"
)

// PriorStartSession сверяет идентичность запроса до нового выбора файлов и физических проверок.
func (s *Service) PriorStartSession(ctx context.Context, request StartSessionRequest) (CommandReceipt, bool, error) {
	content, err := identity(CommandStartSession, request.StreamID, request.DestinationIDs)
	if err != nil {
		return CommandReceipt{}, false, err
	}
	return s.priorCommand(ctx, request.RequestID, content)
}

// PriorAddOutput возвращает прежнюю квитанцию независимо от текущей конфигурации назначения.
func (s *Service) PriorAddOutput(ctx context.Context, request AddOutputRequest) (CommandReceipt, bool, error) {
	content, err := identity(CommandAddOutput, request.SessionID, []ID{request.DestinationID})
	if err != nil {
		return CommandReceipt{}, false, err
	}
	return s.priorCommand(ctx, request.RequestID, content)
}

func (s *Service) priorCommand(ctx context.Context, requestID string, content []byte) (CommandReceipt, bool, error) {
	var result CommandReceipt
	var found bool
	err := s.uow.View(ctx, func(r Repositories) error {
		var err error
		result, found, err = priorReceipt(ctx, r, requestID, content)
		return err
	})
	return result, found, err
}

// SelectStartBindings выбирает варианты только групп первоначально выбранных назначений.
// StartSession повторно сверяет их в транзакции принятия команды.
func (s *Service) SelectStartBindings(ctx context.Context, streamID ID, destinationIDs []ID) ([]PreparedBinding, error) {
	if _, err := identity(CommandStartSession, streamID, destinationIDs); err != nil {
		return nil, err
	}
	var result []PreparedBinding
	err := s.uow.View(ctx, func(r Repositories) error {
		stream, err := r.Config.Stream(ctx, streamID)
		if err != nil {
			return err
		}
		destinations, err := allDestinations(ctx, r, streamID)
		if err != nil {
			return err
		}
		selected := make(map[ID]bool, len(destinationIDs))
		for _, id := range destinationIDs {
			selected[id] = false
		}
		seen := make(map[ID]bool)
		for _, d := range destinations {
			if len(destinationIDs) == 0 {
				if !d.Enabled {
					continue
				}
			} else {
				if _, ok := selected[d.ID]; !ok {
					continue
				}
				selected[d.ID] = true
			}
			if seen[d.GroupID] {
				continue
			}
			binding, err := selectDestinationBinding(ctx, r, stream.ID, stream.Fallback, d)
			if err != nil {
				return err
			}
			result = append(result, binding)
			seen[d.GroupID] = true
		}
		for _, found := range selected {
			if !found {
				return ErrNotFound
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// SelectAddBinding использует текущие настройки назначения и профиль с политикой Session.
// Перезапуск прикреплённого выхода использует его прежний снимок и этого метода не требует.
func (s *Service) SelectAddBinding(ctx context.Context, sessionID, destinationID ID) (PreparedBinding, error) {
	if err := sessionID.Validate(); err != nil {
		return PreparedBinding{}, err
	}
	if err := destinationID.Validate(); err != nil {
		return PreparedBinding{}, err
	}
	var result PreparedBinding
	err := s.uow.View(ctx, func(r Repositories) error {
		session, err := r.Runtime.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		if session.State != SessionRunning {
			return ErrConflict
		}
		d, err := r.Config.Destination(ctx, destinationID)
		if err != nil {
			return err
		}
		result, err = selectDestinationBinding(ctx, r, session.StreamID, session.Snapshot.Stream.Fallback, d)
		return err
	})
	return result, err
}

func selectDestinationBinding(ctx context.Context, r Repositories, streamID ID, policy FallbackPolicy, d Destination) (PreparedBinding, error) {
	if d.StreamID != streamID {
		return PreparedBinding{}, ErrConflict
	}
	g, err := r.Config.Group(ctx, d.GroupID)
	if err != nil {
		return PreparedBinding{}, err
	}
	if g.StreamID != streamID {
		return PreparedBinding{}, ErrConflict
	}
	p, err := r.Config.Profile(ctx, g.ProfileID)
	if err != nil {
		return PreparedBinding{}, err
	}
	if err = policy.Validate(); err != nil {
		return PreparedBinding{}, ErrNotReady
	}
	a, err := r.Artifacts.Asset(ctx, policy.AssetID)
	if err != nil {
		return PreparedBinding{}, err
	}
	if a.CurrentGenerationID == "" {
		return PreparedBinding{}, ErrNotReady
	}
	generation, err := r.Artifacts.Generation(ctx, a.CurrentGenerationID)
	if err != nil {
		return PreparedBinding{}, err
	}
	for _, required := range generation.RequiredProfiles {
		if required.Profile.Equal(p.Profile) {
			binding := PreparedBinding{GenerationID: generation.ID, GroupID: g.ID, VariantID: required.VariantID, GeneratorVersion: generation.GeneratorVersion}
			if _, err = exactManifest(ctx, r, policy, p.Profile, binding); err != nil {
				return PreparedBinding{}, err
			}
			return binding, nil
		}
	}
	return PreparedBinding{}, ErrNotReady
}
