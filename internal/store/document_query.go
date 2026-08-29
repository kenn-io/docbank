package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	DefaultDocumentCatalogPageSize = 50
	MaxDocumentCatalogPageSize     = 250
	MaxDocumentSummaryResolutions  = 100
)

type DocumentCatalogSort string

const (
	DocumentCatalogSortPath       DocumentCatalogSort = "path"
	DocumentCatalogSortName       DocumentCatalogSort = "name"
	DocumentCatalogSortModifiedAt DocumentCatalogSort = "modified_at"
	DocumentCatalogSortSize       DocumentCatalogSort = "size"
	DocumentCatalogSortMediaType  DocumentCatalogSort = "media_type"
)

type DocumentCatalogDirection string

const (
	DocumentCatalogDirectionAscending  DocumentCatalogDirection = "asc"
	DocumentCatalogDirectionDescending DocumentCatalogDirection = "desc"
)

type DocumentCatalogTraversal string

const (
	DocumentCatalogTraversalNext     DocumentCatalogTraversal = "next"
	DocumentCatalogTraversalPrevious DocumentCatalogTraversal = "previous"
)

type DocumentCatalogQuery struct {
	PathPrefix string
	Sort       DocumentCatalogSort
	Direction  DocumentCatalogDirection
	PageSize   int
}

// DocumentCatalogPosition is the typed live-keyset boundary authenticated by
// the daemon's cursor service. It deliberately carries no source data.
type DocumentCatalogPosition struct {
	Value  string
	Size   int64
	Path   string
	NodeID int64
}

type DocumentRenditionIdentity struct {
	ProfileFingerprint string
	AttachmentID       string
	BuildID            string
}

type DocumentSummary struct {
	NodeID                int64
	ContentVersionID      string
	Path                  string
	Name                  string
	MediaType             string
	Size                  int64
	ModifiedAt            string
	LatestProcessingState string
	ActiveRenditions      []DocumentRenditionIdentity
}

// DocumentCatalogIdentity is an exact source fence for a current/live
// document. All three fields must still agree when it is resolved.
type DocumentCatalogIdentity struct {
	NodeID           int64
	ContentVersionID string
	Path             string
}

type DocumentCatalogPage struct {
	Query         DocumentCatalogQuery
	Items         []DocumentSummary
	FirstPosition DocumentCatalogPosition
	LastPosition  DocumentCatalogPosition
	HasPrevious   bool
	HasNext       bool
}

// ListDocuments returns one flattened page of current, live file nodes. The
// caller owns opaque cursor authentication; this method owns normalization,
// SQL, typed keyset positions, and live traversal semantics.
func (s *Store) ListDocuments(
	ctx context.Context,
	query DocumentCatalogQuery,
	boundary *DocumentCatalogPosition,
	traversal DocumentCatalogTraversal,
) (DocumentCatalogPage, error) {
	normalized, err := normalizeDocumentCatalogQuery(query)
	if err != nil {
		return DocumentCatalogPage{}, err
	}
	if traversal == "" {
		traversal = DocumentCatalogTraversalNext
	}
	if traversal != DocumentCatalogTraversalNext && traversal != DocumentCatalogTraversalPrevious {
		return DocumentCatalogPage{}, fmt.Errorf("%w: invalid traversal %q", ErrInvalidDocumentQuery, traversal)
	}
	if boundary == nil && traversal == DocumentCatalogTraversalPrevious {
		return DocumentCatalogPage{}, fmt.Errorf("%w: previous traversal requires a position", ErrInvalidDocumentQuery)
	}
	if boundary != nil {
		if err := validateDocumentCatalogPosition(normalized.Sort, *boundary); err != nil {
			return DocumentCatalogPage{}, err
		}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DocumentCatalogPage{}, fmt.Errorf("starting document catalog page: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	items, err := s.queryDocumentCatalogPage(ctx, tx, normalized, boundary, traversal)
	if err != nil {
		return DocumentCatalogPage{}, err
	}
	page := DocumentCatalogPage{Query: normalized, Items: items}
	if len(items) > 0 {
		page.FirstPosition = documentCatalogPosition(normalized.Sort, items[0])
		page.LastPosition = documentCatalogPosition(normalized.Sort, items[len(items)-1])
		page.HasPrevious, err = s.documentCatalogHasRows(
			ctx, tx, normalized, page.FirstPosition, DocumentCatalogTraversalPrevious)
		if err != nil {
			return DocumentCatalogPage{}, err
		}
		page.HasNext, err = s.documentCatalogHasRows(
			ctx, tx, normalized, page.LastPosition, DocumentCatalogTraversalNext)
		if err != nil {
			return DocumentCatalogPage{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return DocumentCatalogPage{}, fmt.Errorf("closing document catalog page: %w", err)
	}
	return page, nil
}

// ResolveDocumentSummaries resolves an ordered bounded set of exact
// current/live identities in one catalog query. Missing, moved, superseded,
// or repeated identities fail closed so callers cannot attach stale links.
func (s *Store) ResolveDocumentSummaries(
	ctx context.Context, identities []DocumentCatalogIdentity,
) ([]DocumentSummary, error) {
	if len(identities) == 0 || len(identities) > MaxDocumentSummaryResolutions {
		return nil, fmt.Errorf("%w: document resolution must contain 1 through %d identities",
			ErrInvalidDocumentQuery, MaxDocumentSummaryResolutions)
	}
	seen := make(map[DocumentCatalogIdentity]struct{}, len(identities))
	for _, identity := range identities {
		if identity.NodeID <= 0 || identity.ContentVersionID == "" || !strings.HasPrefix(identity.Path, "/") ||
			len(identity.Path) > MaxWalkPathBytes {
			return nil, fmt.Errorf("%w: invalid document identity", ErrInvalidDocumentQuery)
		}
		if _, duplicate := seen[identity]; duplicate {
			return nil, ErrProcessingSourceFenceStaleVersion
		}
		seen[identity] = struct{}{}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("starting document summary resolution: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	values := make([]string, len(identities))
	args := make([]any, 0, 5+len(identities)*4)
	// The shared live-document CTE carries its ordinary path-prefix predicate;
	// root binds it as an unrestricted current/live catalog for this exact set.
	args = append(args, s.rootID, "/", "/", "/", "/")
	for index, identity := range identities {
		values[index] = "(?,?,?,?)"
		args = append(args, index, identity.NodeID, identity.ContentVersionID, identity.Path)
	}
	rows, err := tx.QueryContext(ctx, documentCatalogCTE+`, requested(ordinal,node_id,content_version_id,path) AS (
		VALUES `+strings.Join(values, ",")+`
	)
	SELECT r.ordinal,d.node_id,d.content_version_id,d.path,d.name,d.media_type,d.size,d.modified_at,
	       d.processing_state,h.profile_fingerprint,h.attachment_id,a.build_id
	FROM requested r JOIN documents d ON d.node_id=r.node_id
		AND d.content_version_id=r.content_version_id AND d.path=r.path
	LEFT JOIN rendition_heads h ON h.content_version_id=d.content_version_id
	LEFT JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
	ORDER BY r.ordinal,h.profile_fingerprint,h.attachment_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying document summary resolution: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]DocumentSummary, 0, len(identities))
	ordinal := -1
	for rows.Next() {
		var rowOrdinal int
		var item DocumentSummary
		var profile, attachment, build sql.NullString
		if err := rows.Scan(&rowOrdinal, &item.NodeID, &item.ContentVersionID, &item.Path, &item.Name,
			&item.MediaType, &item.Size, &item.ModifiedAt, &item.LatestProcessingState,
			&profile, &attachment, &build); err != nil {
			return nil, fmt.Errorf("scanning document summary resolution: %w", err)
		}
		if rowOrdinal != ordinal {
			if rowOrdinal != len(items) {
				return nil, ErrProcessingSourceFenceStaleVersion
			}
			items = append(items, item)
			ordinal = rowOrdinal
		}
		if profile.Valid || attachment.Valid || build.Valid {
			if !profile.Valid || !attachment.Valid || !build.Valid {
				return nil, ErrProcessingSourceFenceStaleVersion
			}
			items[len(items)-1].ActiveRenditions = append(items[len(items)-1].ActiveRenditions,
				DocumentRenditionIdentity{ProfileFingerprint: profile.String, AttachmentID: attachment.String,
					BuildID: build.String})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading document summary resolution: %w", err)
	}
	if len(items) != len(identities) {
		return nil, ErrProcessingSourceFenceStaleVersion
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("closing document summary resolution: %w", err)
	}
	return items, nil
}

func normalizeDocumentCatalogQuery(query DocumentCatalogQuery) (DocumentCatalogQuery, error) {
	if query.PathPrefix == "" {
		query.PathPrefix = "/"
	}
	if !strings.HasPrefix(query.PathPrefix, "/") {
		return DocumentCatalogQuery{}, fmt.Errorf("%w: path prefix must be absolute", ErrInvalidDocumentQuery)
	}
	if len(query.PathPrefix) > MaxWalkPathBytes {
		return DocumentCatalogQuery{}, fmt.Errorf("%w: path prefix exceeds %d bytes",
			ErrInvalidDocumentQuery, MaxWalkPathBytes)
	}
	segments := splitPath(query.PathPrefix)
	for index, segment := range segments {
		normalized, err := NormalizeName(segment)
		if err != nil {
			return DocumentCatalogQuery{}, fmt.Errorf("%w: invalid path prefix: %w", ErrInvalidDocumentQuery, err)
		}
		segments[index] = normalized
	}
	query.PathPrefix = "/" + strings.Join(segments, "/")

	if query.Sort == "" {
		query.Sort = DocumentCatalogSortPath
	}
	switch query.Sort {
	case DocumentCatalogSortPath, DocumentCatalogSortName, DocumentCatalogSortModifiedAt,
		DocumentCatalogSortSize, DocumentCatalogSortMediaType:
	default:
		return DocumentCatalogQuery{}, fmt.Errorf("%w: invalid sort %q", ErrInvalidDocumentQuery, query.Sort)
	}
	if query.Direction == "" {
		query.Direction = DocumentCatalogDirectionAscending
	}
	if query.Direction != DocumentCatalogDirectionAscending &&
		query.Direction != DocumentCatalogDirectionDescending {
		return DocumentCatalogQuery{}, fmt.Errorf("%w: invalid direction %q",
			ErrInvalidDocumentQuery, query.Direction)
	}
	if query.PageSize == 0 {
		query.PageSize = DefaultDocumentCatalogPageSize
	}
	if query.PageSize < 1 || query.PageSize > MaxDocumentCatalogPageSize {
		return DocumentCatalogQuery{}, fmt.Errorf("%w: page size must be between 1 and %d",
			ErrInvalidDocumentQuery, MaxDocumentCatalogPageSize)
	}
	return query, nil
}

// NormalizeDocumentCatalogQuery applies the store-owned query defaults and
// logical-path rules without reading catalog state.
func NormalizeDocumentCatalogQuery(query DocumentCatalogQuery) (DocumentCatalogQuery, error) {
	return normalizeDocumentCatalogQuery(query)
}

const documentCatalogCTE = `WITH RECURSIVE live_nodes(id,path) AS (
	SELECT id,'/' FROM nodes WHERE id=? AND trashed_at IS NULL
	UNION ALL
	SELECT n.id,CASE WHEN p.path='/' THEN '/'||n.name ELSE p.path||'/'||n.name END
	FROM nodes n JOIN live_nodes p ON n.parent_id=p.id
	WHERE n.trashed_at IS NULL
), documents AS (
	SELECT n.id AS node_id,cv.version_id AS content_version_id,l.path,n.name,
	       COALESCE(cv.mime_type,'') AS media_type,cv.size,n.modified_at,
	       COALESCE((SELECT j.state
	         FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id
	         WHERE w.content_version_id=cv.version_id
	         ORDER BY j.updated_at DESC,j.job_id DESC LIMIT 1),'') AS processing_state
	FROM live_nodes l JOIN nodes n ON n.id=l.id
	JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
	WHERE n.kind='file' AND (?='/' OR l.path=? OR substr(l.path,1,length(?)+1)=?||'/')
)
`

func (s *Store) queryDocumentCatalogPage(
	ctx context.Context, tx *sql.Tx, query DocumentCatalogQuery,
	boundary *DocumentCatalogPosition, traversal DocumentCatalogTraversal,
) ([]DocumentSummary, error) {
	where, boundaryArgs := documentCatalogBoundary(query, boundary, traversal)
	order := documentCatalogOrder(query, traversal)
	args := []any{s.rootID, query.PathPrefix, query.PathPrefix, query.PathPrefix, query.PathPrefix}
	args = append(args, boundaryArgs...)
	args = append(args, query.PageSize+1)
	rows, err := tx.QueryContext(ctx, documentCatalogCTE+`
		SELECT node_id,content_version_id,path,name,media_type,size,modified_at,processing_state
		FROM documents `+where+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying document catalog page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]DocumentSummary, 0, query.PageSize+1)
	for rows.Next() {
		var item DocumentSummary
		if err := rows.Scan(&item.NodeID, &item.ContentVersionID, &item.Path, &item.Name,
			&item.MediaType, &item.Size, &item.ModifiedAt, &item.LatestProcessingState); err != nil {
			return nil, fmt.Errorf("scanning document catalog page: %w", err)
		}
		item.ActiveRenditions, err = activeDocumentRenditions(ctx, tx, item.ContentVersionID)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading document catalog page: %w", err)
	}
	if len(items) > query.PageSize {
		items = items[:query.PageSize]
	}
	if traversal == DocumentCatalogTraversalPrevious {
		slices.Reverse(items)
	}
	return items, nil
}

func activeDocumentRenditions(
	ctx context.Context, tx *sql.Tx, contentVersionID string,
) ([]DocumentRenditionIdentity, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT h.profile_fingerprint,h.attachment_id,a.build_id
		FROM rendition_heads h JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		WHERE h.content_version_id=?
		ORDER BY h.profile_fingerprint,h.attachment_id`, contentVersionID)
	if err != nil {
		return nil, fmt.Errorf("querying active document renditions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	identities := make([]DocumentRenditionIdentity, 0)
	for rows.Next() {
		var identity DocumentRenditionIdentity
		if err := rows.Scan(&identity.ProfileFingerprint, &identity.AttachmentID, &identity.BuildID); err != nil {
			return nil, fmt.Errorf("scanning active document rendition: %w", err)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading active document renditions: %w", err)
	}
	return identities, nil
}

func (s *Store) documentCatalogHasRows(
	ctx context.Context, tx *sql.Tx, query DocumentCatalogQuery,
	position DocumentCatalogPosition, traversal DocumentCatalogTraversal,
) (bool, error) {
	where, boundaryArgs := documentCatalogBoundary(query, &position, traversal)
	args := []any{s.rootID, query.PathPrefix, query.PathPrefix, query.PathPrefix, query.PathPrefix}
	args = append(args, boundaryArgs...)
	var one int
	err := tx.QueryRowContext(ctx, documentCatalogCTE+`
		SELECT 1 FROM documents `+where+` LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking document catalog continuation: %w", err)
	}
	return true, nil
}

func documentCatalogBoundary(
	query DocumentCatalogQuery, boundary *DocumentCatalogPosition,
	traversal DocumentCatalogTraversal,
) (string, []any) {
	if boundary == nil {
		return "", nil
	}
	ascending := query.Direction == DocumentCatalogDirectionAscending
	if traversal == DocumentCatalogTraversalPrevious {
		ascending = !ascending
	}
	op := ">"
	if !ascending {
		op = "<"
	}
	if query.Sort == DocumentCatalogSortPath {
		return fmt.Sprintf(`WHERE (path %s ? OR (path=? AND node_id %s ?))`, op, op),
			[]any{boundary.Path, boundary.Path, boundary.NodeID}
	}
	expression := documentCatalogPrimaryExpression(query.Sort)
	primary := any(boundary.Value)
	if query.Sort == DocumentCatalogSortSize {
		primary = boundary.Size
	}
	return fmt.Sprintf(`WHERE (%s %s ? OR (%s=? AND (path %s ? OR (path=? AND node_id %s ?))))`,
			expression, op, expression, op, op),
		[]any{primary, primary, boundary.Path, boundary.Path, boundary.NodeID}
}

func documentCatalogOrder(
	query DocumentCatalogQuery, traversal DocumentCatalogTraversal,
) string {
	direction := "ASC"
	ascending := query.Direction == DocumentCatalogDirectionAscending
	if traversal == DocumentCatalogTraversalPrevious {
		ascending = !ascending
	}
	if !ascending {
		direction = "DESC"
	}
	if query.Sort == DocumentCatalogSortPath {
		return "path " + direction + ",node_id " + direction
	}
	return documentCatalogPrimaryExpression(query.Sort) + " " + direction +
		",path " + direction + ",node_id " + direction
}

func documentCatalogPrimaryExpression(sortBy DocumentCatalogSort) string {
	switch sortBy {
	case DocumentCatalogSortName:
		return "name"
	case DocumentCatalogSortModifiedAt:
		return "modified_at"
	case DocumentCatalogSortSize:
		return "size"
	case DocumentCatalogSortMediaType:
		return "media_type"
	default:
		return "path"
	}
}

func documentCatalogPosition(
	sortBy DocumentCatalogSort, item DocumentSummary,
) DocumentCatalogPosition {
	position := DocumentCatalogPosition{Path: item.Path, NodeID: item.NodeID}
	switch sortBy {
	case DocumentCatalogSortPath:
		position.Value = item.Path
	case DocumentCatalogSortName:
		position.Value = item.Name
	case DocumentCatalogSortModifiedAt:
		position.Value = item.ModifiedAt
	case DocumentCatalogSortSize:
		position.Size = item.Size
	case DocumentCatalogSortMediaType:
		position.Value = item.MediaType
	}
	return position
}

func validateDocumentCatalogPosition(
	sortBy DocumentCatalogSort, position DocumentCatalogPosition,
) error {
	if position.NodeID <= 0 || !strings.HasPrefix(position.Path, "/") ||
		len(position.Path) > MaxWalkPathBytes {
		return fmt.Errorf("%w: invalid catalog position", ErrInvalidDocumentQuery)
	}
	if sortBy == DocumentCatalogSortSize {
		if position.Size < 0 {
			return fmt.Errorf("%w: invalid size position", ErrInvalidDocumentQuery)
		}
	} else if position.Value == "" && sortBy != DocumentCatalogSortMediaType {
		return fmt.Errorf("%w: missing catalog position", ErrInvalidDocumentQuery)
	}
	return nil
}
