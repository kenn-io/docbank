package api

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

const (
	documentCursorVersion       = 1
	documentCursorTTL           = 15 * time.Minute
	maxEncodedDocumentCursorLen = 2048
	documentCursorKeyBytes      = 32
	documentCursorHandleBytes   = 16
	documentCursorExpiryBytes   = 8
	documentCursorPayloadBytes  = 1 + documentCursorExpiryBytes + documentCursorHandleBytes
	maxLiveDocumentCursors      = 4096
	documentCursorRandomRetries = 16
)

type documentQueryService struct {
	store   *store.Store
	key     [documentCursorKeyBytes]byte
	now     func() time.Time
	random  func([]byte) error
	cursors documentCursorRegistry
}

type documentCursorHandle [documentCursorHandleBytes]byte

type documentCursorRegistry struct {
	mu      sync.Mutex
	entries map[documentCursorHandle]documentCursorEntry
}

type documentCursorEntry struct {
	query     store.DocumentCatalogQuery
	position  store.DocumentCatalogPosition
	traversal store.DocumentCatalogTraversal
	issuedAt  time.Time
	expiresAt time.Time
}

type documentCursorRequest struct {
	position  store.DocumentCatalogPosition
	traversal store.DocumentCatalogTraversal
}

type preparedDocumentCursor struct {
	handle  documentCursorHandle
	request documentCursorRequest
}

func newDocumentQueryService(deps Deps) *documentQueryService {
	service := &documentQueryService{store: deps.Store, now: deps.DocumentCursorNow,
		random: deps.DocumentCursorRandom}
	if service.now == nil {
		service.now = time.Now
	}
	if service.random == nil {
		service.random = func(destination []byte) error {
			_, err := cryptorand.Read(destination)
			if err != nil {
				return fmt.Errorf("reading cursor randomness: %w", err)
			}
			return nil
		}
	}
	service.cursors.entries = make(map[documentCursorHandle]documentCursorEntry)
	if len(deps.DocumentCursorKey) != 0 {
		if len(deps.DocumentCursorKey) != documentCursorKeyBytes {
			panic("api: DocumentCursorKey must contain exactly 32 bytes")
		}
		copy(service.key[:], deps.DocumentCursorKey)
		return service
	}
	if err := service.random(service.key[:]); err != nil {
		panic(fmt.Sprintf("api: generating document cursor key: %v", err))
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

func (service *documentQueryService) encodeCursors(
	query store.DocumentCatalogQuery, requests []documentCursorRequest,
) ([]string, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	now := service.now().UTC().Truncate(time.Second)
	service.cursors.mu.Lock()
	defer service.cursors.mu.Unlock()
	for handle, entry := range service.cursors.entries {
		if !now.Before(entry.expiresAt) {
			delete(service.cursors.entries, handle)
		}
	}
	if len(service.cursors.entries)+len(requests) > maxLiveDocumentCursors {
		return nil, store.ErrDocumentCursorCapacity
	}

	prepared := make([]preparedDocumentCursor, 0, len(requests))
	for _, request := range requests {
		var handle documentCursorHandle
		unique := false
		for range documentCursorRandomRetries {
			if err := service.random(handle[:]); err != nil {
				return nil, fmt.Errorf("generating document cursor handle: %w", err)
			}
			if _, exists := service.cursors.entries[handle]; exists {
				continue
			}
			if slicesContainsPreparedDocumentCursorHandle(prepared, handle) {
				continue
			}
			unique = true
			break
		}
		if !unique {
			return nil, fmt.Errorf("generating unique document cursor handle after %d attempts",
				documentCursorRandomRetries)
		}
		prepared = append(prepared, preparedDocumentCursor{handle: handle, request: request})
	}

	encoded := make([]string, 0, len(prepared))
	for _, cursor := range prepared {
		wire, err := service.encodeDocumentCursorHandle(cursor.handle, now.Add(documentCursorTTL))
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, wire)
	}
	for _, cursor := range prepared {
		service.cursors.entries[cursor.handle] = documentCursorEntry{
			query: query, position: cursor.request.position, traversal: cursor.request.traversal,
			issuedAt: now, expiresAt: now.Add(documentCursorTTL),
		}
	}
	return encoded, nil
}

func slicesContainsPreparedDocumentCursorHandle(
	prepared []preparedDocumentCursor, want documentCursorHandle,
) bool {
	return slices.ContainsFunc(prepared, func(cursor preparedDocumentCursor) bool {
		return cursor.handle == want
	})
}

func (service *documentQueryService) encodeDocumentCursorHandle(
	handle documentCursorHandle, expiresAt time.Time,
) (string, error) {
	payload := make([]byte, documentCursorPayloadBytes)
	payload[0] = documentCursorVersion
	if _, err := binary.Encode(payload[1:1+documentCursorExpiryBytes], binary.BigEndian,
		expiresAt.Unix()); err != nil {
		return "", fmt.Errorf("encoding document cursor expiry: %w", err)
	}
	copy(payload[1+documentCursorExpiryBytes:], handle[:])
	mac := hmac.New(sha256.New, service.key[:])
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (service *documentQueryService) decodeCursor(
	raw string, query store.DocumentCatalogQuery,
) (store.DocumentCatalogPosition, store.DocumentCatalogTraversal, error) {
	invalid := func(detail string) (store.DocumentCatalogPosition, store.DocumentCatalogTraversal, error) {
		return store.DocumentCatalogPosition{}, "",
			fmt.Errorf("%w: %s", store.ErrInvalidDocumentCursor, detail)
	}
	if len(raw) > maxEncodedDocumentCursorLen {
		return invalid("encoded length exceeds 2048 bytes")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return invalid("malformed envelope")
	}
	strictBase64 := base64.RawURLEncoding.Strict()
	payload, err := strictBase64.DecodeString(parts[0])
	if err != nil || len(payload) != documentCursorPayloadBytes ||
		base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
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
	if payload[0] != documentCursorVersion {
		return invalid("unsupported version")
	}
	wireExpirySeconds := binary.BigEndian.Uint64(payload[1 : 1+documentCursorExpiryBytes])
	if wireExpirySeconds > math.MaxInt64 {
		return invalid("invalid expiry")
	}
	wireExpiry := time.Unix(int64(wireExpirySeconds), 0).UTC()
	now := service.now().UTC()
	if !now.Before(wireExpiry) {
		return store.DocumentCatalogPosition{}, "", store.ErrDocumentCursorExpired
	}
	var handle documentCursorHandle
	copy(handle[:], payload[1+documentCursorExpiryBytes:])
	service.cursors.mu.Lock()
	entry, exists := service.cursors.entries[handle]
	if exists && !entry.expiresAt.Equal(wireExpiry) {
		service.cursors.mu.Unlock()
		return invalid("registry expiry mismatch")
	}
	for expiredHandle, candidate := range service.cursors.entries {
		if !now.Before(candidate.expiresAt) {
			delete(service.cursors.entries, expiredHandle)
		}
	}
	service.cursors.mu.Unlock()
	if !exists {
		return invalid("unknown handle")
	}
	if now.Before(entry.issuedAt) {
		return invalid("issued in the future")
	}
	if entry.query != query {
		return invalid("query binding mismatch")
	}
	if entry.traversal != store.DocumentCatalogTraversalNext &&
		entry.traversal != store.DocumentCatalogTraversalPrevious {
		return invalid("invalid traversal")
	}
	if !validDocumentCursorPosition(query.Sort, entry.position) {
		return invalid("invalid position")
	}
	return entry.position, entry.traversal, nil
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
