package daemonconn

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

const maxDocumentSummaryResolutions = 100

// ListDocuments returns one typed page from the daemon-owned current/live
// document catalog. Cursor contents remain opaque to the client.
func (c *Connection) ListDocuments(
	ctx context.Context, query api.DocumentQuery,
) (api.DocumentPage, error) {
	if err := validateDocumentQuery(query); err != nil {
		return api.DocumentPage{}, err
	}
	params := apiclient.ListDocumentsQuery{}
	if query.PathPrefix != "" {
		params.PathPrefix = new(query.PathPrefix)
	}
	if query.Sort != "" {
		params.Sort = new(apiclient.ListDocumentsQuerySort(query.Sort))
	}
	if query.Direction != "" {
		params.Direction = new(apiclient.ListDocumentsQueryDirection(query.Direction))
	}
	if query.PageSize != 0 {
		params.PageSize = new(int64(query.PageSize))
	}
	if query.Cursor != "" {
		params.Cursor = new(query.Cursor)
	}
	var page api.DocumentPage
	apiResponse, err := c.API().ListDocuments(ctx, &apiclient.ListDocumentsRequestOptions{Query: &params})
	if err != nil {
		return api.DocumentPage{}, err
	}
	page = *apiResponse
	if err := validateDocumentPage(query, page); err != nil {
		return api.DocumentPage{}, err
	}
	return page, nil
}

// ListScopedDocuments submits a bounded source fence in the request body so
// the daemon can filter catalog candidates before applying its page limit.
func (c *Connection) ListScopedDocuments(
	ctx context.Context, query api.ScopedDocumentQuery,
) (api.DocumentPage, error) {
	if err := validateDocumentQuery(query.DocumentQuery); err != nil {
		return api.DocumentPage{}, err
	}
	if len(query.ContentVersionIDs) == 0 || len(query.ContentVersionIDs) > store.MaxDocumentCatalogSourceIDs {
		return api.DocumentPage{}, store.ErrInvalidDocumentQuery
	}
	allowed := make(map[string]bool, len(query.ContentVersionIDs))
	for _, id := range query.ContentVersionIDs {
		if !validUUIDv4(id) {
			return api.DocumentPage{}, store.ErrInvalidDocumentQuery
		}
		allowed[id] = true
	}
	response, err := c.API().ListScopedDocuments(ctx, &apiclient.ListScopedDocumentsRequestOptions{Body: &query})
	if err != nil {
		return api.DocumentPage{}, err
	}
	page := *response
	if err := validateDocumentPage(query.DocumentQuery, page); err != nil {
		return api.DocumentPage{}, err
	}
	for _, item := range page.Items {
		if !allowed[item.ContentVersionID] {
			return api.DocumentPage{}, errors.New("scoped document page returned an out-of-fence source")
		}
	}
	return page, nil
}

// ResolveDocumentSummaries returns the ordered current/live summaries for an
// exact bounded identity set. A mismatched daemon response is rejected before
// it can be used to construct a resource link.
func (c *Connection) ResolveDocumentSummaries(
	ctx context.Context, request api.DocumentSummaryResolveRequest,
) ([]api.DocumentSummary, error) {
	if err := validateDocumentSummaryResolveRequest(request); err != nil {
		return nil, err
	}
	var response api.DocumentSummaryResolveResponse
	apiResponse, err := c.API().ResolveDocumentSummaries(ctx, &apiclient.ResolveDocumentSummariesRequestOptions{Body: &request})
	if err != nil {
		return nil, err
	}
	response = *apiResponse
	if len(response.Items) != len(request.Identities) {
		return nil, errors.New("document summary resolution response has an invalid item count")
	}
	for index, item := range response.Items {
		identity := request.Identities[index]
		if err := validateDocumentSummary(item, index); err != nil || item.NodeID != identity.NodeID ||
			item.ContentVersionID != identity.ContentVersionID || item.Path != identity.Path {
			return nil, errors.New("document summary resolution response does not bind its identities")
		}
	}
	return response.Items, nil
}

func validateDocumentSummaryResolveRequest(request api.DocumentSummaryResolveRequest) error {
	if len(request.Identities) == 0 || len(request.Identities) > maxDocumentSummaryResolutions {
		return store.ErrInvalidDocumentQuery
	}
	seen := make(map[api.DocumentIdentity]struct{}, len(request.Identities))
	for _, identity := range request.Identities {
		if identity.NodeID <= 0 || !validUUIDv4(identity.ContentVersionID) ||
			!strings.HasPrefix(identity.Path, "/") || len(identity.Path) > store.MaxWalkPathBytes {
			return store.ErrInvalidDocumentQuery
		}
		if _, duplicate := seen[identity]; duplicate {
			return store.ErrInvalidDocumentQuery
		}
		seen[identity] = struct{}{}
	}
	return nil
}

func validateDocumentQuery(query api.DocumentQuery) error {
	_, err := normalizeClientDocumentQuery(query)
	if err != nil {
		return err
	}
	if len(query.Cursor) > api.MaxDocumentCursorBytes {
		return store.ErrInvalidDocumentCursor
	}
	return nil
}

func validateDocumentPage(query api.DocumentQuery, page api.DocumentPage) error {
	normalized, err := normalizeClientDocumentQuery(query)
	if err != nil {
		return err
	}
	if page.PathPrefix != normalized.PathPrefix || page.Sort != string(normalized.Sort) ||
		page.Direction != string(normalized.Direction) || page.PageSize != normalized.PageSize ||
		len(page.Items) > page.PageSize {
		return errors.New("document catalog response does not bind its query")
	}
	if len(page.NextCursor) > api.MaxDocumentCursorBytes ||
		len(page.PreviousCursor) > api.MaxDocumentCursorBytes {
		return errors.New("document catalog response contains an oversized cursor")
	}
	seen := make(map[int64]struct{}, len(page.Items))
	for index, item := range page.Items {
		if err := validateDocumentSummary(item, index); err != nil ||
			!clientDocumentPathWithinPrefix(item.Path, page.PathPrefix) {
			return fmt.Errorf("document catalog response item %d has invalid identity", index)
		}
		if _, duplicate := seen[item.NodeID]; duplicate {
			return fmt.Errorf("document catalog response repeats node %d", item.NodeID)
		}
		seen[item.NodeID] = struct{}{}
	}
	return nil
}

func validateDocumentSummary(item api.DocumentSummary, index int) error {
	if item.NodeID <= 0 || !validUUIDv4(item.ContentVersionID) || item.Size < 0 ||
		!strings.HasPrefix(item.Path, "/") || pathpkg.Base(item.Path) != item.Name {
		return fmt.Errorf("document summary response item %d has invalid identity", index)
	}
	if _, err := time.Parse(time.RFC3339Nano, item.ModifiedAt); err != nil {
		return fmt.Errorf("document summary response item %d has invalid modified time", index)
	}
	for renditionIndex, rendition := range item.ActiveRenditions {
		if !validSHA256Hex(rendition.ProfileFingerprint) ||
			!validSHA256Hex(rendition.AttachmentID) || !validSHA256Hex(rendition.BuildID) {
			return fmt.Errorf("document summary response item %d rendition %d has invalid identity",
				index, renditionIndex)
		}
	}
	return nil
}

func normalizeClientDocumentQuery(query api.DocumentQuery) (store.DocumentCatalogQuery, error) {
	return store.NormalizeDocumentCatalogQuery(store.DocumentCatalogQuery{
		PathPrefix: query.PathPrefix, Sort: store.DocumentCatalogSort(query.Sort),
		Direction: store.DocumentCatalogDirection(query.Direction), PageSize: query.PageSize,
	})
}

func clientDocumentPathWithinPrefix(path, prefix string) bool {
	return prefix == "/" || path == prefix || strings.HasPrefix(path, prefix+"/")
}
