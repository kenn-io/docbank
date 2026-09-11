package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/query"
)

const (
	querySnapshotMaxRows      = int64(250000)
	querySnapshotMaxRowBytes  = int64(64 << 10)
	querySnapshotMaxBytes     = int64(512 << 20)
	querySnapshotBuildTimeout = 30 * time.Second
	querySnapshotFacetTimeout = 5 * time.Second
	querySnapshotFacetMembers = int64(250000)
	querySnapshotDefaultPage  = 100
)

// ErrQuerySnapshotTooLarge reports a row, population, or serialized projection
// that cannot be retained within the snapshot materialization bounds.
var ErrQuerySnapshotTooLarge = errors.New("query snapshot exceeds its materialization limit")

// SnapshotRequest selects one frozen QueryV1 projection and its optional facets.
type SnapshotRequest struct {
	Query    query.Query       `json:"query"`
	Coverage CoverageSelection `json:"coverage"`
	PageSize int               `json:"page_size"`
	Facets   []string          `json:"facets"`
}

// SnapshotMember is the exact node/content authority frozen by a snapshot.
type SnapshotMember struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	BlobHash         string `json:"blob_hash"`
	Size             int64  `json:"size"`
	Revision         int64  `json:"revision"`
}

// SnapshotTag is one assigned tag observation without live aggregate counts.
type SnapshotTag struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

// SnapshotRow freezes display metadata around one exact snapshot member.
type SnapshotRow struct {
	SnapshotMember

	Name                   string        `json:"name"`
	Path                   string        `json:"path"`
	MIMEType               string        `json:"mime_type"`
	MediaFamily            string        `json:"media_family"`
	ModifiedAt             string        `json:"modified_at"`
	SortKey                string        `json:"sort_key"`
	Tags                   []SnapshotTag `json:"tags"`
	CollectionIDs          []string      `json:"collection_ids"`
	DisplayCollectionID    string        `json:"display_collection_id"`
	DisplayCollectionLabel *string       `json:"display_collection_label"`
	CoverageState          string        `json:"coverage_state"`
	CoverageBuildID        string        `json:"coverage_build_id"`
	CoverageAttachmentID   string        `json:"coverage_attachment_id"`
	Excerpt                string        `json:"excerpt"`
}

// SnapshotGeneration identifies the native or rendition lexical source used.
type SnapshotGeneration struct {
	Kind         string `json:"kind"`
	GenerationID string `json:"generation_id"`
}

// SnapshotFacetValue is one counted and optionally selected facet value.
type SnapshotFacetValue struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Count    int64  `json:"count"`
	Selected bool   `json:"selected"`
}

// SnapshotFacet is either a complete count projection or an explicit unavailable result.
type SnapshotFacet struct {
	Dimension string               `json:"dimension"`
	Available bool                 `json:"available"`
	Reason    string               `json:"reason"`
	Total     *int64               `json:"total"`
	Values    []SnapshotFacetValue `json:"values"`
	Missing   *int64               `json:"missing"`
	Other     *int64               `json:"other"`
}

// SnapshotProjection is a complete immutable materialization without cache ownership.
type SnapshotProjection struct {
	Query               query.Query        `json:"query"`
	Dependencies        []query.Dependency `json:"dependencies"`
	QueryFingerprint    string             `json:"query_fingerprint"`
	MemberHash          string             `json:"member_hash"`
	SnapshotFingerprint string             `json:"snapshot_fingerprint"`
	Generation          SnapshotGeneration `json:"generation"`
	Coverage            CoverageSelection  `json:"coverage"`
	ObservedAt          time.Time          `json:"observed_at"`
	PageSize            int                `json:"page_size"`
	Total               int64              `json:"total"`
	TotalBytes          int64              `json:"total_bytes"`
	Rows                []SnapshotRow      `json:"rows"`
	Facets              []SnapshotFacet    `json:"facets"`
	SerializedBytes     int64              `json:"-"`
}

type snapshotMaterializeOptions struct {
	MaxRows            int64
	MaxRowBytes        int64
	MaxSerializedBytes int64
	FacetMemberLimit   int64
	BuildTimeout       time.Duration
	FacetTimeout       time.Duration
	Now                func() time.Time
	Charge             func(rows, bytes int64) error
	SavedQuery         *savedQuerySnapshotInput
}

type savedQuerySnapshotInput struct {
	ID               string
	ExpectedRevision int64
}

func defaultSnapshotMaterializeOptions() snapshotMaterializeOptions {
	return snapshotMaterializeOptions{
		MaxRows: querySnapshotMaxRows, MaxRowBytes: querySnapshotMaxRowBytes,
		MaxSerializedBytes: querySnapshotMaxBytes,
		FacetMemberLimit:   querySnapshotFacetMembers,
		BuildTimeout:       querySnapshotBuildTimeout, FacetTimeout: querySnapshotFacetTimeout,
		Now:    time.Now,
		Charge: func(int64, int64) error { return nil },
	}
}

func (options snapshotMaterializeOptions) withDefaults() snapshotMaterializeOptions {
	defaults := defaultSnapshotMaterializeOptions()
	if options.MaxRows == 0 {
		options.MaxRows = defaults.MaxRows
	}
	if options.MaxRowBytes == 0 {
		options.MaxRowBytes = defaults.MaxRowBytes
	}
	if options.MaxSerializedBytes == 0 {
		options.MaxSerializedBytes = defaults.MaxSerializedBytes
	}
	if options.FacetMemberLimit == 0 {
		options.FacetMemberLimit = defaults.FacetMemberLimit
	}
	if options.BuildTimeout == 0 {
		options.BuildTimeout = defaults.BuildTimeout
	}
	if options.FacetTimeout == 0 {
		options.FacetTimeout = defaults.FacetTimeout
	}
	if options.Now == nil {
		options.Now = defaults.Now
	}
	if options.Charge == nil {
		options.Charge = defaults.Charge
	}
	return options
}

func (s *Store) MaterializeQuerySnapshot(
	ctx context.Context, request SnapshotRequest,
) (SnapshotProjection, error) {
	return s.materializeQuerySnapshot(ctx, request, defaultSnapshotMaterializeOptions())
}

func (s *Store) materializeQuerySnapshot(
	ctx context.Context, request SnapshotRequest, options snapshotMaterializeOptions,
) (SnapshotProjection, error) {
	options = options.withDefaults()
	pageSize, err := normalizeSnapshotPageSize(request.PageSize)
	if err != nil {
		return SnapshotProjection{}, err
	}
	facets, err := normalizeSnapshotFacets(request.Facets)
	if err != nil {
		return SnapshotProjection{}, err
	}
	coverage, err := normalizeCoverageSelection([]CoverageSelection{request.Coverage})
	if err != nil {
		return SnapshotProjection{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, options.BuildTimeout)
	defer cancel()

	var projection SnapshotProjection
	err = s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) error {
		value := request.Query
		if options.SavedQuery != nil {
			resolved, resolveErr := (queryResolver{q: q}).Resolve(
				ctx, query.ReferenceSaved, options.SavedQuery.ID, true,
			)
			if resolveErr != nil {
				if errors.Is(resolveErr, query.ErrUnknownReference) {
					return fmt.Errorf("saved query %q changed before execution: %w", options.SavedQuery.ID, ErrStaleRevision)
				}
				return resolveErr
			}
			if resolved.Dependency.Revision != options.SavedQuery.ExpectedRevision {
				return fmt.Errorf("saved query %s revision is %d, expected %d: %w",
					options.SavedQuery.ID, resolved.Dependency.Revision,
					options.SavedQuery.ExpectedRevision, ErrStaleRevision)
			}
			value = *resolved.Query
		}
		compiled, err := CompileQuery(ctx, value, queryResolver{q: q})
		if err != nil {
			return err
		}
		if coverage.Configuration == "configured" {
			if err := validateSnapshotCoverageProfile(ctx, q, coverage); err != nil {
				return err
			}
		}
		queryFingerprint, err := query.Fingerprint(compiled.Query)
		if err != nil {
			return err
		}
		projection = SnapshotProjection{
			Query: compiled.Query, Dependencies: slices.Clone(compiled.Dependencies),
			QueryFingerprint: queryFingerprint, Coverage: coverage, PageSize: pageSize,
			ObservedAt: options.Now().UTC(), Rows: make([]SnapshotRow, 0), Facets: make([]SnapshotFacet, 0, len(facets)),
			Generation: SnapshotGeneration{Kind: "native"},
		}
		if generation.ID != "" {
			projection.Generation = SnapshotGeneration{Kind: "rendition", GenerationID: generation.ID}
		}
		if err := materializeSnapshotRows(ctx, q, compiled, generation.ID, coverage, options, &projection); err != nil {
			return err
		}
		projection.MemberHash = snapshotMemberHash(snapshotMembers(projection.Rows))
		projection.Facets, err = materializeSnapshotFacets(ctx, q, compiled, generation.ID, coverage, facets, options, &projection.SerializedBytes)
		if err != nil {
			return err
		}
		projection.SnapshotFingerprint, err = snapshotProjectionFingerprint(projection)
		if err != nil {
			return err
		}
		metadataBytes, err := snapshotProjectionMetadataBytes(projection)
		if err != nil {
			return err
		}
		if err := chargeSnapshotMaterialization(options, &projection.SerializedBytes, 0, metadataBytes); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return SnapshotProjection{}, err
	}
	return projection, nil
}

func normalizeSnapshotPageSize(value int) (int, error) {
	if value == 0 {
		return querySnapshotDefaultPage, nil
	}
	if value != 50 && value != 100 && value != 250 {
		return 0, errors.New("snapshot page size must be 50, 100, or 250")
	}
	return value, nil
}

func validateSnapshotCoverageProfile(ctx context.Context, q metadataQuerier, selection CoverageSelection) error {
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM processing_profiles WHERE profile_fingerprint=?)`, selection.ProfileFingerprint).Scan(&exists); err != nil {
		return err
	}
	if exists {
		_, err := loadProcessingProfile(ctx, q, selection.ProfileFingerprint)
		return err
	}
	return nil
}

func materializeSnapshotRows(
	ctx context.Context, q metadataQuerier, compiled CompiledQuery, generationID string,
	coverage CoverageSelection, options snapshotMaterializeOptions, projection *SnapshotProjection,
) error {
	if err := chargeSnapshotMaterialization(options, &projection.SerializedBytes, 0, 2); err != nil {
		return err
	}
	population, err := matchedPopulation(compiled, generationID, "")
	if err != nil {
		return err
	}
	statement, args, err := bindQueryPopulation(population, coverage, generationID)
	if err != nil {
		return err
	}
	coverageCTE, coverageJoin, coverageColumns := "", "", `'', '', ''`
	if coverage.Configuration == "configured" {
		coverageCTE = `, coverage_members AS (SELECT node_id FROM matched_members), ` + processingCoverageCTE()
		coverageJoin = `LEFT JOIN processing_coverage pc ON pc.node_id=n.id AND pc.version_id=cv.version_id`
		coverageColumns = `COALESCE(pc.state,''), COALESCE(pc.build_id,''), COALESCE(pc.attachment_id,'')`
		args = append(args, coverage.ProfileFingerprint, generationID)
	}
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE
		matched_members AS (`+statement+`),
		tree_paths(id,path) AS (
			SELECT id,'/' FROM nodes WHERE parent_id IS NULL
			UNION ALL
			SELECT n.id, CASE WHEN p.path='/' THEN '/'||n.name ELSE p.path||'/'||n.name END
			FROM nodes n JOIN tree_paths p ON p.id=n.parent_id
		), `+CollectionMembershipCTE+coverageCTE+`
		SELECT n.id,cv.version_id,cv.blob_hash,cv.size,n.revision,n.name,tp.path,
			COALESCE(cv.mime_type,''),n.modified_at,
			COALESCE((SELECT json_group_array(json_object('id',id,'name',name,'revision',revision))
				FROM (SELECT t.id,t.name,t.revision FROM node_tags nt JOIN tags t ON t.id=nt.tag_id
					WHERE nt.node_id=n.id ORDER BY t.id)),'[]'),
			COALESCE((SELECT json_group_array(ingest_id) FROM (
				SELECT ingest_id FROM collection_members WHERE node_id=n.id ORDER BY ingest_id)),'[]'),
			COALESCE((SELECT ingest_id FROM collection_members WHERE node_id=n.id ORDER BY ingest_id LIMIT 1),''),
			(SELECT l.label FROM collection_members cm LEFT JOIN collection_labels l ON l.ingest_id=cm.ingest_id
				WHERE cm.node_id=n.id ORDER BY cm.ingest_id LIMIT 1),
			`+coverageColumns+`
		FROM matched_members m JOIN nodes n ON n.id=m.node_id
		JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=m.content_version_id
		JOIN tree_paths tp ON tp.id=n.id `+coverageJoin, args...)
	if err != nil {
		return fmt.Errorf("materializing query snapshot rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if int64(len(projection.Rows)) >= options.MaxRows {
			return fmt.Errorf("%w: more than %d rows", ErrQuerySnapshotTooLarge, options.MaxRows)
		}
		var row SnapshotRow
		var tagsJSON, collectionsJSON []byte
		var displayLabel sql.NullString
		if err := rows.Scan(&row.NodeID, &row.ContentVersionID, &row.BlobHash, &row.Size, &row.Revision,
			&row.Name, &row.Path, &row.MIMEType, &row.ModifiedAt, &tagsJSON, &collectionsJSON,
			&row.DisplayCollectionID, &displayLabel, &row.CoverageState, &row.CoverageBuildID,
			&row.CoverageAttachmentID); err != nil {
			return fmt.Errorf("scanning query snapshot row: %w", err)
		}
		if int64(len(tagsJSON)) > options.MaxRowBytes || int64(len(collectionsJSON)) > options.MaxRowBytes {
			return fmt.Errorf("%w: row %d membership projection exceeds %d bytes", ErrQuerySnapshotTooLarge, row.NodeID, options.MaxRowBytes)
		}
		if err := json.Unmarshal(tagsJSON, &row.Tags); err != nil {
			return fmt.Errorf("decoding snapshot tags: %w", err)
		}
		if err := json.Unmarshal(collectionsJSON, &row.CollectionIDs); err != nil {
			return fmt.Errorf("decoding snapshot collections: %w", err)
		}
		if row.Tags == nil {
			row.Tags = make([]SnapshotTag, 0)
		}
		if row.CollectionIDs == nil {
			row.CollectionIDs = make([]string, 0)
		}
		if displayLabel.Valid {
			row.DisplayCollectionLabel = new(displayLabel.String)
		}
		row.MediaFamily = query.ClassifyMedia(row.MIMEType, row.Name)
		row.SortKey, err = snapshotSortKey(row, compiled.Query.Sort.Field)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("encoding query snapshot row: %w", err)
		}
		rowBytes := int64(len(encoded))
		if rowBytes > options.MaxRowBytes {
			return fmt.Errorf("%w: row %d encodes to %d bytes", ErrQuerySnapshotTooLarge, row.NodeID, rowBytes)
		}
		if len(projection.Rows) != 0 {
			rowBytes++ // JSON array separator; container brackets were charged before the query.
		}
		if err := chargeSnapshotMaterialization(options, &projection.SerializedBytes, 1, rowBytes); err != nil {
			return err
		}
		projection.TotalBytes += row.Size
		projection.Rows = append(projection.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("materializing query snapshot rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing query snapshot rows: %w", err)
	}
	projection.Total = int64(len(projection.Rows))
	sortSnapshotRows(projection.Rows, compiled.Query.Sort)
	return nil
}

func snapshotSortKey(row SnapshotRow, field string) (string, error) {
	switch field {
	case "name":
		return row.Name, nil
	case "path":
		return row.Path, nil
	case "modified_at":
		key, err := query.TimestampKey(row.ModifiedAt)
		if err != nil {
			return "", fmt.Errorf("node %d has invalid modified timestamp: %w", row.NodeID, err)
		}
		return key, nil
	case "size":
		return strconv.FormatInt(row.Size, 10), nil
	case "media_type":
		return query.NormalizeMediaType(row.MIMEType), nil
	default:
		return "", errors.New("unsupported snapshot sort")
	}
}

func sortSnapshotRows(rows []SnapshotRow, order query.Sort) {
	slices.SortStableFunc(rows, func(left, right SnapshotRow) int {
		var result int
		if order.Field == "size" {
			result = cmp.Compare(left.Size, right.Size)
		} else {
			result = strings.Compare(left.SortKey, right.SortKey)
		}
		if order.Direction == "desc" {
			result = -result
		}
		if result != 0 {
			return result
		}
		if result = cmp.Compare(left.NodeID, right.NodeID); result != 0 {
			return result
		}
		return strings.Compare(left.ContentVersionID, right.ContentVersionID)
	})
}

func snapshotMembers(rows []SnapshotRow) []SnapshotMember {
	members := make([]SnapshotMember, len(rows))
	for i := range rows {
		members[i] = rows[i].SnapshotMember
	}
	return members
}

func snapshotMemberHash(members []SnapshotMember) string {
	members = slices.Clone(members)
	slices.SortFunc(members, func(left, right SnapshotMember) int {
		if result := cmp.Compare(left.NodeID, right.NodeID); result != 0 {
			return result
		}
		return strings.Compare(left.ContentVersionID, right.ContentVersionID)
	})
	digest := sha256.New()
	for _, member := range members {
		_, _ = fmt.Fprintf(digest, "%d:%s\n", member.NodeID, member.ContentVersionID)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

type snapshotFingerprintPayload struct {
	Query        query.Query        `json:"query"`
	Dependencies []query.Dependency `json:"dependencies"`
	Generation   SnapshotGeneration `json:"generation"`
	Coverage     CoverageSelection  `json:"coverage"`
	Rows         []SnapshotRow      `json:"rows"`
	Facets       []SnapshotFacet    `json:"facets"`
}

func snapshotProjectionFingerprint(projection SnapshotProjection) (string, error) {
	digest := sha256.New()
	err := json.MarshalWrite(digest, snapshotFingerprintPayload{
		Query: projection.Query, Dependencies: projection.Dependencies, Generation: projection.Generation,
		Coverage: projection.Coverage, Rows: projection.Rows, Facets: projection.Facets,
	}, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encoding snapshot fingerprint: %w", err)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func snapshotProjectionMetadataBytes(projection SnapshotProjection) (int64, error) {
	metadata := projection
	metadata.Rows = nil
	metadata.Facets = nil
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return 0, fmt.Errorf("encoding snapshot metadata: %w", err)
	}
	return int64(len(encoded)), nil
}

func chargeSnapshotMaterialization(
	options snapshotMaterializeOptions, used *int64, rows, bytes int64,
) error {
	if bytes < 0 || *used > options.MaxSerializedBytes-bytes {
		return fmt.Errorf("%w: serialized projection exceeds %d bytes", ErrQuerySnapshotTooLarge, options.MaxSerializedBytes)
	}
	if err := options.Charge(rows, bytes); err != nil {
		return err
	}
	*used += bytes
	return nil
}
