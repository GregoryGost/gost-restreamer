package storage

import (
	"context"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Stream читает проверенную конфигурацию Stream по ID в текущей транзакции.
func (r *repositories) Stream(ctx context.Context, id core.ID) (core.Stream, error) {
	return getValue[core.Stream](ctx, r, "streams", "id", id)
}

// Streams читает ограниченный упорядоченный список конфигураций Stream с разрешёнными фильтрами.
func (r *repositories) Streams(ctx context.Context, q core.ListQuery) ([]core.Stream, error) {
	return listValues[core.Stream](ctx, r, "streams", q)
}

// InsertStream проверяет и сохраняет новую конфигурацию Stream и её индексируемые поля.
func (r *repositories) InsertStream(ctx context.Context, v core.Stream) error {
	return insertValue(ctx, r, "streams", v)
}

// UpdateStream заменяет конфигурацию при ожидаемой ревизии, запрещая изменение ключа публикации.
func (r *repositories) UpdateStream(ctx context.Context, v core.Stream, expected uint64) error {
	return updateValue(ctx, r, "streams", v, expected)
}

// DeleteStream удаляет Stream при ожидаемой ревизии; ограничения связей и удержаний проверяет SQLite.
func (r *repositories) DeleteStream(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "streams", id, expected)
}

// Profile читает полный сохранённый профиль по ID без назначения значений по умолчанию.
func (r *repositories) Profile(ctx context.Context, id core.ID) (core.TranscodeProfile, error) {
	return getValue[core.TranscodeProfile](ctx, r, "profiles", "id", id)
}

// Profiles читает ограниченную страницу сохраняемых профилей.
func (r *repositories) Profiles(ctx context.Context, q core.ListQuery) ([]core.TranscodeProfile, error) {
	return listValues[core.TranscodeProfile](ctx, r, "profiles", q)
}

// InsertProfile проверяет и сохраняет полный профиль вместе с его записью.
func (r *repositories) InsertProfile(ctx context.Context, v core.TranscodeProfile) error {
	return insertValue(ctx, r, "profiles", v)
}

// UpdateProfile заменяет проверенный профиль при следующей ревизии после expected.
func (r *repositories) UpdateProfile(ctx context.Context, v core.TranscodeProfile, expected uint64) error {
	return updateValue(ctx, r, "profiles", v, expected)
}

// DeleteProfile удаляет профиль при ожидаемой ревизии с сохранением ограничений ссылок SQLite.
func (r *repositories) DeleteProfile(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "profiles", id, expected)
}

// Group читает конфигурацию группы и её ссылки по ID.
func (r *repositories) Group(ctx context.Context, id core.ID) (core.TranscodeGroup, error) {
	return getValue[core.TranscodeGroup](ctx, r, "groups", "id", id)
}

// Groups читает ограниченный упорядоченный список групп с фильтрами Stream и профиля.
func (r *repositories) Groups(ctx context.Context, q core.ListQuery) ([]core.TranscodeGroup, error) {
	return listValues[core.TranscodeGroup](ctx, r, "groups", q)
}

// InsertGroup проверяет и сохраняет новую группу и её ссылки.
func (r *repositories) InsertGroup(ctx context.Context, v core.TranscodeGroup) error {
	return insertValue(ctx, r, "groups", v)
}

// UpdateGroup заменяет проверенную группу при ожидаемой ревизии и неизменном времени создания.
func (r *repositories) UpdateGroup(ctx context.Context, v core.TranscodeGroup, expected uint64) error {
	return updateValue(ctx, r, "groups", v, expected)
}

// DeleteGroup удаляет группу при ожидаемой ревизии; используемые связи защищены ограничениями БД.
func (r *repositories) DeleteGroup(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "groups", id, expected)
}

// Destination читает настройки назначения по ID.
func (r *repositories) Destination(ctx context.Context, id core.ID) (core.Destination, error) {
	return getValue[core.Destination](ctx, r, "destinations", "id", id)
}

// Destinations читает ограниченный упорядоченный список назначений с разрешёнными фильтрами.
func (r *repositories) Destinations(ctx context.Context, q core.ListQuery) ([]core.Destination, error) {
	return listValues[core.Destination](ctx, r, "destinations", q)
}

// InsertDestination проверяет и сохраняет назначение; принадлежность группы Stream дополнительно
// проверяет SQLite.
func (r *repositories) InsertDestination(ctx context.Context, v core.Destination) error {
	return insertValue(ctx, r, "destinations", v)
}

// UpdateDestination заменяет проверенные настройки назначения при ожидаемой ревизии.
func (r *repositories) UpdateDestination(ctx context.Context, v core.Destination, expected uint64) error {
	return updateValue(ctx, r, "destinations", v, expected)
}

// DeleteDestination удаляет назначение при ожидаемой ревизии, сохраняя ограничения удержаний.
func (r *repositories) DeleteDestination(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "destinations", id, expected)
}

// Asset читает стабильный объект заглушки и указатели поколений по ID.
func (r *repositories) Asset(ctx context.Context, id core.ID) (core.FallbackAsset, error) {
	return getValue[core.FallbackAsset](ctx, r, "fallback_assets", "id", id)
}

// Assets читает ограниченную страницу метаданных заглушек.
func (r *repositories) Assets(ctx context.Context, q core.ListQuery) ([]core.FallbackAsset, error) {
	return listValues[core.FallbackAsset](ctx, r, "fallback_assets", q)
}

// InsertAsset проверяет и сохраняет стабильный объект заглушки.
func (r *repositories) InsertAsset(ctx context.Context, v core.FallbackAsset) error {
	return insertValue(ctx, r, "fallback_assets", v)
}

// UpdateAsset заменяет метаданные заглушки при ожидаемой ревизии, включая указатели текущего и
// ожидающего поколений.
func (r *repositories) UpdateAsset(ctx context.Context, v core.FallbackAsset, expected uint64) error {
	return updateValue(ctx, r, "fallback_assets", v, expected)
}

// DeleteAsset удаляет метаданные заглушки при ожидаемой ревизии; связанные конфигурации и удержания
// защищает БД.
func (r *repositories) DeleteAsset(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "fallback_assets", id, expected)
}

// Variant читает метаданные конкретного варианта с проверкой сохранённой нагрузки.
func (r *repositories) Variant(ctx context.Context, id core.ID) (core.FallbackVariant, error) {
	return getValue[core.FallbackVariant](ctx, r, "fallback_variants", "id", id)
}

// Variants читает ограниченный список вариантов с фильтрами поколения и заглушки.
func (r *repositories) Variants(ctx context.Context, q core.ListQuery) ([]core.FallbackVariant, error) {
	return listValues[core.FallbackVariant](ctx, r, "fallback_variants", q)
}

// InsertVariant проверяет и сохраняет вариант с точным профилем и принадлежностью поколению.
func (r *repositories) InsertVariant(ctx context.Context, v core.FallbackVariant) error {
	return insertValue(ctx, r, "fallback_variants", v)
}

// UpdateVariant обновляет вариант при ожидаемой ревизии, сохраняя его происхождение и неизменность
// завершённого содержимого.
func (r *repositories) UpdateVariant(ctx context.Context, v core.FallbackVariant, expected uint64) error {
	return updateValue(ctx, r, "fallback_variants", v, expected)
}

// DeleteVariant удаляет метаданные варианта при ожидаемой ревизии; физический файл этим не удаляется.
func (r *repositories) DeleteVariant(ctx context.Context, id core.ID, expected uint64) error {
	return deleteValue(ctx, r, "fallback_variants", id, expected)
}

// Session читает долговечное состояние и снимок запуска по ID.
func (r *repositories) Session(ctx context.Context, id core.ID) (core.Session, error) {
	return getValue[core.Session](ctx, r, "sessions", "id", id)
}

// Sessions читает ограниченную страницу долговечных запусков с разрешёнными фильтрами.
func (r *repositories) Sessions(ctx context.Context, q core.ListQuery) ([]core.Session, error) {
	return listValues[core.Session](ctx, r, "sessions", q)
}

// InsertSession проверяет и сохраняет новый запуск; единственность нетерминальной Session защищает
// SQLite.
func (r *repositories) InsertSession(ctx context.Context, v core.Session) error {
	return insertValue(ctx, r, "sessions", v)
}

// UpdateSession обновляет управление запуском при ожидаемой ревизии, сохраняя Stream, incarnation и
// исходный снимок.
func (r *repositories) UpdateSession(ctx context.Context, v core.Session, expected uint64) error {
	return updateValue(ctx, r, "sessions", v, expected)
}

// Output читает сохраняемое намерение и снимок выхода без оперативного состояния соединения.
func (r *repositories) Output(ctx context.Context, id core.ID) (core.SessionOutput, error) {
	return getValue[core.SessionOutput](ctx, r, "outputs", "id", id)
}

// Outputs читает ограниченную страницу сохраняемых выходов; неподдерживаемые фильтры отклоняются.
func (r *repositories) Outputs(ctx context.Context, q core.ListQuery) ([]core.SessionOutput, error) {
	return listValues[core.SessionOutput](ctx, r, "outputs", q)
}

// InsertOutput проверяет и сохраняет новый выход с его самостоятельным снимком.
func (r *repositories) InsertOutput(ctx context.Context, v core.SessionOutput) error {
	return insertValue(ctx, r, "outputs", v)
}

// UpdateOutput обновляет намерение, поколения и очистку выхода при ожидаемой ревизии, сохраняя
// SessionID и снимок.
func (r *repositories) UpdateOutput(ctx context.Context, v core.SessionOutput, expected uint64) error {
	return updateValue(ctx, r, "outputs", v, expected)
}

// ReceiptByID читает и проверяет квитанцию по внутреннему ID.
func (r *repositories) ReceiptByID(ctx context.Context, id core.ID) (core.CommandReceipt, error) {
	return getValue[core.CommandReceipt](ctx, r, "command_receipts", "id", id)
}

// Receipts читает ограниченный список квитанций, включая фильтр незавершённых команд.
func (r *repositories) Receipts(ctx context.Context, q core.ListQuery) ([]core.CommandReceipt, error) {
	return listValues[core.CommandReceipt](ctx, r, "command_receipts", q)
}

// InsertReceipt сохраняет квитанцию и её индексируемую идентичность в текущей транзакции записи.
func (r *repositories) InsertReceipt(ctx context.Context, v core.CommandReceipt) error {
	return insertValue(ctx, r, "command_receipts", v)
}

// UpdateReceipt обновляет состояние квитанции при ожидаемой ревизии, сохраняя исходную идентичность
// запроса и владельца.
func (r *repositories) UpdateReceipt(ctx context.Context, v core.CommandReceipt, expected uint64) error {
	return updateValue(ctx, r, "command_receipts", v, expected)
}

// Cleanup читает сохраняемое намерение очистки по ID.
func (r *repositories) Cleanup(ctx context.Context, id core.ID) (core.CleanupIntent, error) {
	return getValue[core.CleanupIntent](ctx, r, "cleanup_intents", "id", id)
}

// Cleanups читает ограниченный список намерений очистки, включая неподтверждённые.
func (r *repositories) Cleanups(ctx context.Context, q core.ListQuery) ([]core.CleanupIntent, error) {
	return listValues[core.CleanupIntent](ctx, r, "cleanup_intents", q)
}

// InsertCleanup проверяет и сохраняет намерение точной очистки ресурсов исполнения либо файла.
func (r *repositories) InsertCleanup(ctx context.Context, v core.CleanupIntent) error {
	return insertValue(ctx, r, "cleanup_intents", v)
}

// UpdateCleanup обновляет подтверждение очистки при ожидаемой ревизии, запрещая изменение
// происхождения и владельца.
func (r *repositories) UpdateCleanup(ctx context.Context, v core.CleanupIntent, expected uint64) error {
	return updateValue(ctx, r, "cleanup_intents", v, expected)
}

// Receipt читает квитанцию по исходному RequestID с проверкой её сохраняемых метаданных.
func (r *repositories) Receipt(ctx context.Context, requestID string) (core.CommandReceipt, error) {
	return getValue[core.CommandReceipt](ctx, r, "command_receipts", "request_id", requestID)
}

// ActiveSession читает единственную STARTING, RUNNING или STOPPING-сессию Stream; отсутствие
// переводится в ErrNotFound.
func (r *repositories) ActiveSession(ctx context.Context, streamID core.ID) (core.Session, error) {
	// db содержит разрешённую читающую GORM-сессию, а err — результат проверки доступа к
	// репозиторию.
	db, err := r.access(ctx, false)
	if err != nil {
		return core.Session{}, err
	}
	// row принимает единственную нетерминальную сессию потока для проверки метаданных.
	var row metadataRow
	err = db.Table("sessions").Where("stream_id = ? AND state IN ?", streamID, []string{"STARTING", "RUNNING", "STOPPING"}).Take(&row).Error
	if err != nil {
		return core.Session{}, translate(err)
	}
	return decodeRow[core.Session](row)
}

// InsertEvent проверяет и сохраняет неизменяемое событие протокола управления.
func (r *repositories) InsertEvent(ctx context.Context, v core.Event) error {
	return insertValue(ctx, r, "events", v)
}

// Events читает ограниченную упорядоченную страницу долговечных событий управления.
func (r *repositories) Events(ctx context.Context, q core.ListQuery) ([]core.Event, error) {
	return listValues[core.Event](ctx, r, "events", q)
}

// InsertHold сохраняет ссылки удержания Session либо конкретного Output; вид таблицы выбирается по
// наличию OutputID.
func (r *repositories) InsertHold(ctx context.Context, v core.ReferenceHold) error {
	// err сообщает об ошибке проверки полного набора удерживаемых ссылок.
	if err := v.Validate(); err != nil {
		return err
	}
	// db содержит разрешённую записывающую GORM-сессию, а err — результат проверки доступа.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	// fields содержит SQL-поля удержания Session либо отдельного Output.
	fields := map[string]any{"session_id": v.SessionID, "stream_id": v.StreamID, "asset_id": v.AssetID, "generation_id": v.GenerationID}
	// table выбирает таблицу удержаний сессии или выхода по наличию OutputID.
	table := "session_holds"
	if v.OutputID != "" {
		table = "output_holds"
		fields["output_id"] = v.OutputID
		fields["group_id"] = v.GroupID
		fields["profile_id"] = v.ProfileID
		fields["destination_id"] = v.DestinationID
		fields["variant_id"] = v.VariantID
	}
	return translate(db.Table(table).Create(fields).Error)
}

// ReleaseHolds удаляет удержания указанного выхода, а при пустом OutputID — все удержания выходов и
// самой Session; пустой SessionID отклоняется.
func (r *repositories) ReleaseHolds(ctx context.Context, sessionID, outputID core.ID) error {
	if sessionID == "" {
		return core.ErrInvalid
	}
	// db содержит разрешённую записывающую GORM-сессию, а err — результат проверки доступа.
	db, err := r.access(ctx, true)
	if err != nil {
		return err
	}
	// query ограничивает удаление удержаний выходов указанной сессией.
	query := db.Table("output_holds").Where("session_id = ?", sessionID)
	if outputID != "" {
		return translate(query.Where("output_id = ?", outputID).Delete(map[string]any{}).Error)
	}
	if err = query.Delete(map[string]any{}).Error; err != nil {
		return translate(err)
	}
	return translate(db.Table("session_holds").Where("session_id = ?", sessionID).Delete(map[string]any{}).Error)
}

// Generation читает полные метаданные и протокол подготовки поколения по ID.
func (r *repositories) Generation(ctx context.Context, id core.ID) (core.FallbackGeneration, error) {
	return getValue[core.FallbackGeneration](ctx, r, "fallback_generations", "id", id)
}

// Generations читает ограниченный список поколений с разрешёнными фильтрами.
func (r *repositories) Generations(ctx context.Context, q core.ListQuery) ([]core.FallbackGeneration, error) {
	return listValues[core.FallbackGeneration](ctx, r, "fallback_generations", q)
}

// InsertGeneration проверяет и сохраняет новое поколение с полным комплектом требуемых профилей.
func (r *repositories) InsertGeneration(ctx context.Context, v core.FallbackGeneration) error {
	return insertValue(ctx, r, "fallback_generations", v)
}

// UpdateGeneration обновляет протокол подготовки при ожидаемой ревизии, сохраняя происхождение и
// полный набор профилей; терминальное содержимое неизменяемо.
func (r *repositories) UpdateGeneration(ctx context.Context, v core.FallbackGeneration, e uint64) error {
	return updateValue(ctx, r, "fallback_generations", v, e)
}

// DeleteGeneration удаляет метаданные поколения при ожидаемой ревизии; удержания защищаются SQLite,
// файлы удаляются отдельно.
func (r *repositories) DeleteGeneration(ctx context.Context, id core.ID, e uint64) error {
	return deleteValue(ctx, r, "fallback_generations", id, e)
}

// InsertOwnedFile проверяет и сохраняет происхождение управляемого пути поколения независимо от
// наличия файла на диске.
func (r *repositories) InsertOwnedFile(ctx context.Context, v core.OwnedFile) error {
	return insertValue(ctx, r, "owned_paths", v)
}

// OwnedFiles читает ограниченную страницу записей происхождения управляемых путей.
func (r *repositories) OwnedFiles(ctx context.Context, q core.ListQuery) ([]core.OwnedFile, error) {
	return listValues[core.OwnedFile](ctx, r, "owned_paths", q)
}

// UpdateOwnedFile заполняет хеш и размер исходно пустой записи либо обновляет её Record при ожидаемой
// ревизии; происхождение и уже записанное содержимое неизменяемы.
func (r *repositories) UpdateOwnedFile(ctx context.Context, v core.OwnedFile, expected uint64) error {
	return updateValue(ctx, r, "owned_paths", v, expected)
}

// GenerationHeld проверяет наличие хотя бы одного удержания поколения среди Session и Output в текущем
// согласованном чтении.
func (r *repositories) GenerationHeld(ctx context.Context, id core.ID) (bool, error) {
	// err сообщает об ошибке проверки идентификатора удерживаемого поколения.
	if err := id.Validate(); err != nil {
		return false, err
	}
	// db содержит разрешённую читающую GORM-сессию, а err — результат проверки доступа.
	db, err := r.access(ctx, false)
	if err != nil {
		return false, err
	}
	// held принимает результат проверки существования удержаний поколения в обеих таблицах.
	var held bool
	err = db.Raw("SELECT EXISTS(SELECT 1 FROM session_holds WHERE generation_id = ? UNION ALL SELECT 1 FROM output_holds WHERE generation_id = ?)", id, id).Scan(&held).Error
	return held, translate(err)
}
