package api

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

const (
	documentCursorVersion = 1
	documentCursorTTL     = 15 * time.Minute
	// MaxDocumentCursorBytes accommodates a full 16 KiB path without daemon-side state.
	MaxDocumentCursorBytes = 32 << 10
	documentCursorKeyBytes = 32
)

type documentQueryService struct {
	store *store.Store
	key   [documentCursorKeyBytes]byte
	now   func() time.Time
}

// Byte slices keep control characters in legal paths from inflating JSON sixfold.
// The path sort's primary value is the path itself and need not be stored twice.
type documentCursorPayload struct {
	Version   int                            `json:"v"`
	IssuedAt  int64                          `json:"iat"`
	QueryHash [sha256.Size]byte              `json:"q"`
	Path      []byte                         `json:"p"`
	Value     []byte                         `json:"x,omitempty"`
	Size      int64                          `json:"s,omitempty"`
	NodeID    int64                          `json:"n"`
	Traversal store.DocumentCatalogTraversal `json:"t"`
}

type documentCursorRequest struct {
	position  store.DocumentCatalogPosition
	traversal store.DocumentCatalogTraversal
}

func newDocumentQueryService(deps Deps) *documentQueryService {
	service := &documentQueryService{store: deps.Store, now: deps.DocumentCursorNow}
	if service.now == nil {
		service.now = time.Now
	}
	if len(deps.DocumentCursorKey) != 0 {
		if len(deps.DocumentCursorKey) != documentCursorKeyBytes {
			panic("api: DocumentCursorKey must contain exactly 32 bytes")
		}
		copy(service.key[:], deps.DocumentCursorKey)
	} else {
		_, _ = cryptorand.Read(service.key[:])
	}
	return service
}

func registerDocumentQueryRoute(api huma.API, service *documentQueryService) {
	type response struct {
		Body DocumentPage
	}
	huma.Register(api, huma.Operation{
		OperationID: "listDocuments", Method: http.MethodGet, Path: "/api/v1/documents",
		Summary: "List current live documents with authenticated keyset pagination",
	}, func(ctx context.Context, input *struct {
		PathPrefix string `query:"path_prefix" default:"/"`
		Sort       string `query:"sort" default:"path" enum:"path,name,modified_at,size,media_type"`
		Direction  string `query:"direction" default:"asc" enum:"asc,desc"`
		PageSize   int    `query:"page_size" default:"50" minimum:"1" maximum:"250"`
		Cursor     string `query:"cursor"`
	}) (*response, error) {
		query, err := store.NormalizeDocumentCatalogQuery(store.DocumentCatalogQuery{
			PathPrefix: input.PathPrefix, Sort: store.DocumentCatalogSort(input.Sort),
			Direction: store.DocumentCatalogDirection(input.Direction), PageSize: input.PageSize,
		})
		if err != nil {
			return nil, FromStoreError(err)
		}
		var boundary *store.DocumentCatalogPosition
		traversal := store.DocumentCatalogTraversalNext
		if input.Cursor != "" {
			position, cursorTraversal, decodeErr := service.decodeCursor(input.Cursor, query)
			if decodeErr != nil {
				return nil, FromStoreError(decodeErr)
			}
			boundary, traversal = &position, cursorTraversal
		}
		page, err := service.store.ListDocuments(ctx, query, boundary, traversal)
		if err != nil {
			return nil, FromStoreError(err)
		}
		wire, err := service.toDocumentPage(page)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &response{Body: wire}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "listScopedDocuments", Method: http.MethodPost, Path: "/api/v1/documents/scoped",
		Summary: "List source-fenced live documents with authenticated keyset pagination",
	}, func(ctx context.Context, input *struct {
		Body ScopedDocumentQuery
	}) (*response, error) {
		query, err := store.NormalizeDocumentCatalogQuery(store.DocumentCatalogQuery{
			PathPrefix: input.Body.PathPrefix, Sort: store.DocumentCatalogSort(input.Body.Sort),
			Direction: store.DocumentCatalogDirection(input.Body.Direction), PageSize: input.Body.PageSize,
			SourceIDs: input.Body.ContentVersionIDs,
		})
		if err != nil {
			return nil, FromStoreError(err)
		}
		var boundary *store.DocumentCatalogPosition
		traversal := store.DocumentCatalogTraversalNext
		if input.Body.Cursor != "" {
			position, cursorTraversal, decodeErr := service.decodeCursor(input.Body.Cursor, query)
			if decodeErr != nil {
				return nil, FromStoreError(decodeErr)
			}
			boundary, traversal = &position, cursorTraversal
		}
		page, err := service.store.ListDocuments(ctx, query, boundary, traversal)
		if err != nil {
			return nil, FromStoreError(err)
		}
		wire, err := service.toDocumentPage(page)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &response{Body: wire}, nil
	})
	type resolveResponse struct {
		Body DocumentSummaryResolveResponse
	}
	huma.Register(api, huma.Operation{
		OperationID: "resolveDocumentSummaries", Method: http.MethodPost, Path: "/api/v1/documents/resolve",
		Summary: "Resolve bounded exact current live document summaries",
	}, func(ctx context.Context, input *struct {
		Body DocumentSummaryResolveRequest
	}) (*resolveResponse, error) {
		identities := make([]store.DocumentCatalogIdentity, len(input.Body.Identities))
		for index, identity := range input.Body.Identities {
			identities[index] = store.DocumentCatalogIdentity{NodeID: identity.NodeID,
				ContentVersionID: identity.ContentVersionID, Path: identity.Path}
		}
		items, err := service.store.ResolveDocumentSummaries(ctx, identities)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &resolveResponse{Body: DocumentSummaryResolveResponse{
			Items: documentSummariesFromStore(items),
		}}, nil
	})
}

func (service *documentQueryService) toDocumentPage(
	page store.DocumentCatalogPage,
) (DocumentPage, error) {
	wire := DocumentPage{PathPrefix: page.Query.PathPrefix, Sort: string(page.Query.Sort),
		Direction: string(page.Query.Direction), PageSize: page.Query.PageSize,
		Items: documentSummariesFromStore(page.Items)}
	requests := make([]documentCursorRequest, 0, 2)
	if page.HasNext {
		requests = append(requests, documentCursorRequest{
			position: page.LastPosition, traversal: store.DocumentCatalogTraversalNext})
	}
	if page.HasPrevious {
		requests = append(requests, documentCursorRequest{
			position: page.FirstPosition, traversal: store.DocumentCatalogTraversalPrevious})
	}
	cursors, err := service.encodeCursors(page.Query, requests)
	if err != nil {
		return DocumentPage{}, err
	}
	next := 0
	if page.HasNext {
		wire.NextCursor = cursors[next]
		next++
	}
	if page.HasPrevious {
		wire.PreviousCursor = cursors[next]
	}
	return wire, nil
}

func documentSummariesFromStore(items []store.DocumentSummary) []DocumentSummary {
	wire := make([]DocumentSummary, len(items))
	for index, item := range items {
		wireItem := DocumentSummary{NodeID: item.NodeID, ContentVersionID: item.ContentVersionID,
			Path: item.Path, Name: item.Name, MediaType: item.MediaType, Size: item.Size,
			ModifiedAt: item.ModifiedAt, LatestProcessingState: item.LatestProcessingState,
			ActiveRenditions: make([]DocumentRenditionIdentity, len(item.ActiveRenditions))}
		for renditionIndex, rendition := range item.ActiveRenditions {
			wireItem.ActiveRenditions[renditionIndex] = DocumentRenditionIdentity{
				ProfileFingerprint: rendition.ProfileFingerprint, AttachmentID: rendition.AttachmentID,
				BuildID: rendition.BuildID,
			}
		}
		wire[index] = wireItem
	}
	return wire
}

func documentCursorQueryHash(query store.DocumentCatalogQuery) [sha256.Size]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%q %s %s %d %q",
		query.PathPrefix, query.Sort, query.Direction, query.PageSize, query.SourceIDs)))
}

func (service *documentQueryService) encodeCursors(
	query store.DocumentCatalogQuery, requests []documentCursorRequest,
) ([]string, error) {
	cursors := make([]string, 0, len(requests))
	for _, request := range requests {
		value := request.position.Value
		if query.Sort == store.DocumentCatalogSortPath {
			value = ""
		}
		payload, err := json.Marshal(documentCursorPayload{
			Version: documentCursorVersion, IssuedAt: service.now().Unix(),
			QueryHash: documentCursorQueryHash(query), Path: []byte(request.position.Path),
			Value: []byte(value), Size: request.position.Size, NodeID: request.position.NodeID,
			Traversal: request.traversal,
		})
		if err != nil {
			return nil, fmt.Errorf("encoding document cursor: %w", err)
		}
		mac := hmac.New(sha256.New, service.key[:])
		_, _ = mac.Write(payload)
		cursor := base64.RawURLEncoding.EncodeToString(payload) + "." +
			base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if len(cursor) > MaxDocumentCursorBytes {
			return nil, fmt.Errorf("%w: cursor exceeds encoded size bound", store.ErrInvalidDocumentQuery)
		}
		cursors = append(cursors, cursor)
	}
	return cursors, nil
}

func (service *documentQueryService) decodeCursor(
	raw string, query store.DocumentCatalogQuery,
) (store.DocumentCatalogPosition, store.DocumentCatalogTraversal, error) {
	invalid := func(detail string) (store.DocumentCatalogPosition, store.DocumentCatalogTraversal, error) {
		return store.DocumentCatalogPosition{}, "", fmt.Errorf("%w: %s", store.ErrInvalidDocumentCursor, detail)
	}
	if len(raw) > MaxDocumentCursorBytes {
		return invalid("encoded length exceeds bound")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return invalid("malformed envelope")
	}
	strictBase64 := base64.RawURLEncoding.Strict()
	payload, err := strictBase64.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		return invalid("malformed payload")
	}
	signature, err := strictBase64.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size ||
		base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return invalid("malformed signature")
	}
	mac := hmac.New(sha256.New, service.key[:])
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return invalid("authentication failed")
	}
	var cursor documentCursorPayload
	if err := json.Unmarshal(payload, &cursor, json.RejectUnknownMembers(true)); err != nil {
		return invalid("malformed payload")
	}
	if cursor.Version != documentCursorVersion {
		return invalid("unsupported version")
	}
	now := service.now()
	issued := time.Unix(cursor.IssuedAt, 0)
	if now.Before(issued) {
		return invalid("issued in the future")
	}
	if !now.Before(issued.Add(documentCursorTTL)) {
		return store.DocumentCatalogPosition{}, "", store.ErrDocumentCursorExpired
	}
	if cursor.QueryHash != documentCursorQueryHash(query) {
		return invalid("query binding mismatch")
	}
	if cursor.Traversal != store.DocumentCatalogTraversalNext &&
		cursor.Traversal != store.DocumentCatalogTraversalPrevious {
		return invalid("invalid traversal")
	}
	position := store.DocumentCatalogPosition{Path: string(cursor.Path), Value: string(cursor.Value),
		NodeID: cursor.NodeID, Size: cursor.Size}
	if query.Sort == store.DocumentCatalogSortPath {
		position.Value = position.Path
	}
	if !validDocumentCursorPosition(query.Sort, position) {
		return invalid("invalid position")
	}
	return position, cursor.Traversal, nil
}

func validDocumentCursorPosition(
	sortBy store.DocumentCatalogSort, position store.DocumentCatalogPosition,
) bool {
	if position.NodeID <= 0 || !strings.HasPrefix(position.Path, "/") ||
		len(position.Path) > store.MaxWalkPathBytes {
		return false
	}
	switch sortBy {
	case store.DocumentCatalogSortPath:
		return position.Value == position.Path
	case store.DocumentCatalogSortName, store.DocumentCatalogSortModifiedAt:
		return position.Value != ""
	case store.DocumentCatalogSortSize:
		return position.Size >= 0
	case store.DocumentCatalogSortMediaType:
		return true
	default:
		return false
	}
}
