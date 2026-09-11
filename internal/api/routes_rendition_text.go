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

	"go.kenn.io/docbank/internal/store"
)

const renditionTextMaxBytes int64 = 16 << 20

type renditionTextObserved struct {
	Configuration      string `json:"configuration" enum:"configured,unconfigured,profile_required"`
	ProfileFingerprint string `json:"profile_fingerprint,omitempty" pattern:"^[0-9a-f]{64}$"`
	GenerationID       string `json:"generation_id,omitempty" pattern:"^[0-9a-f]{64}$"`
	CoverageState      string `json:"coverage_state,omitempty" enum:"complete,partial,failed,unprocessed,none,unavailable"`
	AttachmentID       string `json:"attachment_id,omitempty" pattern:"^[0-9a-f]{64}$"`
	BuildID            string `json:"build_id,omitempty" pattern:"^[0-9a-f]{64}$"`
}

type renditionTextRequest struct {
	NodeID    int64                  `json:"node_id" minimum:"1"`
	Revision  int64                  `json:"revision" minimum:"1"`
	VersionID string                 `json:"version_id" format:"uuid"`
	BlobHash  string                 `json:"blob_hash" pattern:"^[0-9a-f]{64}$"`
	Size      int64                  `json:"size" minimum:"0"`
	Profile   string                 `json:"profile,omitempty" maxLength:"128"`
	Observed  *renditionTextObserved `json:"observed,omitempty"`
}

type renditionTextSource struct {
	NodeID    int64  `json:"node_id"`
	Revision  int64  `json:"revision"`
	VersionID string `json:"version_id"`
	BlobHash  string `json:"blob_hash"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

type renditionTextProfile struct {
	Name          string `json:"name"`
	Configuration string `json:"configuration" enum:"configured,unconfigured,profile_required"`
	Fingerprint   string `json:"fingerprint"`
}

type renditionTextArtifact struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

type renditionTextReceipt struct {
	State        string                 `json:"state" enum:"ready,verified_empty,failed,unprocessed,unconfigured,profile_required,historical_unavailable"`
	Source       renditionTextSource    `json:"source"`
	Profile      renditionTextProfile   `json:"profile"`
	GenerationID string                 `json:"generation_id"`
	AttachmentID string                 `json:"attachment_id"`
	BuildID      string                 `json:"build_id"`
	Artifact     *renditionTextArtifact `json:"artifact,omitempty"`
}

func exactRenditionTextSource(ctx context.Context, d Deps, request renditionTextRequest) (store.ContentVersionView, error) {
	view, err := d.Store.ContentVersionViewByID(ctx, request.NodeID, request.VersionID)
	if err != nil {
		return store.ContentVersionView{}, FromStoreError(err)
	}
	if view.Node.IsDir() || view.Node.TrashedAt != nil {
		return store.ContentVersionView{}, NewError(http.StatusNotFound, "not_found", "The selected document is not live.")
	}
	if view.Node.Revision != request.Revision || view.Version.BlobHash != request.BlobHash ||
		view.Version.Size != request.Size {
		return store.ContentVersionView{}, NewError(http.StatusConflict, "rendition_selection_stale",
			"The selected document or version changed; refresh it before reading text.")
	}
	return view, nil
}

func renditionTextState(request renditionTextRequest, selection collectionProfileSelection) (renditionTextProfile, store.RenditionTextBinding, string, error) {
	profile := renditionTextProfile{Name: selection.Name, Configuration: selection.Coverage.Configuration,
		Fingerprint: selection.Coverage.ProfileFingerprint}
	binding := store.RenditionTextBinding{NodeID: request.NodeID, NodeRevision: request.Revision,
		ContentVersionID: request.VersionID, SourceSHA256: request.BlobHash, SourceSize: request.Size}
	coverageState := ""
	if request.Observed != nil {
		observed := request.Observed
		profile.Configuration = observed.Configuration
		profile.Fingerprint = observed.ProfileFingerprint
		coverageState = observed.CoverageState
		binding.GenerationID = observed.GenerationID
		binding.AttachmentID = observed.AttachmentID
		binding.BuildID = observed.BuildID
		if observed.Configuration == "configured" && observed.ProfileFingerprint == "" {
			return profile, binding, coverageState, NewError(http.StatusUnprocessableEntity,
				"invalid_rendition_selection", "A configured rendition observation requires a profile fingerprint.")
		}
		if selection.Coverage.Configuration == "configured" && request.Profile != "" &&
			observed.Configuration == "configured" &&
			selection.Coverage.ProfileFingerprint != observed.ProfileFingerprint {
			return profile, binding, coverageState, NewError(http.StatusConflict,
				"rendition_selection_stale", "The selected processing profile changed; refresh the snapshot.")
		}
	}
	binding.ProfileFingerprint = profile.Fingerprint
	return profile, binding, coverageState, nil
}

func absentRenditionState(coverage string, binding store.RenditionTextBinding) string {
	switch coverage {
	case "failed":
		return "failed"
	case "unprocessed", "none", "":
		return "unprocessed"
	default:
		if binding.GenerationID != "" || binding.AttachmentID != "" || binding.BuildID != "" ||
			coverage == "complete" || coverage == "partial" || coverage == "unavailable" {
			return "historical_unavailable"
		}
		return "unprocessed"
	}
}

func resolveRenditionText(ctx context.Context, d Deps, request renditionTextRequest) (renditionTextReceipt, error) {
	sourceView, err := exactRenditionTextSource(ctx, d, request)
	if err != nil {
		return renditionTextReceipt{}, err
	}
	selection, err := selectCollectionProfile(d.Cfg, request.Profile)
	if err != nil {
		return renditionTextReceipt{}, err
	}
	profile, binding, coverage, err := renditionTextState(request, selection)
	if err != nil {
		return renditionTextReceipt{}, err
	}
	receipt := renditionTextReceipt{Source: renditionTextSource{NodeID: sourceView.Node.ID,
		Revision: sourceView.Node.Revision, VersionID: sourceView.Version.ID,
		BlobHash: sourceView.Version.BlobHash, Size: sourceView.Version.Size,
		MediaType: sourceView.Version.MimeType}, Profile: profile,
		GenerationID: binding.GenerationID, AttachmentID: binding.AttachmentID, BuildID: binding.BuildID}
	if profile.Configuration != "configured" {
		receipt.State = profile.Configuration
		return receipt, nil
	}
	view, err := d.Store.ResolveRenditionText(ctx, binding)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrRenditionTextUnavailable) {
		receipt.State = absentRenditionState(coverage, binding)
		return receipt, nil
	}
	if errors.Is(err, store.ErrRenditionTextStale) {
		return renditionTextReceipt{}, NewError(http.StatusConflict, "rendition_selection_stale",
			"The selected rendition generation changed; refresh the snapshot.")
	}
	if err != nil {
		return renditionTextReceipt{}, FromStoreError(err)
	}
	receipt.GenerationID = binding.GenerationID
	receipt.AttachmentID = view.Rendition.Attachment.ID
	receipt.BuildID = view.Rendition.Build.ID
	if view.Empty {
		receipt.State = "verified_empty"
		return receipt, nil
	}
	if view.Artifact.Size > renditionTextMaxBytes {
		return renditionTextReceipt{}, NewError(http.StatusRequestEntityTooLarge, "rendition_text_too_large",
			"The verified text rendition exceeds the browser text limit.")
	}
	receipt.State = "ready"
	receipt.Artifact = &renditionTextArtifact{ID: view.Artifact.ID, SHA256: view.Artifact.BlobHash,
		Size: view.Artifact.Size, MediaType: "text/markdown; charset=utf-8"}
	return receipt, nil
}

func registerRenditionTextRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{OperationID: "resolveRenditionText", Method: http.MethodPost,
		Path: "/api/v1/renditions/text", Summary: "Resolve exact verified text for one selected document version",
		MaxBodyBytes: 8192}, func(ctx context.Context, in *struct{ Body renditionTextRequest }) (*struct{ Body renditionTextReceipt }, error) {
		receipt, err := resolveRenditionText(ctx, d, in.Body)
		if err != nil {
			return nil, err
		}
		return &struct{ Body renditionTextReceipt }{Body: receipt}, nil
	})

	huma.Register(api, huma.Operation{OperationID: "readRenditionText", Method: http.MethodGet,
		Path: "/api/v1/renditions/text/content", Summary: "Read one exact verified text rendition artifact"},
		func(ctx context.Context, in *struct {
			NodeID             int64  `query:"node_id" minimum:"1"`
			Revision           int64  `query:"revision" minimum:"1"`
			VersionID          string `query:"version_id" format:"uuid"`
			BlobHash           string `query:"blob_hash" pattern:"^[0-9a-f]{64}$"`
			Size               int64  `query:"size" minimum:"0"`
			ProfileFingerprint string `query:"profile_fingerprint" pattern:"^[0-9a-f]{64}$"`
			GenerationID       string `query:"generation_id,omitempty" pattern:"^[0-9a-f]{64}$"`
			AttachmentID       string `query:"attachment_id" pattern:"^[0-9a-f]{64}$"`
			BuildID            string `query:"build_id" pattern:"^[0-9a-f]{64}$"`
			ArtifactID         string `query:"artifact_id" maxLength:"128"`
		}) (*huma.StreamResponse, error) {
			view, err := d.Store.ResolveRenditionText(ctx, store.RenditionTextBinding{NodeID: in.NodeID,
				NodeRevision: in.Revision, ContentVersionID: in.VersionID, SourceSHA256: in.BlobHash,
				SourceSize: in.Size, ProfileFingerprint: in.ProfileFingerprint,
				GenerationID: in.GenerationID, AttachmentID: in.AttachmentID, BuildID: in.BuildID})
			if errors.Is(err, store.ErrRenditionTextStale) || errors.Is(err, store.ErrNotFound) ||
				errors.Is(err, store.ErrRenditionTextUnavailable) {
				return nil, NewError(http.StatusConflict, "rendition_selection_stale",
					"The selected rendition is no longer available; refresh the snapshot.")
			}
			if err != nil {
				return nil, FromStoreError(err)
			}
			if view.Empty || view.Artifact == nil || view.Artifact.ID != in.ArtifactID {
				return nil, NewError(http.StatusConflict, "rendition_selection_stale",
					"The selected rendition artifact changed; refresh the snapshot.")
			}
			if view.Artifact.Size > renditionTextMaxBytes {
				return nil, NewError(http.StatusRequestEntityTooLarge, "rendition_text_too_large",
					"The verified text rendition exceeds the browser text limit.")
			}
			stream, size, err := d.Blobs.OpenStreamContext(ctx, view.Artifact.BlobHash)
			if err != nil {
				return nil, NewError(http.StatusServiceUnavailable, "rendition_text_unavailable",
					"The verified text rendition bytes are unavailable.")
			}
			defer func() { _ = stream.Close() }()
			if size != view.Artifact.Size {
				return nil, NewError(http.StatusInternalServerError, "rendition_text_corrupt",
					"The verified text rendition size does not match its catalog authority.")
			}
			body, err := io.ReadAll(io.LimitReader(stream, renditionTextMaxBytes+1))
			if err != nil || int64(len(body)) != size || !stream.Verified() {
				return nil, NewError(http.StatusInternalServerError, "rendition_text_corrupt",
					"The verified text rendition failed content verification.")
			}
			digest := sha256.Sum256(body)
			if hex.EncodeToString(digest[:]) != view.Artifact.BlobHash {
				return nil, NewError(http.StatusInternalServerError, "rendition_text_corrupt",
					"The verified text rendition digest does not match its catalog authority.")
			}
			return &huma.StreamResponse{Body: func(hctx huma.Context) {
				hctx.SetHeader("Cache-Control", "no-store")
				hctx.SetHeader("Content-Type", "text/markdown; charset=utf-8")
				hctx.SetHeader("Content-Length", strconv.FormatInt(size, 10))
				hctx.SetHeader("X-Content-Type-Options", "nosniff")
				hctx.SetHeader("X-Docbank-Rendition-SHA256", view.Artifact.BlobHash)
				hctx.SetHeader("X-Docbank-Rendition-Size", strconv.FormatInt(size, 10))
				hctx.SetHeader("X-Docbank-Rendition-Profile", in.ProfileFingerprint)
				hctx.SetHeader("X-Docbank-Rendition-Generation", in.GenerationID)
				hctx.SetHeader("X-Docbank-Rendition-Attachment", in.AttachmentID)
				hctx.SetHeader("X-Docbank-Rendition-Build", in.BuildID)
				hctx.SetHeader("X-Docbank-Rendition-Artifact", in.ArtifactID)
				hctx.SetHeader("Content-Digest", contentDigest(digest[:]))
				_, _ = hctx.BodyWriter().Write(body)
			}}, nil
		})
}
