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
	// MaxDocumentCatalogNameCharacters matches the bounded document summary contract.
	MaxDocumentCatalogNameCharacters = 255
	DefaultDocumentCatalogPageSize   = 50
	MaxDocumentCatalogPageSize       = 250
	MaxPhotoMapPageSize              = 1000
	MaxDocumentSummaryResolutions    = 100
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
	SourcePresent bool
	PathPrefix    string
	Sort          DocumentCatalogSort
	Direction     DocumentCatalogDirection
	PageSize      int
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

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return DocumentCatalogPage{}, fmt.Errorf("starting document catalog page: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	root, err := s.nodeByPath(ctx, tx, normalized.PathPrefix)
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNotDir) {
		return DocumentCatalogPage{}, fmt.Errorf("resolving document catalog prefix: %w", err)
	}
	args := append(documentCatalogArgs(root.ID, normalized.PathPrefix), normalized.SourcePresent)
	page, err := s.queryDocumentCatalogPage(ctx, tx, normalized, boundary, traversal, args)
	if err != nil {
		return DocumentCatalogPage{}, err
	}
	page.Query = normalized
	if len(page.Items) > 0 {
		page.FirstPosition = documentCatalogPosition(normalized.Sort, page.Items[0])
		page.LastPosition = documentCatalogPosition(normalized.Sort, page.Items[len(page.Items)-1])
	}
	if err := tx.Commit(); err != nil {
		return DocumentCatalogPage{}, fmt.Errorf("closing document catalog page: %w", err)
	}
	return page, nil
}

// ResolveDocumentSummaries resolves an ordered bounded set of exact
// current/live identities in one query. Missing, moved, superseded,
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
			return nil, ErrInvalidDocumentQuery
		}
		if _, err := NormalizeDocumentCatalogQuery(DocumentCatalogQuery{PathPrefix: identity.Path}); err != nil {
			return nil, err
		}
		seen[identity] = struct{}{}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("starting document summary resolution: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// PostgreSQL infers the remaining VALUES rows from the first typed row.
	var values strings.Builder
	values.WriteString("(CAST($5 AS INTEGER),CAST($6 AS BIGINT),CAST($7 AS TEXT),CAST($8 AS TEXT))")
	args := []any{s.rootID, MaxWalkDepth, MaxWalkPathBytes, MaxDocumentCatalogNameCharacters}
	for index, identity := range identities {
		if index > 0 {
			offset := len(args)
			fmt.Fprintf(&values, ",($%d,$%d,$%d,$%d)", offset+1, offset+2, offset+3, offset+4)
		}
		args = append(args, index, identity.NodeID, identity.ContentVersionID, identity.Path)
	}
	postgres := s.postgresSchema != ""
	nameBytes := documentCatalogByteLength(postgres, "n.name")
	ancestorBytes := documentCatalogByteLength(postgres, "a.path")
	// Resolve each selected parent once, then check exact filenames. Unrelated
	// subtrees must not make a bounded identity lookup grow with the whole vault.
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE
	limits(root_id,max_depth,max_path_bytes,max_name_chars) AS (
	 VALUES (CAST($1 AS BIGINT),CAST($2 AS INTEGER),CAST($3 AS INTEGER),CAST($4 AS INTEGER))
	),
	requested(ordinal,node_id,content_version_id,path) AS (
		VALUES `+values.String()+`
	), selected_documents AS (
		SELECT r.ordinal,n.id AS node_id,n.parent_id,cv.version_id AS content_version_id,
			r.path,n.name,COALESCE(cv.mime_type,'') AS media_type,cv.size,n.modified_at
		FROM requested r JOIN nodes n ON n.id=r.node_id
		JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
		CROSS JOIN limits
		WHERE n.kind='file' AND n.trashed_at IS NULL
			AND cv.version_id=r.content_version_id AND length(n.name)<=limits.max_name_chars
	), ancestry(origin_id,id,parent_id,path,depth) AS (
		SELECT DISTINCT n.id,n.id,n.parent_id,
			CASE WHEN n.id=limits.root_id THEN '' ELSE '/'||n.name END,1
		FROM selected_documents d JOIN nodes n ON n.id=d.parent_id CROSS JOIN limits
		WHERE n.trashed_at IS NULL AND (n.id=limits.root_id
			OR `+nameBytes+`+1<=limits.max_path_bytes)
		UNION ALL
		SELECT a.origin_id,n.id,n.parent_id,
			CASE WHEN n.id=limits.root_id THEN a.path ELSE '/'||n.name||a.path END,a.depth+1
		FROM nodes n JOIN ancestry a ON n.id=a.parent_id CROSS JOIN limits
		WHERE n.trashed_at IS NULL AND a.depth<limits.max_depth
			AND `+ancestorBytes+` + CASE WHEN n.id=limits.root_id THEN 0
				ELSE `+nameBytes+`+1 END <= limits.max_path_bytes
	), documents AS (
		SELECT d.* FROM selected_documents d CROSS JOIN limits
		JOIN ancestry a ON a.origin_id=d.parent_id AND a.id=limits.root_id
		WHERE CASE WHEN `+ancestorBytes+`+`+documentCatalogByteLength(postgres, "d.name")+`+1
			<=limits.max_path_bytes THEN a.path||'/'||d.name END=d.path
	)
	SELECT d.ordinal,d.node_id,d.content_version_id,d.path,d.name,d.media_type,d.size,d.modified_at,
	       `+documentCatalogProcessingState(postgres)+`,h.profile_fingerprint,h.attachment_id,a.build_id
	FROM documents d
	LEFT JOIN rendition_heads h ON h.content_version_id=d.content_version_id
	LEFT JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
	ORDER BY d.ordinal,h.profile_fingerprint,h.attachment_id`, args...)
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
	if len(segments) > MaxWalkDepth {
		return DocumentCatalogQuery{}, fmt.Errorf("%w: path prefix exceeds depth bound", ErrInvalidDocumentQuery)
	}
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

// NULL paths mark the first out-of-bounds node and stop expansion there. They
// remain visible to the page query so exceeding a bound cannot silently omit files.
func documentCatalogCTE(postgres bool) string {
	prefix := "CAST($4 AS TEXT)"
	if postgres {
		// The recursive path starts with a parameter, not a C-collated column.
		prefix += ` COLLATE "C"`
	}
	return `WITH RECURSIVE
limits(max_depth,max_path_bytes,max_name_chars) AS (
 VALUES (CAST($1 AS INTEGER),CAST($2 AS INTEGER),CAST($3 AS INTEGER))
),
live_nodes(id,path,depth) AS (
 SELECT id,CASE WHEN kind='file' AND length(name)>limits.max_name_chars THEN NULL ELSE ` + prefix + ` END,
        CAST($5 AS INTEGER)
 FROM nodes CROSS JOIN limits WHERE id=$6 AND trashed_at IS NULL
 UNION ALL
 SELECT n.id,CASE WHEN p.depth>=limits.max_depth
   OR ` + documentCatalogByteLength(postgres, "p.path") + `+` +
		documentCatalogByteLength(postgres, "n.name") + `+CASE WHEN p.path='/' THEN 0 ELSE 1 END>limits.max_path_bytes
   OR (n.kind='file' AND length(n.name)>limits.max_name_chars) THEN NULL
   WHEN p.path='/' THEN '/'||n.name ELSE p.path||'/'||n.name END,p.depth+1
 FROM nodes n JOIN live_nodes p ON n.parent_id=p.id CROSS JOIN limits
 WHERE n.trashed_at IS NULL AND p.path IS NOT NULL
), documents AS (
 SELECT n.id AS node_id,cv.version_id AS content_version_id,l.path,n.name,
        COALESCE(cv.mime_type,'') AS media_type,cv.size,n.modified_at
 FROM live_nodes l JOIN nodes n ON n.id=l.id
 JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
 WHERE n.kind='file' AND l.path IS NOT NULL AND (NOT $7 OR ` + sourcePresentSQL + `)
)
`
}

// Catalog bounds are measured in bytes even when filenames contain Unicode.
func documentCatalogByteLength(postgres bool, value string) string {
	if postgres {
		return "octet_length(" + value + ")"
	}
	return "length(CAST(" + value + " AS BLOB))"
}

// Enrich only selected documents, not every row in the shared catalog.
func documentCatalogProcessingState(postgres bool) string {
	return `COALESCE((SELECT j.state
 FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id
 WHERE w.content_version_id=d.content_version_id AND ` + (metadataJSONDialect{postgres}).originalFileWaiter() + `
 ORDER BY j.updated_at DESC,j.job_id DESC LIMIT 1),'')`
}

func documentCatalogArgs(rootID int64, prefix string) []any {
	return []any{MaxWalkDepth, MaxWalkPathBytes, MaxDocumentCatalogNameCharacters,
		prefix, len(splitPath(prefix)), rootID}
}

func (s *Store) queryDocumentCatalogPage(
	ctx context.Context, tx *sql.Tx, query DocumentCatalogQuery,
	boundary *DocumentCatalogPosition, traversal DocumentCatalogTraversal, args []any,
) (DocumentCatalogPage, error) {
	postgres := s.postgresSchema != ""
	where, boundaryArgs := documentCatalogBoundary(query, boundary, traversal, len(args), postgres)
	order := documentCatalogOrder(query, traversal)
	keys := "path,node_id"
	if query.Sort != DocumentCatalogSortPath {
		keys = documentCatalogPrimaryExpression(query.Sort) + "," + keys
	}
	previousOp, nextOp := "<", ">"
	if query.Direction == DocumentCatalogDirectionDescending {
		previousOp, nextOp = ">", "<"
	}
	firstOrder := documentCatalogOrder(query, DocumentCatalogTraversalNext)
	lastOrder := documentCatalogOrder(query, DocumentCatalogTraversalPrevious)
	args = append(slices.Clone(args), boundaryArgs...)
	args = append(args, query.PageSize)
	// Page and continuation checks share the same recursive traversal. Compare
	// against the returned edges so deleted or moved cursor rows remain valid.
	limit := fmt.Sprintf("$%d", len(args))
	rows, err := tx.QueryContext(ctx, documentCatalogCTE(postgres)+`, page AS (
  SELECT * FROM documents `+where+` ORDER BY `+order+` LIMIT `+limit+`
 )
 SELECT COALESCE(d.node_id,0),COALESCE(d.content_version_id,''),COALESCE(d.path,''),
  COALESCE(d.name,''),COALESCE(d.media_type,''),COALESCE(d.size,0),
  COALESCE(d.modified_at,''),`+documentCatalogProcessingState(postgres)+`,
  h.profile_fingerprint,h.attachment_id,a.build_id,
  EXISTS(SELECT 1 FROM live_nodes WHERE path IS NULL),
  EXISTS(SELECT 1 FROM documents WHERE (`+keys+`) `+previousOp+`
    (SELECT `+keys+` FROM page ORDER BY `+firstOrder+` LIMIT 1)),
  EXISTS(SELECT 1 FROM documents WHERE (`+keys+`) `+nextOp+`
    (SELECT `+keys+` FROM page ORDER BY `+lastOrder+` LIMIT 1))
 FROM (SELECT 1) LEFT JOIN page d ON TRUE
 LEFT JOIN rendition_heads h ON h.content_version_id=d.content_version_id
 LEFT JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
 ORDER BY d.`+strings.ReplaceAll(order, ",", ",d.")+`,h.profile_fingerprint,h.attachment_id`, args...)
	if err != nil {
		return DocumentCatalogPage{}, fmt.Errorf("querying document catalog page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]DocumentSummary, 0, query.PageSize)
	var page DocumentCatalogPage
	for rows.Next() {
		var item DocumentSummary
		var profile, attachment, build sql.NullString
		var exceedsBounds bool
		if err := rows.Scan(&item.NodeID, &item.ContentVersionID, &item.Path, &item.Name,
			&item.MediaType, &item.Size, &item.ModifiedAt, &item.LatestProcessingState,
			&profile, &attachment, &build, &exceedsBounds, &page.HasPrevious, &page.HasNext); err != nil {
			return DocumentCatalogPage{}, fmt.Errorf("scanning document catalog page: %w", err)
		}
		if exceedsBounds {
			return DocumentCatalogPage{}, fmt.Errorf("%w: catalog traversal exceeds path, name, or depth bound", ErrInvalidDocumentQuery)
		}
		if item.NodeID == 0 {
			continue
		}
		if len(items) == 0 || items[len(items)-1].NodeID != item.NodeID {
			item.ActiveRenditions = make([]DocumentRenditionIdentity, 0)
			items = append(items, item)
		}
		if profile.Valid && attachment.Valid && build.Valid {
			current := &items[len(items)-1]
			current.ActiveRenditions = append(current.ActiveRenditions, DocumentRenditionIdentity{
				ProfileFingerprint: profile.String, AttachmentID: attachment.String, BuildID: build.String})
		}
	}
	if err := rows.Err(); err != nil {
		return DocumentCatalogPage{}, fmt.Errorf("reading document catalog page: %w", err)
	}
	if traversal == DocumentCatalogTraversalPrevious {
		slices.Reverse(items)
	}
	page.Items = items
	return page, nil
}

func documentCatalogBoundary(
	query DocumentCatalogQuery, boundary *DocumentCatalogPosition,
	traversal DocumentCatalogTraversal, offset int, postgres bool,
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
		return fmt.Sprintf(`WHERE (path %s $%d OR (path=$%d AND node_id %s $%d))`, op, offset+1, offset+1, op, offset+2),
			[]any{boundary.Path, boundary.NodeID}
	}
	expression := documentCatalogPrimaryExpression(query.Sort)
	primary := any(boundary.Value)
	switch query.Sort {
	case DocumentCatalogSortSize:
		primary = boundary.Size
	case DocumentCatalogSortMediaType:
		primary = storedString(postgres, boundary.Value)
	}
	return fmt.Sprintf(`WHERE (%s %s $%d OR (%s=$%d AND (path %s $%d OR (path=$%d AND node_id %s $%d))))`,
			expression, op, offset+1, expression, offset+1, op, offset+2, offset+2, op, offset+3),
		[]any{primary, boundary.Path, boundary.NodeID}
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
		return string(DocumentCatalogSortName)
	case DocumentCatalogSortModifiedAt:
		return "modified_at"
	case DocumentCatalogSortSize:
		return string(DocumentCatalogSortSize)
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
