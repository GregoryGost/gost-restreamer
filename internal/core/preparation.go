package core

import (
	"bytes"
	"context"
	"fmt"
	"sort"
)

// preparationLimits получает явно заданные бюджеты подготовки от UnitOfWork и проверяет их
// допустимость.
func (s *Service) preparationLimits() (PreparationLimits, error) {
	// p предоставляет общий бюджет подготовки хранилища, а ok показывает наличие этой возможности.
	p, ok := s.uow.(PreparationLimitsProvider)
	if !ok {
		return PreparationLimits{}, ErrRuntimeCapability
	}
	// l содержит явные общие пределы очереди подготовки и числа профилей.
	l := p.PreparationLimits()
	return l, l.Validate()
}

// CreateAsset создаёт стабильную заглушку в PREPARING без содержимого и выбранного поколения.
func (s *Service) CreateAsset(ctx context.Context, name string) (FallbackAsset, error) {
	// record содержит новую идентичность и временные метки сохраняемой записи. err сообщает об
	// ошибке создания новой сохраняемой записи.
	record, err := newRecord()
	if err != nil {
		return FallbackAsset{}, err
	}
	// a содержит стабильную заглушку и её указатели поколений.
	a := FallbackAsset{Record: record, Name: name, State: AssetPreparing}
	if err = a.Validate(); err != nil {
		return FallbackAsset{}, err
	}
	err = s.uow.Write(ctx, func(r Repositories) error {
		// Вставляет стабильный FallbackAsset до появления содержимого поколения.
		return r.Artifacts.InsertAsset(ctx, a)
	})
	return a, err
}

// requiredProfiles собирает уникальные канонические профили всех групп Stream, использующих Asset,
// ограничивает их число и сортирует по каноническим байтам.
func requiredProfiles(ctx context.Context, r Repositories, assetID ID, limit int) ([]RequiredProfile, error) {
	// result накапливает уникальные полные профили всех потоков выбранной заглушки.
	result := make([]RequiredProfile, 0)
	// seen отмечает встреченные канонические профили для исключения дублей.
	seen := map[string]bool{}
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей подготовки.
	q := ListQuery{Limit: MaxListLimit, AssetID: assetID}
	for {
		// streams содержит страницу потоков, использующих выбранную заглушку. err сообщает об
		// ошибке чтения потоков выбранной заглушки.
		streams, err := r.Config.Streams(ctx, q)
		if err != nil {
			return nil, err
		}
		// stream содержит очередной поток, использующий выбранную заглушку.
		for _, stream := range streams {
			// groups содержит все группы очередного использующего заглушку потока. err
			// сообщает об ошибке чтения всех групп потока.
			groups, err := allGroups(ctx, r, stream.ID)
			if err != nil {
				return nil, err
			}
			// group содержит очередную группу для включения её полного профиля в подготовку.
			for _, group := range groups {
				// p содержит полный профиль группы, требуемый для подготовки. err
				// сообщает об ошибке чтения полного профиля группы.
				p, err := r.Config.Profile(ctx, group.ProfileID)
				if err != nil {
					return nil, err
				}
				// key содержит ключ требуемого профиля, а b — его канонические байты для
				// проверки уникальности.
				key, b, err := ProfileIdentity(p.Profile)
				if err != nil {
					return nil, err
				}
				if !seen[string(b)] {
					if len(result) >= limit {
						return nil, fmt.Errorf("%w: превышен бюджет профилей", ErrBusy)
					}
					seen[string(b)] = true
					// canonical содержит восстановленный из канонических байтов
					// полный профиль. err сообщает об ошибке восстановления
					// канонического профиля.
					canonical, err := DecodeProfile(b)
					if err != nil {
						return nil, err
					}
					result = append(result, RequiredProfile{Profile: canonical, ProfileKey: key, CanonicalProfile: b})
				}
			}
		}
		if len(streams) < q.Limit {
			break
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := streams[len(streams)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	sort.Slice(result, func(i, j int) bool {
		// Упорядочивает требуемые профили лексикографически по каноническим байтам для последующей точной
		// сверки полного набора.
		return bytes.Compare(result[i].CanonicalProfile, result[j].CanonicalProfile) < 0
	})
	return result, nil
}

// runtimeIdleForPreparation запрещает подготовку при активной сессии, выданных ресурсах или
// неподтверждённой очистке ресурсов исполнения любого прежнего запуска.
func runtimeIdleForPreparation(ctx context.Context, r Repositories) error {
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей подготовки.
	q := ListQuery{Limit: MaxListLimit}
	for {
		// sessions содержит страницу запусков для проверки отсутствия эфира и выданных ресурсов.
		// err сообщает об ошибке чтения страницы запусков.
		sessions, err := r.Runtime.Sessions(ctx, q)
		if err != nil {
			return err
		}
		// session содержит очередной запуск для проверки допустимости подготовки.
		for _, session := range sessions {
			if session.State.Active() || session.Issued || session.CleanupPending {
				return ErrInUse
			}
		}
		if len(sessions) < q.Limit {
			break
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := sessions[len(sessions)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	q = ListQuery{Limit: MaxListLimit, PendingOnly: true}
	for {
		// intents содержит страницу неподтверждённых намерений очистки. err сообщает об ошибке
		// чтения страницы намерений очистки.
		intents, err := r.Artifacts.Cleanups(ctx, q)
		if err != nil {
			return err
		}
		// i содержит очередное неподтверждённое намерение очистки для поиска ресурсов
		// исполнения.
		for _, i := range intents {
			if i.Kind == CleanupRuntime {
				return ErrNotReady
			}
		}
		if len(intents) < q.Limit {
			break
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := intents[len(intents)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	q = ListQuery{Limit: MaxListLimit}
	for {
		// outputs содержит страницу выходов для проверки выданных ресурсов и незавершённой
		// очистки. err сообщает об ошибке чтения страницы выходов.
		outputs, err := r.Runtime.Outputs(ctx, q)
		if err != nil {
			return err
		}
		// output содержит очередной выход для проверки допустимости подготовки.
		for _, output := range outputs {
			if output.Issued || output.CleanupPending {
				return ErrNotReady
			}
		}
		if len(outputs) < q.Limit {
			break
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := outputs[len(outputs)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}

	return nil
}

// preparationIdleForRuntime запрещает новый эфир при QUEUED/CLAIMED/ISSUED-подготовке текущей
// incarnation; терминальные остатки слот запуска не занимают.
func preparationIdleForRuntime(ctx context.Context, r Repositories, incarnation string) error {
	if r.Artifacts == nil {
		return ErrRuntimeCapability
	}
	// Терминальные остатки сохраняют удержания, но не занимают допуск нового эфира.
	// state выбирает очередное активное состояние подготовки, несовместимое с новым эфиром.
	for _, state := range []PreparationState{PreparationQueued, PreparationClaimed, PreparationIssued} {
		// gs содержит страницу поколений, отобранных по текущим условиям проверки или
		// восстановления. err сообщает об ошибке чтения сохраняемых поколений подготовки.
		gs, err := r.Artifacts.Generations(ctx, ListQuery{Limit: 1, State: string(state), Incarnation: incarnation})
		if err != nil {
			return err
		}
		if len(gs) > 0 {
			return ErrNotReady
		}
	}
	return nil
}

// ReservePreparation сохраняет новое ожидающее поколение, полный набор требуемых профилей и
// происхождение будущих файлов при ожидаемой ревизии Asset. Прежнее текущее поколение сохраняется; при
// Rebuild записывается ссылка на его оригинал.
func (s *Service) ReservePreparation(ctx context.Context, request PreparationRequest) (FallbackGeneration, error) {
	// limits содержит проверенные общие пределы очереди и числа профилей. err сообщает об ошибке
	// проверки общих бюджетов подготовки.
	limits, err := s.preparationLimits()
	if err != nil {
		return FallbackGeneration{}, err
	}
	if _, err = s.runtimeAuthority(); err != nil {
		return FallbackGeneration{}, err
	}
	if err = validText("generator", request.GeneratorVersion); err != nil {
		return FallbackGeneration{}, err
	}
	// record содержит новую идентичность и временные метки сохраняемой записи.
	record, err := newRecord()
	if err != nil {
		return FallbackGeneration{}, err
	}
	// token различает конкретное владение новой подготовкой.
	token, err := newID()
	if err != nil {
		return FallbackGeneration{}, err
	}
	// result принимает зарезервированное новое поколение с полным набором профилей и путей.
	var result FallbackGeneration
	err = s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции проверяет отсутствие исполнения и бюджет очереди, резервирует новое поколение,
		// уникальные пути и журнал файлов, затем назначает PendingGenerationID у Asset.

		// e сообщает об ошибке проверки отсутствия эфира и неочищенных ресурсов.
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		// a содержит стабильную заглушку и её указатели поколений. e сообщает об ошибке чтения
		// заглушки и указателей поколений.
		a, e := r.Artifacts.Asset(ctx, request.AssetID)
		if e != nil {
			return e
		}
		if a.Revision != request.ExpectedRevision {
			return ErrConflict
		}
		if a.PendingGenerationID != "" {
			return ErrConflict
		}
		// queued содержит незавершённые подготовки для проверки очереди и исключительного слота.
		queued, e := r.Artifacts.Generations(ctx, ListQuery{Limit: limits.MaxQueued, PendingOnly: true})
		if e != nil {
			return e
		}
		// pending содержит очередную незавершённую подготовку для проверки владельца и очистки.
		for _, pending := range queued {
			if pending.Incarnation != s.incarnation && (pending.Issued || pending.CleanupPending) {
				return ErrNotReady
			}
		}
		if len(queued) >= limits.MaxQueued {
			return ErrBusy
		}
		// profiles содержит полный уникальный набор канонических профилей нового поколения.
		profiles, e := requiredProfiles(ctx, r, a.ID, limits.MaxProfiles)
		if e != nil {
			return e
		}
		// g содержит новое QUEUED-поколение с уникальными путями оригинала и staging.
		g := FallbackGeneration{Record: record, AssetID: a.ID, Kind: request.Kind, GeneratorVersion: request.GeneratorVersion, Incarnation: s.incarnation, AttemptToken: string(token), State: PreparationQueued, RequiredProfiles: profiles, OriginalPath: GenerationDirectory(record.ID) + "/original", StagingPath: GenerationDirectory(record.ID) + "/original.stage"}
		if request.Rebuild {
			if a.CurrentGenerationID == "" {
				return ErrNotReady
			}
			// current содержит опубликованное поколение, оригинал которого используется для
			// rebuild. e сообщает об ошибке чтения точного поколения подготовки.
			current, e := r.Artifacts.Generation(ctx, a.CurrentGenerationID)
			if e != nil {
				return e
			}
			if current.State != PreparationPublished {
				return ErrNotReady
			}
			g.Kind = current.Kind
			g.RebuildFromGenerationID = current.ID
		}
		// i указывает требуемый профиль, которому назначаются новый вариант и уникальные пути.
		for i := range g.RequiredProfiles {
			// id содержит заранее выделенный идентификатор создаваемого варианта. e сообщает
			// об ошибке создания идентификатора варианта.
			id, e := newID()
			if e != nil {
				return e
			}
			g.RequiredProfiles[i].VariantID = id
			g.RequiredProfiles[i].ManagedPath = GenerationDirectory(g.ID) + "/" + string(id) + ".mp4"
			g.RequiredProfiles[i].StagingPath = g.RequiredProfiles[i].ManagedPath + ".stage"
		}
		if e = g.Validate(); e != nil {
			return e
		}
		if e = r.Artifacts.InsertGeneration(ctx, g); e != nil {
			return e
		}
		// old сохраняет ожидаемую ревизию изменяемой записи поколения, заглушки или файла.
		old := a.Revision
		a.PendingGenerationID = g.ID
		a.Record, e = revise(a.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateAsset(ctx, a, old); e != nil {
			return e
		}
		// files содержит принадлежащие поколению пути из журнала файлов.
		files := []OwnedFile{{ManagedPath: GenerationDirectory(g.ID), Kind: OwnedDirectory}, {ManagedPath: g.OriginalPath, Kind: OwnedOriginal}, {ManagedPath: g.StagingPath, Kind: OwnedStaging}}
		// p содержит требуемый профиль с окончательным и промежуточным путями варианта.
		for _, p := range g.RequiredProfiles {
			files = append(files, OwnedFile{ManagedPath: p.ManagedPath, Kind: OwnedVariant}, OwnedFile{ManagedPath: p.StagingPath, Kind: OwnedStaging})
		}
		// f содержит очередную запись владения путём поколения.
		for _, f := range files {
			f.Record, e = newRecord()
			if e != nil {
				return e
			}
			f.AssetID = a.ID
			f.GenerationID = g.ID
			if e = r.Artifacts.InsertOwnedFile(ctx, f); e != nil {
				return e
			}
		}
		result = g
		return nil
	})
	return result, err
}

// preparationFenced читает поколение и требует точного совпадения его PreparationFence.
func preparationFenced(ctx context.Context, r Repositories, f PreparationFence) (FallbackGeneration, error) {
	// g содержит точное поколение текущей операции подготовки. err сообщает об ошибке чтения точного
	// поколения подготовки.
	g, err := r.Artifacts.Generation(ctx, f.GenerationID)
	if err != nil {
		return g, err
	}
	if PreparationFenceFor(g) != f {
		return g, ErrStale
	}
	return g, nil
}

// ClaimPreparation захватывает самую раннюю по CreatedAt и ID QUEUED-подготовку текущей incarnation,
// увеличивая попытку; эфир и незавершённая выданная подготовка блокируют захват.
func (s *Service) ClaimPreparation(ctx context.Context, id ID) (PreparationFence, error) {
	// result принимает точную идентичность успешно захваченной подготовки.
	var result PreparationFence
	// err сообщает об ошибке транзакционного захвата подготовки.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции допускает захват только своей QUEUED-подготовки с учётом порядка очереди и занятых
		// исполнений, увеличивает Attempt и сохраняет CLAIMED.

		// e сообщает об ошибке проверки отсутствия эфира и неочищенных ресурсов.
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		// g содержит точное поколение текущей операции подготовки. e сообщает об ошибке чтения
		// точного поколения подготовки.
		g, e := r.Artifacts.Generation(ctx, id)
		if e != nil {
			return e
		}
		if g.State != PreparationQueued || g.Incarnation != s.incarnation {
			return ErrStale
		}
		// queued содержит незавершённые подготовки для проверки очереди и исключительного слота.
		queued, e := r.Artifacts.Generations(ctx, ListQuery{Limit: MaxListLimit, PendingOnly: true})
		if e != nil {
			return e
		}
		// other содержит очередную конкурирующую подготовку для проверки занятого слота и
		// порядка очереди.
		for _, other := range queued {
			if other.ID != g.ID && (other.State == PreparationClaimed || other.Issued || other.CleanupPending) {
				return ErrBusy
			}
			if other.State == PreparationQueued && (other.CreatedAt.Before(g.CreatedAt) || other.CreatedAt.Equal(g.CreatedAt) && other.ID < g.ID) {
				return ErrBusy
			}
		}
		// old сохраняет ожидаемую ревизию изменяемой записи поколения, заглушки или файла.
		old := g.Revision
		g.State = PreparationClaimed
		g.Attempt, e = NextRevision(g.Attempt)
		if e != nil {
			return e
		}
		g.Record, e = revise(g.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateGeneration(ctx, g, old); e != nil {
			return e
		}
		result = PreparationFenceFor(g)
		return nil
	})
	return result, err
}

// MarkPreparationIssued перед внешними действиями переводит точную CLAIMED-попытку в ISSUED и
// сохраняет необходимость очистки.
func (s *Service) MarkPreparationIssued(ctx context.Context, f PreparationFence) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет точный PreparationFence, отсутствие эфира и конкурирующей подготовки, затем
		// сохраняет ISSUED и CleanupPending до внешнего действия.

		// e сообщает об ошибке проверки отсутствия эфира и неочищенных ресурсов.
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		// g содержит точное поколение текущей операции подготовки. e сообщает об ошибке проверки
		// точного владельца и попытки подготовки.
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || g.State != PreparationClaimed {
			return ErrStale
		}
		// other содержит незавершённые конкурирующие подготовки для проверки слота.
		other, e := r.Artifacts.Generations(ctx, ListQuery{Limit: MaxListLimit, PendingOnly: true})
		if e != nil {
			return e
		}
		// v содержит конкурирующую подготовку для проверки выданной работы и очистки.
		for _, v := range other {
			if v.ID != g.ID && (v.Issued || v.State == PreparationClaimed || v.CleanupPending) {
				return ErrBusy
			}
		}
		// old сохраняет ожидаемую ревизию изменяемой записи поколения, заглушки или файла.
		old := g.Revision
		g.State = PreparationIssued
		g.Issued = true
		g.CleanupPending = true
		g.Record, e = revise(g.Record)
		if e != nil {
			return e
		}
		return r.Artifacts.UpdateGeneration(ctx, g, old)
	})
}

// RegisterPreparationFile сохраняет происхождение дополнительного пути только для точной
// ISSUED-подготовки текущей incarnation; владельцы и новая запись назначаются здесь.
func (s *Service) RegisterPreparationFile(ctx context.Context, f PreparationFence, value OwnedFile) (OwnedFile, error) {
	// result принимает сохранённую дополнительную запись владения путём подготовки.
	var result OwnedFile
	// err сообщает об ошибке регистрации дополнительного файла в точной подготовке.
	err := s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции сверяет свою выданную подготовку, назначает файлу точные AssetID и GenerationID и
		// сохраняет проверенную запись журнала файлов.

		// g содержит точное поколение текущей операции подготовки. e сообщает об ошибке проверки
		// точного владельца и попытки подготовки.
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || g.State != PreparationIssued || !g.Issued {
			return ErrStale
		}
		value.AssetID = g.AssetID
		value.GenerationID = g.ID
		value.Record, e = newRecord()
		if e != nil {
			return e
		}
		if e = value.Validate(); e != nil {
			return e
		}
		if e = r.Artifacts.InsertOwnedFile(ctx, value); e != nil {
			return e
		}
		result = value
		return nil
	})
	return result, err
}

// PublishPreparation атомарно публикует полный проверенный комплект точного поколения и переключает
// Asset на него. Изменение требуемых профилей, неполный набор или несовпадение вариантов откатывают
// публикацию; признаки выданной работы снимаются отдельной очисткой.
func (s *Service) PublishPreparation(ctx context.Context, f PreparationFence, p GenerationPublication) (FallbackGeneration, error) {
	// limits содержит проверенные общие пределы очереди и числа профилей. err сообщает об ошибке
	// проверки общих бюджетов подготовки.
	limits, err := s.preparationLimits()
	if err != nil {
		return FallbackGeneration{}, err
	}
	// result принимает опубликованное поколение после атомарной проверки полного комплекта.
	var result FallbackGeneration
	err = s.uow.Write(ctx, func(r Repositories) error {
		// В одной транзакции повторно сверяет точный набор профилей, варианты и журнал файлов, затем публикует
		// полное поколение и меняет CurrentGenerationID; ошибка не публикует частичный комплект.

		// e сообщает об ошибке проверки отсутствия эфира и неочищенных ресурсов.
		if e := runtimeIdleForPreparation(ctx, r); e != nil {
			return e
		}
		// g содержит точное поколение текущей операции подготовки. e сообщает об ошибке проверки
		// точного владельца и попытки подготовки.
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || g.State != PreparationIssued || !g.Issued {
			return ErrStale
		}
		// a содержит стабильную заглушку и её указатели поколений.
		a, e := r.Artifacts.Asset(ctx, g.AssetID)
		if e != nil {
			return e
		}
		if a.PendingGenerationID != g.ID {
			return ErrStale
		}
		// current содержит заново вычисленный набор требуемых профилей перед публикацией.
		current, e := requiredProfiles(ctx, r, g.AssetID, limits.MaxProfiles)
		if e != nil {
			return e
		}
		if len(current) != len(g.RequiredProfiles) || len(p.Variants) != len(current) {
			return ErrConflict
		}
		// i указывает профиль в текущем и зарезервированном наборах для точного сравнения.
		for i := range current {
			if current[i].ProfileKey != g.RequiredProfiles[i].ProfileKey || !bytes.Equal(current[i].CanonicalProfile, g.RequiredProfiles[i].CanonicalProfile) {
				return ErrConflict
			}
		}
		g.SourceHash = p.SourceHash
		g.SizeBytes = p.SizeBytes
		g.SourceFormat = p.SourceFormat
		g.State = PreparationPublished
		if e = g.Validate(); e != nil {
			return e
		}
		// generationRevision сохраняет ожидаемую ревизию поколения до публикации.
		generationRevision := g.Revision
		g.Record, e = revise(g.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateGeneration(ctx, g, generationRevision); e != nil {
			return e
		}
		// seen отмечает опубликованные VariantID для запрета повторов в комплекте.
		seen := map[ID]bool{}
		// v содержит очередной представленный готовый вариант для сверки с требуемым профилем.
		for _, v := range p.Variants {
			// required указывает требуемый профиль и зарезервированный путь представленного
			// варианта.
			var required *RequiredProfile
			// i указывает очередной зарезервированный профиль при поиске VariantID.
			for i := range g.RequiredProfiles {
				if g.RequiredProfiles[i].VariantID == v.ID {
					required = &g.RequiredProfiles[i]
					break
				}
			}
			if required == nil || seen[v.ID] || !v.Profile.Equal(required.Profile) || v.ManagedPath != required.ManagedPath || v.GenerationID != g.ID || v.AssetID != g.AssetID || v.GeneratorVersion != g.GeneratorVersion || v.SourceHash != g.SourceHash {
				return ErrConflict
			}
			seen[v.ID] = true
			v.Record = Record{ID: v.ID, Revision: 1, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
			v.Incarnation = g.Incarnation
			v.AttemptToken = g.AttemptToken
			v.State = VariantReady
			v.ProfileKey = required.ProfileKey
			v.CanonicalProfile = bytes.Clone(required.CanonicalProfile)
			if e = v.VerifiedFormat.Validate(); e != nil {
				return e
			}
			if CompatibleFormat(v.VerifiedFormat, v.Profile.Format) != nil {
				return ErrInvalid
			}
			if e = v.Validate(); e != nil {
				return e
			}
			if e = r.Artifacts.InsertVariant(ctx, v); e != nil {
				return e
			}
		}
		if e = completeGenerationLedger(ctx, r, g, p.Variants); e != nil {
			return e
		}
		// old сохраняет ожидаемую ревизию изменяемой записи поколения, заглушки или файла.
		old := a.Revision
		a.CurrentGenerationID = g.ID
		a.PendingGenerationID = ""
		projectGeneration(&a, g)
		a.Record, e = revise(a.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateAsset(ctx, a, old); e != nil {
			return e
		}
		result = g
		return nil
	})
	return result, err
}

// projectGeneration обновляет совместимую проекцию исходных метаданных Asset из опубликованного
// поколения, не меняя указатели поколений и Record.
func projectGeneration(a *FallbackAsset, g FallbackGeneration) {
	a.Kind = g.Kind
	a.OriginalPath = g.OriginalPath
	a.SourceHash = g.SourceHash
	a.SizeBytes = g.SizeBytes
	a.SourceFormat = g.SourceFormat
	a.State = AssetReady
	a.Incarnation = g.Incarnation
	a.AttemptToken = g.AttemptToken
}

// CancelPreparation завершает точную активную подготовку текущего владельца отказом с непустой
// причиной; выданная работа остаётся ожидающей очистки.
func (s *Service) CancelPreparation(ctx context.Context, f PreparationFence, reason string) error {
	// err сообщает об ошибке проверки обязательной причины отмены подготовки.
	if err := validText("reason", reason); err != nil {
		return err
	}
	return s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции принимает отмену только своей активной подготовки; выданное исполнение сохраняет
		// признаки и удержание очистки.

		// g содержит точное поколение текущей операции подготовки. e сообщает об ошибке проверки
		// точного владельца и попытки подготовки.
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if f.Incarnation != s.incarnation || !g.State.Active() {
			return ErrStale
		}
		return failPreparation(ctx, r, g, PreparationFailed, reason)
	})
}

// failPreparation сохраняет терминальное состояние и причину, снимая ожидающий указатель Asset сразу
// только у невыданного поколения.
func failPreparation(ctx context.Context, r Repositories, g FallbackGeneration, state PreparationState, reason string) error {
	// old сохраняет ожидаемую ревизию изменяемой записи поколения, заглушки или файла.
	old := g.Revision
	g.State = state
	g.Failure = reason
	// err передаёт результат обновления терминального поколения и его служебной записи.
	var err error
	g.Record, err = revise(g.Record)
	if err != nil {
		return err
	}
	if err = r.Artifacts.UpdateGeneration(ctx, g, old); err != nil {
		return err
	}
	if !g.Issued {
		return releasePendingPreparation(ctx, r, g)
	}
	return nil
}

// releasePendingPreparation снимает только указатель на указанное ожидающее поколение; без прежнего
// текущего поколения Asset становится FAILED.
func releasePendingPreparation(ctx context.Context, r Repositories, g FallbackGeneration) error {
	// a содержит стабильную заглушку и её указатели поколений. e сообщает об ошибке чтения заглушки
	// и указателей поколений.
	a, e := r.Artifacts.Asset(ctx, g.AssetID)
	if e != nil {
		return e
	}
	if a.PendingGenerationID != g.ID {
		return nil
	}
	// old сохраняет ожидаемую ревизию изменяемой записи поколения, заглушки или файла.
	old := a.Revision
	a.PendingGenerationID = ""
	if a.CurrentGenerationID == "" {
		a.State = AssetFailed
	}
	a.Record, e = revise(a.Record)
	if e != nil {
		return e
	}
	return r.Artifacts.UpdateAsset(ctx, a, old)
}

// AcknowledgePreparationCleanup снимает Issued/CleanupPending терминального поколения только после
// отзыва RAM-владельца и завершения его операций; неуспешное поколение ставится на файловую очистку.
func (s *Service) AcknowledgePreparationCleanup(ctx context.Context, f PreparationFence) error {
	// a содержит общую RuntimeAuthority для проверки RAM-владельца и незавершённых операций
	// подготовки. err сообщает об ошибке её получения.
	a, err := s.runtimeAuthority()
	if err != nil {
		return err
	}
	// Write удерживает RuntimeAuthority, поэтому проверка регистраций и запись неделимы.
	return s.uow.Write(ctx, func(r Repositories) error {
		// Под общей блокировкой требует терминальное поколение, отсутствие действующего владельца и
		// незавершённых регистраций, затем снимает Issued и CleanupPending и ставит очистку неопубликованных
		// файлов.

		// g содержит точное поколение текущей операции подготовки. e сообщает об ошибке проверки
		// точного владельца и попытки подготовки.
		g, e := preparationFenced(ctx, r, f)
		if e != nil {
			return e
		}
		if !g.State.Terminal() {
			return ErrConflict
		}
		// owner содержит зарегистрированного RAM-владельца с числом незавершённых операций
		// подготовки.
		if owner := a.preparations[g.ID]; owner != nil {
			if owner.valid.Load() || owner.operations != 0 {
				return ErrInUse
			}
		}
		if !g.CleanupPending && !g.Issued {
			return nil
		}
		// old сохраняет ожидаемую ревизию изменяемой записи поколения, заглушки или файла.
		old := g.Revision
		g.Issued = false
		g.CleanupPending = false
		g.Record, e = revise(g.Record)
		if e != nil {
			return e
		}
		if e = r.Artifacts.UpdateGeneration(ctx, g, old); e != nil {
			return e
		}
		if g.State != PreparationPublished {
			if e = releasePendingPreparation(ctx, r, g); e != nil {
				return e
			}
			if e = queueGenerationCleanup(ctx, r, g); e != nil {
				return e
			}
		}
		return nil
	})
}

// queueGenerationCleanup создаёт отсутствующие намерения очистки для всех записей происхождения
// поколения, включая временные файлы и каталог; существующие намерения по FileID не дублируются.
func queueGenerationCleanup(ctx context.Context, r Repositories, g FallbackGeneration) error {
	// existing отмечает FileID, для которых намерение физической очистки уже сохранено.
	existing := map[ID]bool{}
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей подготовки.
	q := ListQuery{Limit: MaxListLimit, GenerationID: g.ID}
	for {
		// page содержит страницу ранее сохранённых намерений очистки поколения. err сообщает об
		// ошибке чтения страницы намерений очистки.
		page, err := r.Artifacts.Cleanups(ctx, q)
		if err != nil {
			return err
		}
		// i содержит очередное намерение очистки для учёта уже зарегистрированного FileID.
		for _, i := range page {
			if i.Kind == CleanupFile {
				existing[i.FileID] = true
			}
		}
		if len(page) < q.Limit {
			break
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := page[len(page)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	q = ListQuery{Limit: MaxListLimit, GenerationID: g.ID}
	for {
		// files содержит принадлежащие поколению пути из журнала файлов. e сообщает об ошибке
		// чтения журнала файлов поколения.
		files, e := r.Artifacts.OwnedFiles(ctx, q)
		if e != nil {
			return e
		}
		// f содержит очередную запись владения путём поколения.
		for _, f := range files {
			if existing[f.ID] {
				continue
			}
			// record содержит новую идентичность и временные метки сохраняемой записи. e
			// сообщает об ошибке создания новой сохраняемой записи.
			record, e := newRecord()
			if e != nil {
				return e
			}
			if e = r.Artifacts.InsertCleanup(ctx, CleanupIntent{Record: record, Kind: CleanupFile, ArtifactGenerationID: g.ID, FileID: f.ID, FileKind: f.Kind, ContentHash: f.ContentHash, SizeBytes: f.SizeBytes, ManagedPath: f.ManagedPath, Incarnation: g.Incarnation}); e != nil {
				return e
			}
		}
		if len(files) < q.Limit {
			return nil
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := files[len(files)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

// DeleteGeneration при ожидаемой ревизии удаляет только невыбранное терминальное поколение без
// незавершённой работы, заранее сохраняя файловую очистку; удержания репозитория препятствуют
// удалению.
func (s *Service) DeleteGeneration(ctx context.Context, id ID, expected uint64) error {
	return s.uow.Write(ctx, func(r Repositories) error {
		// В транзакции запрещает удаление текущего, ожидающего, активного или неочищенного поколения, ставит
		// очистку файлов и удаляет варианты и поколение.

		// g содержит точное поколение текущей операции подготовки. e сообщает об ошибке чтения
		// точного поколения подготовки.
		g, e := r.Artifacts.Generation(ctx, id)
		if e != nil {
			return e
		}
		if g.Revision != expected {
			return ErrConflict
		}
		// a содержит стабильную заглушку и её указатели поколений.
		a, e := r.Artifacts.Asset(ctx, g.AssetID)
		if e != nil {
			return e
		}
		if a.CurrentGenerationID == g.ID || a.PendingGenerationID == g.ID || g.State.Active() || g.Issued || g.CleanupPending {
			return ErrInUse
		}
		if e = queueGenerationCleanup(ctx, r, g); e != nil {
			return e
		}
		for {
			// vs содержит страницу вариантов удаляемого поколения. e сообщает об ошибке
			// чтения вариантов удаляемого поколения.
			vs, e := r.Artifacts.Variants(ctx, ListQuery{Limit: MaxListLimit, GenerationID: g.ID})
			if e != nil {
				return e
			}
			// v содержит очередной вариант, удаляемый вместе с полным комплектом поколения.
			for _, v := range vs {
				if e = r.Artifacts.DeleteVariant(ctx, v.ID, v.Revision); e != nil {
					return e
				}
			}
			if len(vs) < MaxListLimit {
				break
			}
		}
		return r.Artifacts.DeleteGeneration(ctx, g.ID, g.Revision)
	})
}

// GetGeneration читает сохраняемые метаданные и протокол подготовки точного поколения.
func (s *Service) GetGeneration(ctx context.Context, id ID) (FallbackGeneration, error) {
	var result FallbackGeneration
	// err содержит итог согласованного чтения метаданных поколения.
	err := s.uow.View(ctx, func(r Repositories) error {
		// В согласованном чтении заполняет result поколением по ID и возвращает ошибку репозитория.
		// e передаёт результат чтения поколения из репозитория.
		var e error
		result, e = r.Artifacts.Generation(ctx, id)
		return e
	})
	return result, err
}

// ListGenerations возвращает страницу сохраняемых поколений с проверкой фильтров и курсора.
func (s *Service) ListGenerations(ctx context.Context, q PageRequest) (Page[FallbackGeneration], error) {
	return readPage(ctx, s, "generations", q, func(r Repositories, q ListQuery) ([]FallbackGeneration, error) {
		// Читает поколения по проверенным фильтрам и границе страницы.

		return r.Artifacts.Generations(ctx, q)
	}, func(g FallbackGeneration) ListBoundary {
		// Возвращает CreatedAt и ID поколения для исключающего курсора следующей страницы.
		return ListBoundary{g.CreatedAt, g.ID}
	})
}

// ListGenerationFiles возвращает страницу записей происхождения файлов, сохраняемых независимо от
// удаления метаданных поколения.
func (s *Service) ListGenerationFiles(ctx context.Context, q PageRequest) (Page[OwnedFile], error) {
	return readPage(ctx, s, "owned_files", q, func(r Repositories, q ListQuery) ([]OwnedFile, error) {
		// Читает принадлежащие поколению файлы по проверенным фильтрам и границе страницы.
		return r.Artifacts.OwnedFiles(ctx, q)
	}, func(f OwnedFile) ListBoundary {
		// Возвращает CreatedAt и ID файла для исключающего курсора следующей страницы.
		return ListBoundary{f.CreatedAt, f.ID}
	})
}

// recoverPreparations прерывает активные поколения прежних incarnation, сохраняя необходимость очистки
// выданной работы.
func recoverPreparations(ctx context.Context, r Repositories, incarnation string) error {
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей подготовки.
	q := ListQuery{Limit: MaxListLimit, ExcludeIncarnation: incarnation}
	for {
		// gs содержит страницу поколений, отобранных по текущим условиям проверки или
		// восстановления. e сообщает об ошибке чтения сохраняемых поколений подготовки.
		gs, e := r.Artifacts.Generations(ctx, q)
		if e != nil {
			return e
		}
		// g содержит прежнее поколение, которое прерывается при активном состоянии.
		for _, g := range gs {
			if g.State.Active() {
				if e = failPreparation(ctx, r, g, PreparationInterrupted, "Подготовка прервана сменой владельца"); e != nil {
					return e
				}
			}
		}
		if len(gs) < q.Limit {
			return nil
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := gs[len(gs)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
}

// completeGenerationLedger записывает хеши и размеры опубликованного оригинала и вариантов в их записи
// происхождения; отсутствие нужного пути возвращает ErrCorrupt.
func completeGenerationLedger(ctx context.Context, r Repositories, g FallbackGeneration, variants []FallbackVariant) error {
	// complete сопоставляет завершённым оригиналу и вариантам их проверенные хеши и размеры.
	complete := map[string]struct {
		// hash содержит проверенный хеш завершённого файла.
		hash string
		// size содержит проверенный размер завершённого файла в байтах.
		size uint64
	}{g.OriginalPath: {g.SourceHash, g.SizeBytes}}
	// v содержит очередной готовый вариант для переноса хеша и размера в журнал файлов.
	for _, v := range variants {
		complete[v.ManagedPath] = struct {
			// hash содержит проверенный хеш завершённого файла.
			hash string
			// size содержит проверенный размер завершённого файла в байтах.
			size uint64
		}{v.ContentHash, v.SizeBytes}
	}
	// q задаёт фильтры и границу постраничного обхода сохраняемых записей подготовки.
	q := ListQuery{Limit: MaxListLimit, GenerationID: g.ID}
	for {
		// files содержит принадлежащие поколению пути из журнала файлов. err сообщает об ошибке
		// чтения журнала файлов поколения.
		files, err := r.Artifacts.OwnedFiles(ctx, q)
		if err != nil {
			return err
		}
		// file содержит запись владения путём, заполняемую проверенным хешем и размером.
		for _, file := range files {
			// value содержит проверенный хеш и размер завершённого файла этого пути. ok
			// показывает наличие пути журнала в проверенном комплекте файлов.
			if value, ok := complete[file.ManagedPath]; ok {
				// old сохраняет ожидаемую ревизию изменяемой записи поколения, заглушки
				// или файла.
				old := file.Revision
				file.ContentHash = value.hash
				file.SizeBytes = value.size
				file.Record, err = revise(file.Record)
				if err != nil {
					return err
				}
				if err = r.Artifacts.UpdateOwnedFile(ctx, file, old); err != nil {
					return err
				}
				delete(complete, file.ManagedPath)
			}
		}
		if len(files) < q.Limit {
			break
		}
		// last содержит последнюю запись страницы для продолжения обхода.
		last := files[len(files)-1]
		q.After = &ListBoundary{last.CreatedAt, last.ID}
	}
	if len(complete) != 0 {
		return ErrCorrupt
	}
	return nil
}
