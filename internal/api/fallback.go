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

type AssetListInput struct {
	PageParams
	State string `query:"state" enum:"PREPARING,READY,FAILED"`
}
type AssetCreate struct {
	Body struct {
		Name string `json:"name" minLength:"1"`
	}
}
type GenerationInput struct {
	IDInput
	GenerationID string `path:"generationId" minLength:"1"`
}
type VariantListInput struct {
	NestedListInput
	GenerationID string `query:"generation_id"`
	State        string `query:"state" enum:"PREPARING,READY,FAILED"`
}
type RebuildInput struct {
	IDInput
	Body struct {
		ExpectedRevision Decimal `json:"expected_revision"`
	}
}
type PreparationDTO struct {
	AssetID      string `json:"asset_id"`
	GenerationID string `json:"generation_id"`
}
type UploadInput struct {
	IDInput
	ExpectedRevision string `query:"expected_revision" required:"true"`
	Kind             string `query:"kind" required:"true" enum:"image,video"`
	ContentType      string `header:"Content-Type"`
	transport        huma.Context
}

func (i *UploadInput) Resolve(ctx huma.Context) []error { i.transport = ctx; return nil }

type nonemptyReader struct {
	io.ReadCloser
	read bool
}

func (r *nonemptyReader) Read(p []byte) (int, error) {
	n, e := r.ReadCloser.Read(p)
	if n > 0 {
		r.read = true
	}
	if e == io.EOF && !r.read {
		return n, core.ErrInvalid
	}
	return n, e
}
func (a *API) registerFallback() {
	c := a.config.Core
	register(a, operation("GET", "/fallback-assets", "list-fallback-assets", "Список заглушек."), func(ctx context.Context, i *AssetListInput) (*Result[PageDTO[AssetDTO]], error) {
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.State = i.State
		p, e := c.ListAssets(ctx, q)
		return &Result[PageDTO[AssetDTO]]{pageDTO(p, assetDTO)}, e
	})
	op := operation("POST", "/fallback-assets", "create-fallback-asset", "Создать именованную заглушку.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *AssetCreate) (*Created[AssetDTO], error) {
		v, e := c.CreateAsset(ctx, i.Body.Name)
		return &Created[AssetDTO]{Prefix + "/fallback-assets/" + string(v.ID), assetDTO(v)}, e
	})
	register(a, operation("GET", "/fallback-assets/{id}", "get-fallback-asset", "Получить заглушку."), func(ctx context.Context, i *IDInput) (*Result[AssetDTO], error) {
		v, e := c.GetAsset(ctx, core.ID(i.ID))
		return &Result[AssetDTO]{assetDTO(v)}, e
	})
	registerDelete(a, "/fallback-assets/{id}", "delete-fallback-asset", c.DeleteAsset)
	register(a, operation("GET", "/fallback-assets/{id}/generations/{generationId}", "get-fallback-generation", "Получить точный результат подготовки."), func(ctx context.Context, i *GenerationInput) (*Result[GenerationDTO], error) {
		if _, e := c.GetAsset(ctx, core.ID(i.ID)); e != nil {
			return nil, e
		}
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
		asset, e := c.GetAsset(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		generation := asset.CurrentGenerationID
		if i.GenerationID != "" {
			generation = core.ID(i.GenerationID)
		}
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
		g, e := c.GetGeneration(ctx, generation)
		if e != nil {
			return nil, e
		}
		if g.AssetID != asset.ID {
			return nil, core.ErrNotFound
		}
		p, e := c.ListVariants(ctx, q)
		return &Result[PageDTO[VariantDTO]]{pageDTO(p, variantDTO)}, e
	})
	op = operation("POST", "/fallback-assets/{id}/rebuild", "rebuild-fallback-asset", "Принять повторную подготовку заглушки.")
	op.DefaultStatus = 202
	register(a, op, func(ctx context.Context, i *RebuildInput) (*Created[PreparationDTO], error) {
		if i.Body.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
		if a.config.Fallback == nil {
			return nil, unavailable()
		}
		j, e := a.config.Fallback.Rebuild(ctx, core.PreparationRequest{AssetID: core.ID(i.ID), ExpectedRevision: uint64(i.Body.ExpectedRevision), Rebuild: true})
		if e != nil {
			return nil, e
		}
		g := j.Generation()
		return preparationAccepted(g), nil
	})
	op = operation("PUT", "/fallback-assets/{id}/content", "upload-fallback-content", "Передать файл и продолжить подготовку в фоне.")
	op.DefaultStatus = 202
	op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}}
	huma.Register(a.huma, op, func(ctx context.Context, i *UploadInput) (*Created[PreparationDTO], error) {
		v, e := a.upload(ctx, i)
		return v, a.problem(e)
	})
}
func preparationAccepted(g core.FallbackGeneration) *Created[PreparationDTO] {
	return &Created[PreparationDTO]{Prefix + "/fallback-assets/" + string(g.AssetID) + "/generations/" + string(g.ID), PreparationDTO{string(g.AssetID), string(g.ID)}}
}
func (a *API) upload(ctx context.Context, i *UploadInput) (*Created[PreparationDTO], error) {
	expected, e := revision(i.ExpectedRevision)
	if e != nil {
		return nil, e
	}
	kind, _, e := mime.ParseMediaType(i.ContentType)
	if e != nil || kind != "application/octet-stream" {
		return nil, statusError(415)
	}
	if a.config.Fallback == nil {
		return nil, unavailable()
	}
	r, w := humachi.Unwrap(i.transport)
	if r.ContentLength > a.config.Limits.UploadBytes {
		return nil, statusError(413)
	}
	if r.ContentLength == 0 {
		return nil, core.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, a.config.Limits.UploadReadTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if e = i.transport.SetReadDeadline(deadline); e != nil && !errors.Is(e, http.ErrNotSupported) {
		return nil, unavailable()
	}
	defer func() { _ = i.transport.SetReadDeadline(time.Time{}) }()
	reader := &nonemptyReader{ReadCloser: http.MaxBytesReader(w, r.Body, a.config.Limits.UploadBytes)}
	admission, finish := a.operation(ctx)
	job, e := a.config.Fallback.Upload(admission, core.PreparationRequest{AssetID: core.ID(i.ID), ExpectedRevision: expected, Kind: core.AssetKind(i.Kind)}, reader)
	finish()
	if e != nil {
		_ = reader.Close()
		return nil, e
	}
	if e = job.WaitUpload(ctx); e != nil {
		var timeout net.Error
		if errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled) || (errors.As(e, &timeout) && timeout.Timeout()) {
			return nil, statusError(408)
		}
		return nil, e
	}
	return preparationAccepted(job.Generation()), nil
}
