package api

import (
	"context"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// The editor reads the retained PDF selected by the production member. A
// source version can be an email or another non-PDF original, so its content
// endpoint cannot stand in for this exact rendition.
func registerProductionSourcePDFRoute(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "getProductionSourcePDF", Method: http.MethodGet,
		Path:    "/api/v1/productions/sets/{set_id}/revisions/{revision}/members/{member_id}/pdf",
		Summary: "Read the verified retained PDF for one exact draft member",
		Responses: map[string]*huma.Response{"200": {
			Description: "Verified PDF bytes bound to the selected production member",
			Content: map[string]*huma.MediaType{"application/pdf": {
				Schema: &huma.Schema{Type: openAPIStringType, Format: openAPIBinaryFormat},
			}},
		}},
	}, func(ctx context.Context, in *struct {
		SetID    string `path:"set_id" format:"uuid"`
		Revision int64  `path:"revision" minimum:"1"`
		MemberID string `path:"member_id" format:"uuid"`
		IfMatch  string `header:"If-Match"`
	}) (*huma.StreamResponse, error) {
		etag, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		if d.Store == nil || d.Blobs == nil {
			return nil, NewError(http.StatusServiceUnavailable, "production_unavailable",
				"production source storage is unavailable")
		}
		input, err := d.Store.OpenProductionDraftPreviewSource(ctx, in.SetID, in.Revision,
			etag, in.MemberID, d.Blobs)
		if err != nil {
			return nil, productionPreviewStageProblem(err)
		}
		verified, err := productionservice.SpoolProductionPDF(ctx, input.PDF,
			min(input.Recipe.MaxStagingBytes, int64(math.MaxUint32)))
		if err != nil {
			return nil, productionPreviewStageProblem(err)
		}
		current, err := d.Store.CurrentProductionDraftPreviewInput(ctx, store.ProductionPreviewCommand{
			SetID: in.SetID, Revision: in.Revision, ETag: etag,
			MemberID: in.MemberID, Page: 1, OperationID: uuid.NewString(),
		})
		if err != nil || current != input.PreviewInputSHA256 {
			_ = verified.Close()
			if err != nil {
				return nil, productionPreviewStageProblem(err)
			}
			return nil, NewError(http.StatusConflict, "production_source_stale",
				"the draft source changed before the PDF could be served")
		}
		pdf := input.PDF
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer func() { _ = verified.Close() }()
			hctx.SetHeader("Content-Type", "application/pdf")
			hctx.SetHeader("Content-Disposition", `attachment; filename="source.pdf"`)
			hctx.SetHeader("Cache-Control", "no-store")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			hctx.SetHeader("Content-Length", strconv.FormatInt(pdf.Size, 10))
			hctx.SetHeader(BlobHashHeader, pdf.PDFSHA256)
			hctx.SetHeader(BlobSizeHeader, strconv.FormatInt(pdf.Size, 10))
			hctx.SetHeader("Content-Digest", contentDigest(mustDecodeHash(pdf.PDFSHA256)))
			_, _ = io.Copy(hctx.BodyWriter(), verified.File)
		}}, nil
	})
}
