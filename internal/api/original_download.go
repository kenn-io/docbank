package api

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/docbank/internal/store"
)

// OriginalDownloadSelection binds a capability to an exact live document selection.
type OriginalDownloadSelection struct {
	VaultID  string `json:"vault_id" format:"uuid"`
	NodeID   int64  `json:"node_id" minimum:"1"`
	BlobHash string `json:"blob_hash" pattern:"^[0-9a-f]{64}$"`
	Size     int64  `json:"size" minimum:"0"`
}

// Matches reports whether the selected live node and version still agree.
func (s OriginalDownloadSelection) Matches(vaultID, versionID string, node Node, version ContentVersion) bool {
	return node.ID == s.NodeID && version.ID == versionID && version.NodeID == s.NodeID && strings.EqualFold(s.VaultID, vaultID) && node.Kind == "file" && node.TrashedAt == "" && version.BlobHash == s.BlobHash && version.Size == s.Size
}

// OriginalDownloadTicket contains a short-lived capability, never a staged copy.
type OriginalDownloadTicket struct {
	URL              string `json:"url"`
	Name             string `json:"name"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

type originalDownloadSelection struct {
	OriginalDownloadSelection

	VersionID string
}

func (s originalDownloadSelection) check(ctx context.Context, d Deps) (store.ContentVersionView, error) {
	view, err := d.Store.ContentVersionViewByID(ctx, s.NodeID, s.VersionID)
	if err != nil {
		return view, FromStoreError(err)
	}
	if !s.Matches(d.Store.VaultID(), s.VersionID, fromStoreNode(view.Node), fromStoreContentVersion(view.Version)) {
		return view, NewError(http.StatusConflict, "download_selection_stale", "the selected document or version changed; refresh it before downloading")
	}
	return view, nil
}

func registerOriginalDownload(api huma.API, d Deps, downloads *webDownloadRegistry) {
	huma.Register(api, huma.Operation{
		OperationID: "prepareOriginalDownload", Method: http.MethodPost,
		Path:    "/api/v1/versions/{version_id}/download-ticket",
		Summary: "Issue a single-use original download URL valid for two minutes",
	}, func(ctx context.Context, in *struct {
		VersionID string `path:"version_id" format:"uuid"`
		Body      OriginalDownloadSelection
	}) (*struct{ Body OriginalDownloadTicket }, error) {
		selection := originalDownloadSelection{OriginalDownloadSelection: in.Body, VersionID: in.VersionID}
		view, err := selection.check(ctx, d)
		if err != nil {
			return nil, err
		}
		owner, _ := workspaceSnapshotOwner(ctx)
		token, err := downloads.issue(webDownloadTicket{original: &selection, owner: owner, name: view.Node.Name})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", "could not issue original download")
		}
		return &struct{ Body OriginalDownloadTicket }{Body: OriginalDownloadTicket{
			URL: webDownloadFilePath + "?ticket=" + token, Name: view.Node.Name, ExpiresInSeconds: int(webDownloadTicketTTL / time.Second),
		}}, nil
	})
}

func deliverOriginalDownload(w http.ResponseWriter, r *http.Request, d Deps, ticket webDownloadTicket) {
	view, err := ticket.original.check(r.Context(), d)
	var stream *huma.StreamResponse
	if err == nil {
		stream, err = streamContentVersionWithFailure(r.Context(), d, view.Version, true)
	}
	if err != nil {
		problem, ok := errors.AsType[*Error](err)
		if !ok {
			problem = webDownloadProblem(err)
		}
		writeError(w, problem)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": ticket.name}))
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	stream.Body(humago.NewContext(&huma.Operation{}, r, w))
}
