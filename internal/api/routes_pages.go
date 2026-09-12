package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/store"
)

func validPageJobPathID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value && id.Version() == 4
}

type PageSelectionRequest struct {
	Selection store.PageBinding `json:"selection"`
}
type PageRenderRequest struct {
	OperationID string            `json:"operation_id" format:"uuid"`
	Selection   store.PageBinding `json:"selection"`
	Pages       []int             `json:"pages" minItems:"1" maxItems:"16"`
	DPI         float64           `json:"dpi,omitempty" minimum:"0" maximum:"1000000"`
}
type PageInventoryResponse struct {
	RuntimeAvailable bool                `json:"runtime_available"`
	Inventory        store.PageInventory `json:"inventory"`
}

type PageImageRequest struct {
	NodeID       int64  `query:"node_id" minimum:"1"`
	Revision     int64  `query:"revision" minimum:"1"`
	VersionID    string `query:"version_id" format:"uuid"`
	SourceSHA256 string `query:"source_sha256" pattern:"^[0-9a-f]{64}$"`
	SourceSize   int64  `query:"source_size" minimum:"1" maximum:"67108864"`
	Page         int    `query:"page" minimum:"1" maximum:"1000"`
	RecipeSHA256 string `query:"recipe_sha256" pattern:"^[0-9a-f]{64}$"`
	FrameSHA256  string `query:"frame_sha256" pattern:"^[0-9a-f]{64}$"`
	ImageSHA256  string `query:"image_sha256" pattern:"^[0-9a-f]{64}$"`
}

func (r PageImageRequest) Binding() store.PageBinding {
	return store.PageBinding{NodeID: r.NodeID, Revision: r.Revision, Source: document.PageSource{VersionID: r.VersionID, SHA256: r.SourceSHA256, Size: r.SourceSize}}
}

func pageError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pagerender.ErrUnavailable):
		return NewError(http.StatusServiceUnavailable, "page_runtime_unavailable", "Configure and qualify the pinned Linux amd64 page runtime before requesting images.")
	case errors.Is(err, store.ErrPageFenced), errors.Is(err, store.ErrPageConflict):
		return NewError(http.StatusConflict, "page_selection_stale", "The exact page source, job or retained output changed; refresh the selection.")
	case errors.Is(err, store.ErrPageLimit):
		return NewError(http.StatusUnprocessableEntity, "page_limit", "The explicit page request exceeds supported bounds.")
	case errors.Is(err, pagerender.ErrUnsupported):
		return NewError(http.StatusUnprocessableEntity, "page_unsupported", "Physical page geometry or requested density is unavailable for this source.")
	default:
		return FromStoreError(err)
	}
}

func registerPageRoutes(humaAPI huma.API, d Deps, g *OperationGate) {
	huma.Register(humaAPI, huma.Operation{OperationID: "pageInventory", Method: http.MethodPost, Path: "/api/v1/pages/inventory", Summary: "Read physical frames and retained exact-version page images", MaxBodyBytes: 8192}, func(ctx context.Context, in *struct{ Body PageSelectionRequest }) (*struct{ Body PageInventoryResponse }, error) {
		inventory, err := d.Store.PageInventory(ctx, in.Body.Selection)
		if err != nil {
			return nil, pageError(err)
		}
		return &struct{ Body PageInventoryResponse }{Body: PageInventoryResponse{RuntimeAvailable: d.PageRuntime != nil, Inventory: inventory}}, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "createPageRenderJob", Method: http.MethodPost, Path: "/api/v1/pages/jobs", Summary: "Request a bounded explicit set of verified page images", MaxBodyBytes: 8192}, func(ctx context.Context, in *struct{ Body PageRenderRequest }) (*struct{ Body store.PageRenderJob }, error) {
		if d.PageRuntime == nil {
			return nil, pageError(pagerender.ErrUnavailable)
		}
		r := in.Body
		var job store.PageRenderJob
		err := g.mutate(func() error {
			var err error
			job, err = d.Store.QueuePageJob(ctx, r.OperationID, store.PageJobRequest{NodeID: r.Selection.NodeID, Revision: r.Selection.Revision, Source: r.Selection.Source, Pages: r.Pages, DPI: r.DPI, RuntimeFingerprint: d.PageRuntime.Fingerprint()})
			return err
		})
		if err != nil {
			return nil, pageError(err)
		}
		return &struct{ Body store.PageRenderJob }{Body: job}, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "getPageRenderJob", Method: http.MethodPost, Path: "/api/v1/pages/jobs/{id}", Summary: "Read a render job bound to the exact selected source", MaxBodyBytes: 8192}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body PageSelectionRequest
	}) (*struct{ Body store.PageRenderJob }, error) {
		job, err := d.Store.PageJob(ctx, in.ID, in.Body.Selection)
		if err != nil {
			return nil, pageError(err)
		}
		return &struct{ Body store.PageRenderJob }{Body: job}, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "cancelPageRenderJob", Method: http.MethodPost, Path: "/api/v1/pages/jobs/{id}/cancel", Summary: "Cancel a render job and fence late output", MaxBodyBytes: 8192}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body PageSelectionRequest
	}) (*struct{ Body store.PageRenderJob }, error) {
		if err := g.mutate(func() error { return d.Store.CancelPageJob(ctx, in.ID, in.Body.Selection) }); err != nil {
			return nil, pageError(err)
		}
		job, err := d.Store.PageJob(ctx, in.ID, in.Body.Selection)
		if err != nil {
			return nil, pageError(err)
		}
		return &struct{ Body store.PageRenderJob }{Body: job}, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "readPageImage", Method: http.MethodGet, Path: "/api/v1/pages/image", Summary: "Read verified PNG bytes for an exact retained page receipt"}, func(ctx context.Context, in *PageImageRequest) (*huma.StreamResponse, error) {
		view, err := d.Store.PageImage(ctx, in.Binding(), in.RecipeSHA256, in.Page)
		if err != nil {
			return nil, pageError(err)
		}
		if view.Frame.SHA256 != in.FrameSHA256 || view.Image.SHA256 != in.ImageSHA256 {
			return nil, pageError(store.ErrPageFenced)
		}
		stream, size, err := d.Blobs.OpenStreamContext(ctx, view.Image.SHA256)
		if err != nil {
			return nil, NewError(http.StatusServiceUnavailable, "page_image_unavailable", "The retained page image bytes are unavailable.")
		}
		defer func() { _ = stream.Close() }()
		if size != view.Image.Size || size > document.MaxPageImageBytes {
			return nil, pageError(store.ErrPageConflict)
		}
		body, err := io.ReadAll(io.LimitReader(stream, size+1))
		if err != nil || int64(len(body)) != size || !stream.Verified() {
			return nil, NewError(http.StatusInternalServerError, "page_image_corrupt", "The retained page image failed verification.")
		}
		digest := sha256.Sum256(body)
		if hex.EncodeToString(digest[:]) != view.Image.SHA256 {
			return nil, pageError(store.ErrPageConflict)
		}
		if err := pagerender.VerifyPNG(body, view.Image); err != nil {
			return nil, NewError(http.StatusInternalServerError, "page_image_corrupt", "The retained page image failed image verification.")
		}
		final, err := d.Store.PageImage(ctx, in.Binding(), in.RecipeSHA256, in.Page)
		if err != nil {
			return nil, pageError(err)
		}
		if final.Image != view.Image {
			return nil, pageError(store.ErrPageFenced)
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "image/png")
			hctx.SetHeader("Content-Length", strconv.FormatInt(size, 10))
			hctx.SetHeader("Cache-Control", "no-store")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			hctx.SetHeader("Content-Digest", contentDigest(digest[:]))
			hctx.SetHeader("X-Docbank-Page-Frame", view.Frame.SHA256)
			hctx.SetHeader("X-Docbank-Page-Recipe", view.Recipe.SHA256)
			hctx.SetHeader("X-Docbank-Page-SHA256", view.Image.SHA256)
			hctx.SetHeader("X-Docbank-Page-DPI", strconv.FormatFloat(view.Recipe.Recipe.DPI, 'f', -1, 64))
			_, _ = hctx.BodyWriter().Write(body)
		}}, nil
	})
}
