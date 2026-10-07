package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/conditional"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func registerPhotoQueryRoutes(api huma.API, d Deps, service *documentQueryService) {
	recipes := make(map[string]string, 3)
	for _, size := range []string{"grid", "fit", "large"} {
		recipe, err := processing.VisualPreviewRecipeForSize(size)
		if err != nil {
			panic(err)
		}
		_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
		if err != nil {
			panic(err)
		}
		recipes[size] = fingerprint
	}
	huma.Register(api, huma.Operation{
		OperationID: "listPhotoAssets", Method: http.MethodPost, Path: "/api/v1/photos/assets/query",
		Summary:      "Browse matching photo assets with live keyset pagination",
		MaxBodyBytes: query.MaxInputBytes + (64 << 10),
	}, func(ctx context.Context, in *struct{ Body PhotoBrowseRequest }) (*struct{ Body PhotoBrowsePage }, error) {
		value, err := query.Parse(in.Body.Query)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_query", err.Error())
		}
		var boundary *store.PhotoBrowsePosition
		if in.Body.Cursor != "" {
			position, err := service.decodePhotoCursor(in.Body.Cursor)
			if err != nil {
				return nil, FromStoreError(err)
			}
			boundary = &position
		}
		page, err := d.Store.ListPhotoAssets(ctx, store.PhotoBrowseRequest{
			Query: value,
			Coverage: store.CoverageSelection{
				Configuration:      in.Body.Coverage.Configuration,
				ProfileFingerprint: in.Body.Coverage.ProfileFingerprint,
			},
			PageSize: in.Body.PageSize, Recipes: recipes,
		}, boundary)
		if err != nil {
			return nil, workspaceQueryError(err)
		}
		wire := PhotoBrowsePage{Items: make([]PhotoBrowseRow, len(page.Items)), Total: page.Total}
		for i, row := range page.Items {
			slots := map[string]PhotoPreviewSlot{}
			for size, slot := range row.Previews {
				out := PhotoPreviewSlot{State: slot.State}
				if slot.Generation != nil {
					out.GenerationID = slot.Generation.GenerationID
					if slot.State == "ready" {
						output := slot.Generation.Preview.Output
						out.URL = fmt.Sprintf("/api/v1/photos/assets/%s/previews/%s", row.AssetID, out.GenerationID)
						out.SHA256 = output.BlobSHA256
						out.Size = &output.Size
						out.MediaType = output.MediaType
						out.Width = &output.Width
						out.Height = &output.Height
					}
				}
				slots[size] = out
			}
			wire.Items[i] = PhotoBrowseRow{
				AssetID: row.AssetID, Kind: row.Kind, Revision: row.Revision,
				DisplayFileID: row.DisplayFileID, NodeID: row.NodeID,
				ContentVersionID: row.ContentVersionID, Name: row.Name, MediaType: row.MediaType,
				ImportTime: row.ImportTime, CaptureTime: row.Fields.CaptureTime,
				CaptureTimePrecision: row.Fields.CaptureTimePrecision,
				CaptureTimeTimezone:  row.Fields.CaptureTimeTimezone,
				CaptureTimeOffset:    row.Fields.CaptureTimeOffset,
				WidthPX:              row.Fields.WidthPX, HeightPX: row.Fields.HeightPX,
				Previews: PhotoPreviewSlots{Grid: slots["grid"], Fit: slots["fit"], Large: slots["large"]},
				Quality:  photoQualityWire(row),
			}
		}
		if page.Next != nil {
			wire.NextCursor, err = service.encodePhotoCursor(*page.Next)
			if err != nil {
				return nil, FromStoreError(err)
			}
		}
		return &struct{ Body PhotoBrowsePage }{Body: wire}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "readPhotoPreview", Method: http.MethodGet,
		Path:        "/api/v1/photos/assets/{asset_id}/previews/{generation_id}",
		Summary:     "Read verified bytes of an eligible exact photo preview",
		Description: "Returns 304 Not Modified without a body when If-None-Match matches an eligible generation. Private HTTP caches must revalidate before reuse.",
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Verified JPEG preview",
				Content: map[string]*huma.MediaType{"image/jpeg": {
					Schema: &huma.Schema{Type: openAPIStringType, Format: openAPIBinaryFormat},
				}},
			},
			"304": {Description: "The cached preview is still eligible and unchanged"},
			"default": {Description: "Error", Content: map[string]*huma.MediaType{
				new(Error).ContentType("application/json"): {
					Schema: api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[Error](), true, ""),
				},
			}},
		},
	}, func(ctx context.Context, in *struct {
		AssetID      string `path:"asset_id"`
		GenerationID string `path:"generation_id"`
		IfNoneMatch  string `header:"If-None-Match"`
	}) (*huma.StreamResponse, error) {
		view, err := d.Store.PhotoVisualPreviewByGeneration(ctx, in.AssetID, in.GenerationID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		output := view.Generation.Preview.Output
		if view.Generation.Preview.State != document.VisualPreviewReady || output == nil {
			return nil, FromStoreError(store.ErrNotFound)
		}
		if output.Size <= 0 || output.Size > webPreviewImageMaxBytes || output.MediaType != "image/jpeg" {
			return nil, NewError(http.StatusInternalServerError, "photo_preview_corrupt", "The photo preview has invalid output metadata.")
		}
		setCacheHeaders := func(hctx huma.Context) {
			hctx.SetHeader("ETag", strconv.Quote(in.GenerationID))
			hctx.SetHeader("Cache-Control", "private, no-cache")
			hctx.SetHeader("Vary", "X-Api-Key, Authorization, "+WebSessionHeader)
		}
		condition := conditional.Params{IfNoneMatch: strings.Split(in.IfNoneMatch, ",")}
		for i, value := range condition.IfNoneMatch {
			condition.IfNoneMatch[i] = strings.TrimSpace(value)
		}
		if condition.PreconditionFailed(in.GenerationID, time.Time{}) != nil {
			return &huma.StreamResponse{Body: func(hctx huma.Context) {
				setCacheHeaders(hctx)
				hctx.SetStatus(http.StatusNotModified)
			}}, nil
		}
		stream, size, err := d.Blobs.OpenStreamContext(ctx, output.BlobSHA256)
		if err != nil {
			return nil, NewError(http.StatusServiceUnavailable, "photo_preview_unavailable", "The photo preview bytes are unavailable.")
		}
		defer func() { _ = stream.Close() }()
		if size != output.Size {
			return nil, NewError(http.StatusInternalServerError, "photo_preview_corrupt", "The photo preview size disagrees with its generation.")
		}
		body, err := io.ReadAll(io.LimitReader(stream, size+1))
		if err != nil || int64(len(body)) != size || !stream.Verified() {
			return nil, NewError(http.StatusInternalServerError, "photo_preview_corrupt", "The photo preview failed verification.")
		}
		digest := sha256.Sum256(body)
		if hex.EncodeToString(digest[:]) != output.BlobSHA256 {
			return nil, NewError(http.StatusInternalServerError, "photo_preview_corrupt", "The photo preview digest disagrees with its generation.")
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			setCacheHeaders(hctx)
			hctx.SetHeader("Content-Type", output.MediaType)
			hctx.SetHeader("Content-Length", strconv.FormatInt(size, 10))
			hctx.SetHeader("Content-Digest", contentDigest(digest[:]))
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			_, _ = hctx.BodyWriter().Write(body)
		}}, nil
	})
}

type photoCursorPayload struct {
	Type     string                    `json:"type"`
	IssuedAt int64                     `json:"iat"`
	Position store.PhotoBrowsePosition `json:"position"`
}

func (service *documentQueryService) encodePhotoCursor(position store.PhotoBrowsePosition) (string, error) {
	payload, err := json.Marshal(photoCursorPayload{Type: "photo-v1", IssuedAt: service.now().Unix(), Position: position})
	if err != nil {
		return "", fmt.Errorf("encode photo cursor: %w", err)
	}
	cursor := service.signCursorEnvelope(payload)
	if len(cursor) > MaxDocumentCursorBytes {
		return "", store.ErrInvalidPhotoCursor
	}
	return cursor, nil
}

func (service *documentQueryService) decodePhotoCursor(raw string) (store.PhotoBrowsePosition, error) {
	payload, err := service.verifyCursorEnvelope(raw)
	if err != nil {
		return store.PhotoBrowsePosition{}, fmt.Errorf("%w: %w", store.ErrInvalidPhotoCursor, err)
	}
	var cursor photoCursorPayload
	if err := json.Unmarshal(payload, &cursor, json.RejectUnknownMembers(true)); err != nil {
		return store.PhotoBrowsePosition{}, store.ErrInvalidPhotoCursor
	}
	position := cursor.Position
	if cursor.Type != "photo-v1" || query.ValidateTextOperand("asset", position.AssetID) != nil ||
		len(position.Key) > store.MaxPhotoSortKeyBytes || len(position.QueryIdentity) != 64 ||
		position.Missing && position.Key != "" || position.Total < 0 {
		return store.PhotoBrowsePosition{}, store.ErrInvalidPhotoCursor
	}
	if _, err := hex.DecodeString(position.QueryIdentity); err != nil {
		return store.PhotoBrowsePosition{}, store.ErrInvalidPhotoCursor
	}
	now := service.now()
	issued := time.Unix(cursor.IssuedAt, 0)
	if now.Before(issued) {
		return store.PhotoBrowsePosition{}, store.ErrInvalidPhotoCursor
	}
	if !now.Before(issued.Add(documentCursorTTL)) {
		return store.PhotoBrowsePosition{}, store.ErrDocumentCursorExpired
	}
	return position, nil
}

func photoQualityWire(row store.PhotoBrowseRow) *PhotoQuality {
	if row.Kind != "photo" {
		return nil
	}
	state := "pending"
	if row.Quality != nil {
		state = "ready"
	}
	return &PhotoQuality{State: state, Signals: (*PhotoQualitySignals)(row.Quality)}
}
