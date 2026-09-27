package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type ProductionPreviewRequest struct {
	OperationID string `json:"operation_id"`
	MemberID    string `json:"member_id"`
	Page        int    `json:"page"`
}

type ProductionPreviewArtifactTicket struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type ProductionPreviewTicket struct {
	OperationID        string                          `json:"operation_id"`
	PreviewInputSHA256 string                          `json:"preview_input_sha256"`
	ResolvedSHA256     string                          `json:"resolved_sha256"`
	Image              ProductionPreviewArtifactTicket `json:"image"`
	Text               ProductionPreviewArtifactTicket `json:"text"`
}

func productionPreviewStageProblem(err error) error {
	if errors.Is(err, store.ErrProductionRevisionConflict) || errors.Is(err, store.ErrNotFound) {
		return productionSetError(err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return productionDownloadProblem(err)
	}
	return NewError(http.StatusConflict, "production_preview_unavailable",
		"verified production preview source is unavailable")
}

func registerProductionPreviewRoutes(api huma.API, d Deps, g *OperationGate,
	downloads *webDownloadRegistry, sessions *webSessionRegistry) {
	huma.Register(api, huma.Operation{
		OperationID: "createProductionPreview", Method: http.MethodPost,
		Path:         "/api/v1/productions/sets/{set_id}/revisions/{revision}/previews",
		Summary:      "Issue one-use tickets for a verified unnumbered production preview",
		MaxBodyBytes: 4096,
	}, func(ctx context.Context, in *struct {
		SetID    string `path:"set_id" format:"uuid"`
		Revision int64  `path:"revision" minimum:"1"`
		IfMatch  string `header:"If-Match"`
		Body     ProductionPreviewRequest
	}) (*struct{ Body ProductionPreviewTicket }, error) {
		etag, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		if d.Store == nil || d.Blobs == nil || downloads == nil {
			return nil, NewError(http.StatusServiceUnavailable, "production_unavailable",
				"production preview storage is unavailable")
		}
		if err := downloads.ensureStagingDir(); err != nil {
			return nil, NewError(http.StatusInternalServerError, "production_preview_failed",
				"private preview staging is unavailable")
		}
		command := store.ProductionPreviewCommand{SetID: in.SetID, Revision: in.Revision,
			ETag: etag, OperationID: in.Body.OperationID, MemberID: in.Body.MemberID, Page: in.Body.Page}
		var admitted store.ProductionPreviewAdmission
		err = g.MutateContext(ctx, func() error {
			var err error
			admitted, err = d.Store.AdmitProductionDraftPreview(ctx, owner, command)
			return err
		})
		if err != nil {
			return nil, productionSetError(err)
		}
		staged, err := processing.PrepareProductionDraftPreview(ctx, d.Store, d.Blobs,
			downloads.dir, in.SetID, in.Revision, etag, in.Body.MemberID, in.Body.Page)
		if err != nil {
			return nil, productionPreviewStageProblem(err)
		}
		published := false
		defer func() {
			if !published {
				_ = staged.Close()
			}
		}()
		if staged.PreviewInputSHA256 != admitted.PreviewInputSHA256 {
			return nil, NewError(http.StatusConflict, "production_preview_stale",
				"the draft preview input changed before publication")
		}
		imageTicket := webDownloadTicket{path: staged.Image.File.Name(),
			name: "production-preview.png", mediaType: "image/png",
			blobHash: staged.ImageSHA256, size: staged.ImageSize, owner: owner,
			archiveFile: staged.Image.File, releaseArchive: func() { _ = staged.Image.Close() },
			verifyDigestOnDelivery: true}
		textTicket := webDownloadTicket{path: staged.Text.File.Name(),
			name: "production-preview.txt", mediaType: "text/plain; charset=utf-8",
			blobHash: staged.TextSHA256, size: staged.TextSize, owner: owner,
			archiveFile: staged.Text.File, releaseArchive: func() { _ = staged.Text.Close() },
			verifyDigestOnDelivery: true}
		var imageToken, textToken string
		issue := func() error {
			var issueErr error
			imageToken, issueErr = downloads.issue(imageTicket)
			if issueErr != nil {
				return issueErr
			}
			textToken, issueErr = downloads.issue(textTicket)
			if issueErr != nil {
				downloads.cancel(owner, imageToken)
			}
			return issueErr
		}
		err = g.MutateContext(ctx, func() error {
			currentSHA, currentErr := d.Store.CurrentProductionDraftPreviewInput(ctx, command)
			if currentErr != nil || currentSHA != admitted.PreviewInputSHA256 {
				if errors.Is(currentErr, context.Canceled) || errors.Is(currentErr, context.DeadlineExceeded) {
					return currentErr
				}
				return NewError(http.StatusConflict, "production_preview_stale",
					"the draft changed before preview publication")
			}
			if browserSessionRequest(ctx) {
				active, issueErr := sessions.withActiveOwner(owner, issue)
				if !active && issueErr == nil {
					issueErr = NewError(http.StatusGone, "download_session_expired",
						"the browser session expired before preview publication")
				}
				return issueErr
			}
			return issue()
		})
		if err != nil {
			return nil, productionDownloadProblem(err)
		}
		published = true
		return &struct{ Body ProductionPreviewTicket }{Body: ProductionPreviewTicket{
			OperationID: in.Body.OperationID, PreviewInputSHA256: admitted.PreviewInputSHA256,
			ResolvedSHA256: staged.ResolvedSHA256,
			Image: ProductionPreviewArtifactTicket{URL: webDownloadFilePath + "?ticket=" + imageToken,
				SHA256: staged.ImageSHA256, Size: staged.ImageSize},
			Text: ProductionPreviewArtifactTicket{URL: webDownloadFilePath + "?ticket=" + textToken,
				SHA256: staged.TextSHA256, Size: staged.TextSize},
		}}, nil
	})
}
