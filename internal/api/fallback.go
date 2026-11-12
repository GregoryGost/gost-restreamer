package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
)

// AssetListInput задаёт страницу заглушек с необязательным фильтром состояния подготовки.
type AssetListInput struct {
	// PageParams содержит параметры ограниченной страницы и курсора списка заглушек.
	PageParams
	// State ограничивает список заглушек выбранным состоянием PREPARING, READY или FAILED.
	State string `query:"state" enum:"PREPARING,READY,FAILED"`
}

// AssetCreate задаёт тело запроса создания именованной заглушки без загрузки содержимого.
type AssetCreate struct {
	// Body содержит JSON-данные новой именованной заглушки.
	Body struct {
		// Name задаёт непустое имя создаваемой заглушки.
		Name string `json:"name" minLength:"1"`
	}
}

// GenerationInput адресует точное поколение внутри указанной заглушки.
type GenerationInput struct {
	// IDInput задаёт идентификатор родительской заглушки в пути.
	IDInput
	// GenerationID задаёт идентификатор запрашиваемого поколения, чья принадлежность заглушке
	// проверяется обработчиком.
	GenerationID string `path:"generationId" minLength:"1"`
}

// VariantListInput задаёт страницу вариантов текущего или явно выбранного поколения заглушки.
type VariantListInput struct {
	// NestedListInput содержит идентификатор заглушки и параметры ограниченной страницы её
	// вариантов.
	NestedListInput
	// GenerationID выбирает точное поколение; пустое значение оставляет текущее поколение заглушки.
	GenerationID string `query:"generation_id"`
	// State ограничивает варианты выбранным состоянием подготовки.
	State string `query:"state" enum:"PREPARING,READY,FAILED"`
}

// RebuildInput задаёт заглушку и ожидаемую ревизию для повторной подготовки сохранённого оригинала.
type RebuildInput struct {
	// IDInput задаёт идентификатор заглушки, чей оригинал готовится повторно.
	IDInput
	// Body содержит JSON с ожидаемой ревизией заглушки.
	Body struct {
		// ExpectedRevision задаёт ненулевую ревизию для проверки отсутствия конкурирующего
		// изменения заглушки.
		ExpectedRevision Decimal `json:"expected_revision"`
	}
}

// PreparationDTO сообщает заглушку и точное принятое поколение для последующего чтения результата
// подготовки.
type PreparationDTO struct {
	// AssetID содержит идентификатор заглушки, для которой принято задание.
	AssetID string `json:"asset_id"`
	// GenerationID содержит идентификатор принятого поколения, совпадающего с ресурсом Location.
	GenerationID string `json:"generation_id"`
}

// UploadInput задаёт заглушку, ревизию, тип исходника и транспорт потоковой загрузки файла.
type UploadInput struct {
	// IDInput задаёт идентификатор заглушки, получающей новое содержимое.
	IDInput
	// ExpectedRevision передаёт обязательную десятичную ревизию заглушки в query перед допуском
	// загрузки.
	ExpectedRevision string `query:"expected_revision" required:"true"`
	// Kind определяет интерпретацию исходника как image или video.
	Kind string `query:"kind" required:"true" enum:"image,video"`
	// ContentType сохраняет заголовок запроса для допуска только application/octet-stream.
	ContentType string `header:"Content-Type"`
	// transport сохраняет Huma-контекст запроса для доступа к телу и сроку чтения в upload.
	transport huma.Context
}

// Resolve сохраняет транспортный контекст для потокового чтения тела загрузки после проверки
// параметров запроса.
func (i *UploadInput) Resolve(ctx huma.Context) []error { i.transport = ctx; return nil }

// nonemptyReader отклоняет поток, завершившийся EOF до первого полученного байта.
type nonemptyReader struct {
	// ReadCloser передаёт чтение и закрытие ограниченному телу исходного HTTP-запроса.
	io.ReadCloser
	// read запоминает получение хотя бы одного байта для различения пустого и завершённого непустого
	// потока.
	read bool
}

// Read отслеживает получение хотя бы одного байта и преобразует пустой EOF в ошибку недопустимого
// содержимого.
func (r *nonemptyReader) Read(p []byte) (int, error) {
	// n содержит число прочитанных байтов; e содержит результат чтения для отдельной обработки
	// пустого EOF.
	n, e := r.ReadCloser.Read(p)
	if n > 0 {
		r.read = true
	}
	if e == io.EOF && !r.read {
		return n, core.ErrInvalid
	}
	return n, e
}

// registerFallback публикует метаданные, поколения, варианты, rebuild и потоковую загрузку заглушек.
// Вложенные поколения проверяются относительно Asset, а 202 указывает точный результат подготовки.
func (a *API) registerFallback() {
	// c предоставляет доменные операции метаданных, поколений и вариантов заглушек
	// зарегистрированным обработчикам.
	c := a.config.Core
	register(a, operation("GET", "/fallback-assets", "list-fallback-assets", "Список заглушек."), func(ctx context.Context, i *AssetListInput) (*Result[PageDTO[AssetDTO]], error) {
		// Возвращает страницу заглушек с фильтром состояния.

		// q содержит проверенный запрос страницы заглушек; e содержит отказ преобразования
		// курсора или параметров страницы.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.State = i.State
		// p содержит страницу заглушек с выбранным фильтром состояния.
		p, e := c.ListAssets(ctx, q)
		return &Result[PageDTO[AssetDTO]]{pageDTO(p, assetDTO)}, e
	})
	// op описывает создание заглушки со статусом 201 и далее переиспользуется для регистрации
	// операций подготовки.
	op := operation("POST", "/fallback-assets", "create-fallback-asset", "Создать именованную заглушку.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *AssetCreate) (*Created[AssetDTO], error) {
		// Создаёт именованную заглушку и возвращает адрес нового ресурса.

		// v содержит созданную заглушку; e содержит отказ её создания для публичного
		// преобразования.
		v, e := c.CreateAsset(ctx, i.Body.Name)
		return &Created[AssetDTO]{Prefix + "/fallback-assets/" + string(v.ID), assetDTO(v)}, e
	})
	register(a, operation("GET", "/fallback-assets/{id}", "get-fallback-asset", "Получить заглушку."), func(ctx context.Context, i *IDInput) (*Result[AssetDTO], error) {
		// Возвращает сохранённую заглушку по идентификатору.

		// v содержит найденную заглушку; e содержит отказ чтения по идентификатору.
		v, e := c.GetAsset(ctx, core.ID(i.ID))
		return &Result[AssetDTO]{assetDTO(v)}, e
	})
	registerDelete(a, "/fallback-assets/{id}", "delete-fallback-asset", c.DeleteAsset)
	register(a, operation("GET", "/fallback-assets/{id}/generations/{generationId}", "get-fallback-generation", "Получить точный результат подготовки."), func(ctx context.Context, i *GenerationInput) (*Result[GenerationDTO], error) {
		// Проверяет принадлежность запрошенного поколения указанной заглушке перед выдачей результата
		// подготовки.

		// e содержит отказ проверки существования родительской заглушки перед чтением поколения.
		if _, e := c.GetAsset(ctx, core.ID(i.ID)); e != nil {
			return nil, e
		}
		// g содержит запрошенное поколение; e содержит отказ его чтения до проверки
		// принадлежности заглушке.
		g, e := c.GetGeneration(ctx, core.ID(i.GenerationID))
		if e != nil {
			return nil, e
		}
		if g.AssetID != core.ID(i.ID) {
			return nil, core.ErrNotFound
		}
		return &Result[GenerationDTO]{generationDTO(g)}, nil
	})
	register(a, operation("GET", "/fallback-assets/{id}/variants", "list-fallback-variants", "Варианты текущего или указанного поколения."), func(ctx context.Context, i *VariantListInput) (*Result[PageDTO[VariantDTO]], error) {
		// Выбирает текущее либо явно указанное поколение и возвращает страницу его вариантов, проверяя
		// принадлежность заглушке.

		// asset содержит родительскую заглушку; e содержит отказ её чтения перед выбором
		// поколения вариантов.
		asset, e := c.GetAsset(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		// generation содержит текущее либо явно запрошенное поколение, по которому
		// ограничивается список вариантов.
		generation := asset.CurrentGenerationID
		if i.GenerationID != "" {
			generation = core.ID(i.GenerationID)
		}
		// q содержит проверенные параметры страницы вариантов, дополненные фильтрами заглушки,
		// поколения и состояния.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.State = i.State
		q.Query.AssetID = asset.ID
		q.Query.GenerationID = generation
		if generation == "" {
			if i.Cursor != "" {
				return nil, badRequest()
			}
			return &Result[PageDTO[VariantDTO]]{PageDTO[VariantDTO]{Items: []VariantDTO{}}}, nil
		}
		// g содержит выбранное поколение для проверки его принадлежности родительской заглушке.
		g, e := c.GetGeneration(ctx, generation)
		if e != nil {
			return nil, e
		}
		if g.AssetID != asset.ID {
			return nil, core.ErrNotFound
		}
		// p содержит страницу вариантов проверенного поколения заглушки.
		p, e := c.ListVariants(ctx, q)
		return &Result[PageDTO[VariantDTO]]{pageDTO(p, variantDTO)}, e
	})
	op = operation("POST", "/fallback-assets/{id}/rebuild", "rebuild-fallback-asset", "Принять повторную подготовку заглушки.")
	op.DefaultStatus = 202
	register(a, op, func(ctx context.Context, i *RebuildInput) (*Created[PreparationDTO], error) {
		// Передаёт повторную подготовку с ожидаемой ревизией и возвращает адрес принятого поколения.

		if i.Body.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
		if a.config.Fallback == nil {
			return nil, unavailable()
		}
		// j содержит принятое задание повторной подготовки; e содержит отказ допуска с указанной
		// ожидаемой ревизией.
		j, e := a.config.Fallback.Rebuild(ctx, core.PreparationRequest{AssetID: core.ID(i.ID), ExpectedRevision: uint64(i.Body.ExpectedRevision), Rebuild: true})
		if e != nil {
			return nil, e
		}
		// g содержит поколение принятого задания для формирования Location и тела ответа 202.
		g := j.Generation()
		return preparationAccepted(g), nil
	})
	op = operation("PUT", "/fallback-assets/{id}/content", "upload-fallback-content", "Передать файл и продолжить подготовку в фоне.")
	op.DefaultStatus = 202
	op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}}
	huma.Register(a.huma, op, func(ctx context.Context, i *UploadInput) (*Created[PreparationDTO], error) {
		// Передаёт загрузку файла отдельному обработчику и переводит его ошибку в публичный ответ.

		// v содержит ответ принятой загрузки; e содержит отказ передачи содержимого для
		// безопасного HTTP-преобразования.
		v, e := a.upload(ctx, i)
		return v, a.problem(e)
	})
}

// preparationAccepted формирует ответ принятия с AssetID, GenerationID и Location точного поколения.
func preparationAccepted(g core.FallbackGeneration) *Created[PreparationDTO] {
	return &Created[PreparationDTO]{Prefix + "/fallback-assets/" + string(g.AssetID) + "/generations/" + string(g.ID), PreparationDTO{string(g.AssetID), string(g.ID)}}
}

// upload проверяет ревизию, Content-Type и пределы до передачи источника байтов владельцу подготовки.
// Ответ 202 ждёт сохранения оригинала и закрытия источника, а отмена или тайм-аут до завершения
// передачи возвращаются безопасной ошибкой.
func (a *API) upload(ctx context.Context, i *UploadInput) (*Created[PreparationDTO], error) {
	// expected содержит разобранную ожидаемую ревизию заглушки; e содержит отказ её десятичного
	// представления.
	expected, e := revision(i.ExpectedRevision)
	if e != nil {
		return nil, e
	}
	// kind содержит основной MIME-тип тела для допуска только application/octet-stream.
	kind, _, e := mime.ParseMediaType(i.ContentType)
	if e != nil || kind != "application/octet-stream" {
		return nil, statusError(415)
	}
	if a.config.Fallback == nil {
		return nil, unavailable()
	}
	// r содержит исходный HTTP-запрос с длиной и телом; w содержит транспорт ответа для
	// ограничивающего тело reader.
	r, w := humachi.Unwrap(i.transport)
	if r.ContentLength > a.config.Limits.UploadBytes {
		return nil, statusError(413)
	}
	if r.ContentLength == 0 {
		return nil, core.ErrInvalid
	}
	// cancel освобождает срок передачи файла после завершения upload.
	ctx, cancel := context.WithTimeout(ctx, a.config.Limits.UploadReadTimeout)
	defer cancel()
	// deadline содержит срок дочернего контекста загрузки для ограничения чтения HTTP-транспорта.
	deadline, _ := ctx.Deadline()
	if e = i.transport.SetReadDeadline(deadline); e != nil && !errors.Is(e, http.ErrNotSupported) {
		return nil, unavailable()
	}
	defer func() {
		// Сбрасывает срок чтения транспорта после завершения обработки загрузки.
		_ = i.transport.SetReadDeadline(time.Time{})
	}()
	// reader ограничивает байты тела и отклоняет пустой поток; после допуска его чтение и закрытие
	// получает владелец подготовки.
	reader := &nonemptyReader{ReadCloser: http.MaxBytesReader(w, r.Body, a.config.Limits.UploadBytes)}
	// admission ограничивает принятие задания управляющим сроком; finish освобождает этот контекст
	// сразу после Upload.
	admission, finish := a.operation(ctx)
	// job содержит принятое задание, чью передачу оригинала HTTP должен дождаться до ответа 202.
	job, e := a.config.Fallback.Upload(admission, core.PreparationRequest{AssetID: core.ID(i.ID), ExpectedRevision: expected, Kind: core.AssetKind(i.Kind)}, reader)
	finish()
	if e != nil {
		_ = reader.Close()
		return nil, e
	}
	if e = job.WaitUpload(ctx); e != nil {
		// timeout принимает транспортную ошибку для отображения истечения срока передачи в HTTP
		// 408.
		var timeout net.Error
		if errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled) || (errors.As(e, &timeout) && timeout.Timeout()) {
			return nil, statusError(408)
		}
		return nil, e
	}
	return preparationAccepted(job.Generation()), nil
}
