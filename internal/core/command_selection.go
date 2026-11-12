package core

import (
	"context"
)

// PriorStartSession сверяет идентичность запроса до нового выбора файлов и физических проверок.
func (s *Service) PriorStartSession(ctx context.Context, request StartSessionRequest) (CommandReceipt, bool, error) {
	// content содержит каноническую идентичность запуска, а err — причину отказа проверки его целей.
	content, err := identity(CommandStartSession, request.StreamID, request.DestinationIDs)
	if err != nil {
		return CommandReceipt{}, false, err
	}
	return s.priorCommand(ctx, request.RequestID, content)
}

// PriorAddOutput возвращает прежнюю квитанцию независимо от текущей конфигурации назначения.
func (s *Service) PriorAddOutput(ctx context.Context, request AddOutputRequest) (CommandReceipt, bool, error) {
	// content содержит каноническую идентичность добавления выхода, а err — причину отказа проверки
	// его целей.
	content, err := identity(CommandAddOutput, request.SessionID, []ID{request.DestinationID})
	if err != nil {
		return CommandReceipt{}, false, err
	}
	return s.priorCommand(ctx, request.RequestID, content)
}

// priorCommand читает прежнюю квитанцию по RequestID и сверяет точную идентичность запроса в одной
// читающей транзакции.
func (s *Service) priorCommand(ctx context.Context, requestID string, content []byte) (CommandReceipt, bool, error) {
	// result принимает прежнюю квитанцию тождественного запроса.
	var result CommandReceipt
	// found показывает, найдена ли тождественная прежняя квитанция.
	var found bool
	// err содержит причину отказа согласованного поиска прежней команды.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении ищет прежнюю квитанцию и сверяет её с каноническим содержимым запроса.

		// err передаёт отказ поиска или сравнения прежней квитанции.
		var err error
		result, found, err = priorReceipt(ctx, r, requestID, content)
		return err
	})
	return result, found, err
}

// SelectStartBindings выбирает варианты только групп первоначально выбранных назначений.
// StartSession повторно сверяет их в транзакции принятия команды.
func (s *Service) SelectStartBindings(ctx context.Context, streamID ID, destinationIDs []ID) ([]PreparedBinding, error) {
	// err содержит причину отказа проверки потока и выбранных назначений.
	if _, err := identity(CommandStartSession, streamID, destinationIDs); err != nil {
		return nil, err
	}
	// result накапливает точные привязки вариантов для уникальных групп выбранных назначений.
	var result []PreparedBinding
	// err содержит причину отказа согласованного выбора привязок запуска.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении выбирает точные варианты уникальных групп включённых либо явно перечисленных
		// назначений; отсутствие запрошенного назначения возвращает ErrNotFound.

		// stream содержит текущие настройки запускаемого потока, а err — причину отказа их
		// чтения.
		stream, err := r.Config.Stream(ctx, streamID)
		if err != nil {
			return err
		}
		// destinations содержит все текущие назначения потока для выбора начальных выходов.
		destinations, err := allDestinations(ctx, r, streamID)
		if err != nil {
			return err
		}
		// selected отмечает, найдено ли каждое явно запрошенное назначение.
		selected := make(map[ID]bool, len(destinationIDs))
		// id содержит идентификатор очередного явно запрошенного назначения.
		for _, id := range destinationIDs {
			selected[id] = false
		}
		// seen отмечает группы, для которых вариант уже выбран.
		seen := make(map[ID]bool)
		// d содержит очередное текущее назначение потока для проверки выбора.
		for _, d := range destinations {
			if len(destinationIDs) == 0 {
				if !d.Enabled {
					continue
				}
			} else {
				// ok показывает, входит ли назначение в явно запрошенный набор.
				if _, ok := selected[d.ID]; !ok {
					continue
				}
				selected[d.ID] = true
			}
			if seen[d.GroupID] {
				continue
			}
			// binding содержит проверенный вариант группы назначения, а err — причину отказа
			// его выбора.
			binding, err := selectDestinationBinding(ctx, r, stream.ID, stream.Fallback, d)
			if err != nil {
				return err
			}
			result = append(result, binding)
			seen[d.GroupID] = true
		}
		// found показывает, найдено ли очередное явно запрошенное назначение.
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
	// err содержит причину отказа проверки идентификатора сессии.
	if err := sessionID.Validate(); err != nil {
		return PreparedBinding{}, err
	}
	// err содержит причину отказа проверки идентификатора назначения.
	if err := destinationID.Validate(); err != nil {
		return PreparedBinding{}, err
	}
	// result принимает точную привязку варианта добавляемого выхода.
	var result PreparedBinding
	// err содержит причину отказа согласованного выбора привязки добавляемого выхода.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении требует SessionRunning и выбирает вариант текущего назначения с политикой из
		// снимка Session.

		// session содержит запуск и сохранённую политику заглушки, а err — причину отказа их
		// чтения.
		session, err := r.Runtime.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		if session.State != SessionRunning {
			return ErrConflict
		}
		// d содержит текущие настройки добавляемого назначения.
		d, err := r.Config.Destination(ctx, destinationID)
		if err != nil {
			return err
		}
		result, err = selectDestinationBinding(ctx, r, session.StreamID, session.Snapshot.Stream.Fallback, d)
		return err
	})
	return result, err
}

// selectDestinationBinding проверяет принадлежность назначения и группы Stream, затем выбирает из
// текущего поколения заглушки точный готовый вариант полного профиля.
func selectDestinationBinding(ctx context.Context, r Repositories, streamID ID, policy FallbackPolicy, d Destination) (PreparedBinding, error) {
	if d.StreamID != streamID {
		return PreparedBinding{}, ErrConflict
	}
	// g содержит группу назначения, а err — причину отказа её чтения.
	g, err := r.Config.Group(ctx, d.GroupID)
	if err != nil {
		return PreparedBinding{}, err
	}
	if g.StreamID != streamID {
		return PreparedBinding{}, ErrConflict
	}
	// p содержит полный текущий профиль группы назначения.
	p, err := r.Config.Profile(ctx, g.ProfileID)
	if err != nil {
		return PreparedBinding{}, err
	}
	if err = policy.Validate(); err != nil {
		return PreparedBinding{}, ErrNotReady
	}
	// a содержит заглушку и указатель её текущего поколения.
	a, err := r.Artifacts.Asset(ctx, policy.AssetID)
	if err != nil {
		return PreparedBinding{}, err
	}
	if a.CurrentGenerationID == "" {
		return PreparedBinding{}, ErrNotReady
	}
	// generation содержит опубликованное поколение, в котором ищется точный профиль.
	generation, err := r.Artifacts.Generation(ctx, a.CurrentGenerationID)
	if err != nil {
		return PreparedBinding{}, err
	}
	// required содержит очередной профиль и заранее выбранный вариант поколения.
	for _, required := range generation.RequiredProfiles {
		if required.Profile.Equal(p.Profile) {
			// binding фиксирует поколение, группу, вариант и версию генератора найденного
			// профиля.
			binding := PreparedBinding{GenerationID: generation.ID, GroupID: g.ID, VariantID: required.VariantID, GeneratorVersion: generation.GeneratorVersion}
			if _, err = exactManifest(ctx, r, policy, p.Profile, binding); err != nil {
				return PreparedBinding{}, err
			}
			return binding, nil
		}
	}
	return PreparedBinding{}, ErrNotReady
}
