package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func registerBatesRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate, downloads *webDownloadRegistry,
	sessions *webSessionRegistry, cursors *documentQueryService,
) {
	huma.Register(api, huma.Operation{OperationID: "createBatesNamespace", Method: http.MethodPost,
		Path: "/api/v1/bates/namespaces", Summary: "Create or find a Bates namespace", DefaultStatus: http.StatusCreated,
		MaxBodyBytes: 4096}, func(ctx context.Context, in *struct{ Body BatesNamespaceRequest }) (*struct{ Body BatesNamespace }, error) {
		var result store.BatesNamespace
		err := g.mutate(func() error {
			var err error
			result, err = d.Store.EnsureBatesNamespace(ctx, in.Body.Prefix, in.Body.Suffix, in.Body.Padding)
			return err
		})
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body BatesNamespace }{Body: batesNamespaceDTO(result)}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "listBatesNamespaces", Method: http.MethodGet,
		Path: "/api/v1/bates/namespaces", Summary: "List Bates namespaces"}, func(ctx context.Context, in *struct {
		Cursor string `query:"cursor"`
		Limit  int    `query:"limit" minimum:"0" maximum:"250"`
	}) (*struct{ Body BatesNamespacePage }, error) {
		limit := in.Limit
		if limit == 0 {
			limit = 100
		}
		items, total, next, err := d.Store.BatesNamespaces(ctx, in.Cursor, limit)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := BatesNamespacePage{Items: make([]BatesNamespace, len(items)), Total: total, NextCursor: next}
		for i, item := range items {
			out.Items[i] = batesNamespaceDTO(item)
		}
		return &struct{ Body BatesNamespacePage }{Body: out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "planBatesStamp", Method: http.MethodPost,
		Path: "/api/v1/bates/preview", Summary: "Preview tentative Bates labels without stamping or reserving",
		MaxBodyBytes: 128 << 10}, func(ctx context.Context, in *struct{ Body BatesPlanRequest }) (*struct{ Body BatesPlan }, error) {
		request, err := BindBatesPlan(ctx, d.Store, in.Body)
		if err != nil {
			return nil, FromStoreError(err)
		}
		plan, err := d.Store.PreviewBatesRange(ctx, request)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body BatesPlan }{Body: BatesPlan{Namespace: batesNamespaceDTO(plan.Namespace),
			StartSequence: plan.StartSequence, EndSequence: plan.EndSequence,
			Labels: batesLabelsDTO(plan.Labels), StampedNothing: true}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "reserveBatesRange", Method: http.MethodPost,
		Path: "/api/v1/bates/allocations", Summary: "Reserve one idempotent Bates range", DefaultStatus: http.StatusCreated,
		MaxBodyBytes: 128 << 10}, func(ctx context.Context, in *struct{ Body BatesReserveRequest }) (*struct{ Body BatesAllocation }, error) {
		request, err := BindBatesReservation(ctx, d.Store, in.Body)
		if err != nil {
			return nil, FromStoreError(err)
		}
		var allocation store.BatesAllocation
		err = g.mutate(func() error {
			var err error
			allocation, err = d.Store.ReserveBatesRange(ctx, request)
			return err
		})
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body BatesAllocation }{Body: batesAllocationDTO(allocation)}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "readBatesAllocation", Method: http.MethodGet,
		Path: "/api/v1/bates/allocations/{id}", Summary: "Read a Bates allocation"}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{ Body BatesAllocation }, error) {
		allocation, err := d.Store.BatesAllocation(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body BatesAllocation }{Body: batesAllocationDTO(allocation)}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "publishBatesExport", Method: http.MethodPost,
		Path: "/api/v1/bates/exports", Summary: "Publish a verified Bates export", DefaultStatus: http.StatusCreated,
		MaxBodyBytes: 16 << 10}, func(ctx context.Context, in *struct{ Body BatesExportRequest }) (*struct{ Body BatesExport }, error) {
		if in.Body.AllocationID == "" {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "allocation_id is required")
		}
		var artifact store.BatesArtifact
		err := g.MutateContext(ctx, func() error {
			var err error
			artifact, err = processing.PublishBatesExport(ctx, d.Store, d.Blobs, in.Body.AllocationID, in.Body.Recipe)
			return err
		})
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body BatesExport }{Body: batesExportDTO(artifact)}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "readBatesExport", Method: http.MethodGet,
		Path: "/api/v1/bates/exports/{id}", Summary: "Read a verified Bates export"}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{ Body BatesExport }, error) {
		artifact, err := d.Store.BatesArtifact(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body BatesExport }{Body: batesExportDTO(artifact)}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "findBatesExports", Method: http.MethodGet,
		Path: "/api/v1/bates/exports/candidates", Summary: "Find verified Bates export candidates"}, func(ctx context.Context, in *struct {
		BatesLabel     string `query:"bates_label" maxLength:"256"`
		CustodianLabel string `query:"custodian_label" maxLength:"200"`
		PersonID       string `query:"person_id"`
		Cursor         string `query:"cursor" maxLength:"4096"`
		Limit          int    `query:"limit" minimum:"0" maximum:"250"`
	}) (*struct{ Body BatesCandidatePage }, error) {
		selector := store.BatesArtifactSelector{BatesLabel: in.BatesLabel,
			CustodianLabel: in.CustodianLabel, PersonID: in.PersonID}
		key, value := batesSelectorIdentity(selector)
		if key == "" {
			return nil, FromStoreError(store.ErrInvalidBatesSelector)
		}
		position, evidenceArtifactID, evidenceOffset, cursorEpoch, err := decodeBatesCandidateCursor(cursors, in.Cursor, key, value)
		if err != nil {
			return nil, FromStoreError(err)
		}
		limit := in.Limit
		if limit == 0 {
			limit = 100
		}
		var page store.BatesArtifactCandidatePage
		if evidenceArtifactID != "" {
			page, err = d.Store.FindBatesArtifactEvidence(ctx, selector, evidenceArtifactID, evidenceOffset)
		} else {
			page, err = d.Store.FindBatesArtifacts(ctx, selector, position, limit)
		}
		if err != nil {
			return nil, FromStoreError(err)
		}
		if in.Cursor != "" && cursorEpoch != page.BindingEpoch {
			return nil, NewError(http.StatusConflict, "stale_bates_cursor", "person bindings changed; restart candidate discovery")
		}
		out := BatesCandidatePage{Items: make([]BatesCandidate, len(page.Items))}
		for index, item := range page.Items {
			out.Items[index] = batesCandidateDTO(item)
			if item.EvidenceNext > 0 {
				out.Items[index].EvidenceCursor, err = encodeBatesEvidenceCursor(cursors, key, value,
					page.BindingEpoch, item.ArtifactID, item.EvidenceNext)
				if err != nil {
					return nil, FromStoreError(err)
				}
			}
		}
		if evidenceArtifactID == "" && page.Next.ArtifactID != "" {
			out.NextCursor, err = encodeBatesCandidateCursor(cursors, key, value, page.BindingEpoch, page.Next)
			if err != nil {
				return nil, FromStoreError(err)
			}
		}
		return &struct{ Body BatesCandidatePage }{Body: out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "listBatesExports", Method: http.MethodGet,
		Path: "/api/v1/bates/exports", Summary: "List verified Bates export history"}, func(ctx context.Context, in *struct {
		After string `query:"after"`
		Limit int    `query:"limit" minimum:"0" maximum:"250"`
	}) (*struct{ Body BatesExportPage }, error) {
		limit := in.Limit
		if limit == 0 {
			limit = 100
		}
		items, err := d.Store.BatesArtifacts(ctx, in.After, limit)
		if err != nil {
			return nil, FromStoreError(err)
		}
		total, err := d.Store.BatesArtifactCount(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := BatesExportPage{Items: make([]BatesExport, len(items)), Total: total}
		for index, item := range items {
			out.Items[index] = batesExportDTO(item)
		}
		if len(items) == limit {
			out.NextAfter = items[len(items)-1].ArtifactID
		}
		return &struct{ Body BatesExportPage }{Body: out}, nil
	})
	type downloadOutput struct{ Body BatesDownloadTicket }
	huma.Register(api, huma.Operation{OperationID: "downloadBatesExport", Method: http.MethodPost,
		Path: "/api/v1/bates/exports/{id}/download", Summary: "Issue a one-use ticket for a hash-checked Bates export",
		MaxBodyBytes: 1024}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct{}
	}) (*downloadOutput, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		data, artifact, err := processing.ReadBatesExport(ctx, d.Store, d.Blobs, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		file, path, err := downloads.createStagingFile()
		if err != nil {
			return nil, FromStoreError(err)
		}
		keep := false
		defer func() {
			if !keep {
				_ = file.Close()
				_ = os.Remove(path)
			}
		}()
		if _, err = file.Write(data); err != nil {
			return nil, FromStoreError(err)
		}
		if err = file.Sync(); err != nil {
			return nil, FromStoreError(err)
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return nil, FromStoreError(err)
		}
		name := fmt.Sprintf("bates-%s.pdf", artifact.AllocationID)
		ticket := webDownloadTicket{path: path, name: name, mediaType: "application/pdf",
			blobHash: artifact.BlobSHA256, size: artifact.Size, owner: owner, archiveFile: file,
			releaseArchive: func() { _ = file.Close(); _ = os.Remove(path) }}
		var token string
		if browserSessionRequest(ctx) {
			active, issueErr := sessions.withActiveOwner(owner, func() error {
				var issueErr error
				token, issueErr = downloads.issue(ticket)
				return issueErr
			})
			if !active && issueErr == nil {
				issueErr = errors.New("browser session was revoked before Bates download publication")
			}
			err = issueErr
		} else {
			token, err = downloads.issue(ticket)
		}
		if err != nil {
			return nil, FromStoreError(err)
		}
		keep = true
		return &downloadOutput{Body: BatesDownloadTicket{URL: webDownloadFilePath + "?ticket=" + token,
			Name: name, AllocationID: artifact.AllocationID, BlobSHA256: artifact.BlobSHA256, Size: artifact.Size}}, nil
	})
	mux.HandleFunc("GET /api/v1/bates/exports/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		data, artifact, err := processing.ReadBatesExport(r.Context(), d.Store, d.Blobs, r.PathValue("id"))
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="bates-%s.pdf"`, artifact.AllocationID))
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.Size, 10))
		w.Header().Set(BlobHashHeader, artifact.BlobSHA256)
		w.Header().Set("Content-Digest", contentDigest(mustDecodeHash(artifact.BlobSHA256)))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data) //nolint:gosec // Hash-checked PDF bytes verified at publish; attachment is PDF with nosniff.
	})
	api.OpenAPI().AddOperation(&huma.Operation{OperationID: "downloadBatesExportContent", Method: http.MethodGet,
		Path: "/api/v1/bates/exports/{id}/content", Summary: "Download hash-checked Bates export bytes",
		Parameters: []*huma.Param{{Name: "id", In: openAPIPathLocation, Required: true,
			Schema: &huma.Schema{Type: openAPIStringType, Format: "uuid"}}},
		Responses: map[string]*huma.Response{"200": {Description: "Exact retained Bates PDF",
			Content: map[string]*huma.MediaType{"application/pdf": {}}}}})
}

// BindBatesPlan resolves a preview request to its namespace and sealed pages.
func BindBatesPlan(ctx context.Context, s *store.Store, value BatesPlanRequest) (store.BatesPlanRequest, error) {
	if len(value.Pages) > store.MaxBatesExportPages {
		return store.BatesPlanRequest{}, store.ErrBatesPageLimit
	}
	namespace, err := s.BatesNamespace(ctx, value.NamespaceID, value.Prefix, value.Suffix, value.Padding)
	if err != nil {
		return store.BatesPlanRequest{}, err
	}
	request := batesPlanRequestStore(value)
	request.NamespaceID = namespace.NamespaceID
	if len(request.Pages) == 0 {
		request.Pages, err = s.SnapshotBatesPages(ctx, value.SnapshotID)
		if err != nil {
			return store.BatesPlanRequest{}, err
		}
	}
	return request, nil
}

type batesCandidateCursor struct {
	Version        int    `json:"v"`
	IssuedAt       int64  `json:"iat"`
	SelectorKind   string `json:"selector_kind"`
	SelectorValue  string `json:"selector_value"`
	BindingEpoch   int64  `json:"binding_epoch"`
	CreatedAt      string `json:"created_at"`
	ArtifactID     string `json:"artifact_id"`
	EvidenceOffset int    `json:"evidence_offset,omitzero"`
}

const batesCandidateCursorVersion = 1

func batesSelectorIdentity(selector store.BatesArtifactSelector) (string, string) {
	count := 0
	kind, value := "", ""
	if selector.BatesLabel != "" {
		kind, value, count = "bates_label", selector.BatesLabel, count+1
	}
	if selector.CustodianLabel != "" {
		kind, value, count = "custodian_label", document.FoldPersonName(selector.CustodianLabel), count+1
	}
	if selector.PersonID != "" {
		kind, value, count = "person_id", selector.PersonID, count+1
	}
	if count != 1 || value == "" {
		return "", ""
	}
	return kind, value
}

func encodeBatesCandidateCursor(service *documentQueryService, kind, value string, epoch int64,
	position store.BatesArtifactPosition,
) (string, error) {
	return encodeBatesCursor(service, batesCandidateCursor{SelectorKind: kind, SelectorValue: value,
		BindingEpoch: epoch, CreatedAt: position.CreatedAt, ArtifactID: position.ArtifactID})
}

func encodeBatesEvidenceCursor(service *documentQueryService, kind, value string, epoch int64,
	artifactID string, offset int,
) (string, error) {
	return encodeBatesCursor(service, batesCandidateCursor{SelectorKind: kind, SelectorValue: value,
		BindingEpoch: epoch, ArtifactID: artifactID, EvidenceOffset: offset})
}

func encodeBatesCursor(service *documentQueryService, cursor batesCandidateCursor) (string, error) {
	cursor.Version, cursor.IssuedAt = batesCandidateCursorVersion, service.now().Unix()
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, service.key[:])
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decodeBatesCandidateCursor(service *documentQueryService, raw, kind, value string) (
	store.BatesArtifactPosition, string, int, int64, error,
) {
	if raw == "" {
		return store.BatesArtifactPosition{}, "", 0, 0, nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return store.BatesArtifactPosition{}, "", 0, 0, store.ErrInvalidBatesCursor
	}
	strict := base64.RawURLEncoding.Strict()
	encoded, err := strict.DecodeString(parts[0])
	signature, signatureErr := strict.DecodeString(parts[1])
	mac := hmac.New(sha256.New, service.key[:])
	_, _ = mac.Write(encoded)
	if err != nil || signatureErr != nil || len(signature) != sha256.Size || !hmac.Equal(signature, mac.Sum(nil)) {
		return store.BatesArtifactPosition{}, "", 0, 0, store.ErrInvalidBatesCursor
	}
	var cursor batesCandidateCursor
	if err := json.Unmarshal(encoded, &cursor, json.RejectUnknownMembers(true)); err != nil ||
		cursor.Version != batesCandidateCursorVersion || cursor.SelectorKind != kind || cursor.SelectorValue != value ||
		cursor.ArtifactID == "" || service.now().Before(time.Unix(cursor.IssuedAt, 0)) ||
		(cursor.EvidenceOffset == 0) == (cursor.CreatedAt == "") {
		return store.BatesArtifactPosition{}, "", 0, 0, store.ErrInvalidBatesCursor
	}
	if !service.now().Before(time.Unix(cursor.IssuedAt, 0).Add(documentCursorTTL)) {
		return store.BatesArtifactPosition{}, "", 0, 0, store.ErrDocumentCursorExpired
	}
	if cursor.EvidenceOffset > 0 {
		return store.BatesArtifactPosition{}, cursor.ArtifactID, cursor.EvidenceOffset, cursor.BindingEpoch, nil
	}
	return store.BatesArtifactPosition{CreatedAt: cursor.CreatedAt, ArtifactID: cursor.ArtifactID}, "", 0,
		cursor.BindingEpoch, nil
}

func batesCandidateDTO(value store.BatesArtifactCandidate) BatesCandidate {
	evidence := make([]BatesCandidateEvidence, len(value.Evidence))
	for index, item := range value.Evidence {
		evidence[index] = BatesCandidateEvidence{Kind: item.Kind, OccurrenceID: item.OccurrenceID,
			Label: item.Label, OutputPage: item.OutputPage, AssignmentID: item.AssignmentID,
			ScopeKind: item.ScopeKind, RawLabel: item.RawLabel, PersonID: item.PersonID, Rank: item.Rank,
			Basis: item.Basis, PackageID: item.PackageID, PackageRecord: item.PackageRecord}
	}
	return BatesCandidate{ArtifactID: value.ArtifactID, AllocationID: value.AllocationID,
		SnapshotID: value.SnapshotID, BlobSHA256: value.BlobSHA256, Size: value.Size,
		MediaType: value.MediaType, PageCount: value.PageCount, ManifestSHA256: value.ManifestSHA256,
		State: value.State, CreatedAt: value.CreatedAt, Evidence: evidence, EvidenceTruncated: value.EvidenceTruncated}
}
