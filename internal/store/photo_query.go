package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/kit/search/sqlquery"
)

const photoBrowseConfiguredCoverage = "configured"

type PhotoFlagRow struct {
	AssetID string `json:"asset_id" format:"uuid"`
	Flag    string `json:"flag" enum:",pick,reject"`
}

// LookupPhotoFlags returns current display flags for live, visible assets in ID order.
func (s *Store) LookupPhotoFlags(ctx context.Context, assetIDs []string) ([]PhotoFlagRow, error) {
	for _, id := range assetIDs {
		if err := validateUUIDv4(id); err != nil {
			return nil, fmt.Errorf("%w: asset ID %q: %w", ErrInvalidPhotoAsset, id, err)
		}
	}
	encoded, err := json.Marshal(assetIDs)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.asset_id,f.flag FROM `+photoBrowseDisplayFrom+
		` WHERE a.asset_id IN (SELECT value FROM json_each(?)) AND `+photoBrowseLiveDisplay+
		` AND `+photoVisibilityPredicate(false)+` ORDER BY a.asset_id`, string(encoded))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []PhotoFlagRow{}
	for rows.Next() {
		var row PhotoFlagRow
		if err := rows.Scan(&row.AssetID, &row.Flag); err != nil {
			return nil, err
		}
		items = append(items, row)
	}
	return items, rows.Err()
}

// PhotoBrowseRequest supplies query meaning and exact preview recipe identities.
type PhotoBrowseRequest struct {
	SourceMetadataFingerprint string
	Stacks                    bool
	Hidden                    bool
	Query                     query.Query
	Coverage                  CoverageSelection
	PageSize                  int
	Recipes                   map[string]string
	Facets                    []string
	assetIDs                  []string
}

// PhotoBrowsePosition is the selected SQL key bound to the resolved query.
type PhotoBrowsePosition struct {
	Key           string `json:"key"`
	Missing       bool   `json:"missing"`
	AssetID       string `json:"asset_id"`
	QueryIdentity string `json:"query_identity"`
	Total         int64  `json:"total"`
	StackFrontier string `json:"stack_frontier,omitempty"`
}

type PhotoPreviewSlot struct {
	State      string
	Generation *VisualPreviewGeneration
}

type PhotoBrowseRow struct {
	// MatchedAISuggestion marks a row that matches the whole query only with
	// pending suggestion text.
	MatchedAISuggestion bool
	Quality             *document.PhotoQualitySignals
	// QualityUnavailable marks a terminal evaluator result with no signals.
	QualityUnavailable bool
	AssetID            string
	Kind               string
	Revision           int64
	DisplayFileID      string
	Rotation           int
	Rating             int
	Flag               string
	NodeID             int64
	ContentVersionID   string
	Name               string
	MediaType          string
	ImportTime         string
	// FileModifiedTime is set when the file's mtime, not a capture date, placed the photo.
	FileModifiedTime    *string
	Fields              PhotoTechnicalFields
	Previews            map[string]PhotoPreviewSlot
	position            PhotoBrowsePosition
	sourceHash          string
	folderKey           string
	cameraMake          string
	cameraModel         string
	bodySerialNumber    string
	metadataFingerprint string
}

type PhotoBrowsePage struct {
	GroupingIncomplete bool
	Items              []PhotoBrowseRow
	Total              int64
	Next               *PhotoBrowsePosition
	Facets             []SnapshotFacet
	RelevanceAvailable bool
	BurstGapSeconds    int
	// Stacks describes the bursts among Items when the request asked for them.
	Stacks []PhotoStack
}

const (
	// MaxPhotoSortKeyCharacters bounds the sort key compared across pages so
	// every emitted cursor fits the cursor envelope; longer keys tie on this
	// prefix and fall back to asset order.
	MaxPhotoSortKeyCharacters = 1024
	// MaxPhotoSortKeyBytes is the UTF-8 size of the longest bounded sort key.
	MaxPhotoSortKeyBytes = 4 * MaxPhotoSortKeyCharacters
)

// ListPhotoAssets projects complete matching members into one row per included asset.
// The first page counts matching assets; continuations retain that total while
// reading current rows in live order.
func (s *Store) ListPhotoAssets(
	ctx context.Context, request PhotoBrowseRequest, boundary *PhotoBrowsePosition,
) (PhotoBrowsePage, error) {
	request, err := normalizePhotoBrowseRequest(request)
	if err != nil {
		return PhotoBrowsePage{}, err
	}
	dimensions, coverage := request.Facets, request.Coverage
	var page PhotoBrowsePage
	err = s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) error {
		if request.Hidden {
			if _, err := s.hiddenSession(ctx, q); err != nil {
				return err
			}
		}
		compiled, err := (queryCompiler{photoDisplayMetadata: true, photoHidden: request.Hidden}).compile(
			ctx, request.Query, queryResolver{q: q})
		if err != nil {
			return err
		}
		for _, operand := range compiled.predicate.lexical {
			if !operand.negated {
				page.RelevanceAvailable = true
				break
			}
		}
		settings, err := photoSettingsTx(ctx, q)
		if err != nil {
			return err
		}
		page.BurstGapSeconds = settings.BurstGapSeconds
		sortField := compiled.Query.Sort.Field
		stackSort := sortField == "capture_time" && request.Stacks
		cursorGap := 0
		if stackSort {
			cursorGap = settings.BurstGapSeconds
		}
		if err := validatePhotoQuerySort(compiled.Query); err != nil {
			return err
		}
		if sortField == "relevance" && boundary != nil {
			return ErrInvalidPhotoCursor
		}
		from, sortKey, ok := photoBrowseOrder(sortField)
		if !ok && sortField != "relevance" {
			return fmt.Errorf("%w: unsupported sort", ErrInvalidPhotoQuery)
		}
		if len(request.assetIDs) > 0 {
			// Selected exports seek asset IDs before sorting their small result.
			from = photoBrowseDisplayFrom
			if sortField == "added_time" {
				from += ` CROSS JOIN photo_set_members sm ON sm.asset_id=a.asset_id`
			}
		}
		scope, err := newPhotoBrowseScope(ctx, q, compiled, generation.ID, coverage, request.Hidden)
		if err != nil {
			return err
		}
		if len(request.assetIDs) > 0 {
			scope.match.sql = `a.asset_id IN (` + placeholders(len(request.assetIDs)) + `) AND (` + scope.match.sql + `)`
			args := make([]any, 0, len(request.assetIDs)+len(scope.match.args))
			for _, id := range request.assetIDs {
				args = append(args, id)
			}
			scope.match.args = append(args, scope.match.args...)
		}
		canonical, err := query.Canonical(compiled.Query)
		if err != nil {
			return err
		}
		binding, err := json.Marshal(struct {
			Query               []byte
			Dependencies        []query.Dependency
			Coverage            CoverageSelection
			Hidden              bool
			PageSize            int
			Facets              []string
			AssetIDs            []string `json:",omitzero"`
			BurstGap            int
			Stacks              bool
			MetadataFingerprint string
		}{canonical, compiled.Dependencies, coverage, request.Hidden, request.PageSize, dimensions, request.assetIDs, cursorGap, request.Stacks, request.SourceMetadataFingerprint})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(binding)
		identity := hex.EncodeToString(digest[:])
		if boundary != nil {
			if boundary.QueryIdentity != identity || validateUUIDv4(boundary.AssetID) != nil ||
				len(boundary.Key) > MaxPhotoSortKeyBytes || boundary.Total < 0 ||
				boundary.Missing && boundary.Key != "" {
				return ErrInvalidPhotoCursor
			}
		}
		if boundary == nil {
			countSQL, countArgs, err := scope.bind(`SELECT COUNT(*) FROM ` + photoBrowseDisplayFrom +
				` WHERE ` + scope.where())
			if err != nil {
				return err
			}
			if err := q.QueryRowContext(ctx, countSQL, countArgs...).Scan(&page.Total); err != nil {
				return err
			}
		} else {
			page.Total = boundary.Total
		}
		if boundary == nil && len(dimensions) > 0 {
			page.Facets, err = materializePhotoFacets(ctx, q, compiled, generation.ID, coverage, dimensions, defaultSnapshotMaterializeOptions(), new(int64))
			if err != nil {
				return err
			}
		}
		if sortField == "relevance" {
			page.Items, err = rankedPhotoRows(ctx, q, compiled, generation.ID, coverage, request.PageSize)
			if err != nil {
				return err
			}
			return s.hydratePhotoPreviews(ctx, q, request, page.Items)
		}
		primary, comparison := "ASC", ">"
		if compiled.Query.Sort.Direction == "desc" {
			primary, comparison = "DESC", "<"
		}
		// readRaw returns up to limit rows after boundary. It reads known keys
		// from their index, then missing keys in asset order; keeping these
		// separate avoids sorting the entire library to put NULLs last.
		readRaw := func(boundary *PhotoBrowsePosition, limit int) ([]PhotoBrowseRow, error) {
			result := make([]PhotoBrowseRow, 0, limit)
			for _, missing := range []bool{false, true} {
				if !missing && boundary != nil && boundary.Missing {
					continue
				}
				pageFrom, key := from, sortKey
				order := key + ` ` + primary + `,a.asset_id ASC`
				where := key + `>''`
				var args []any
				if sortField == "added_time" {
					where = `sm.set_id=? AND ` + where
					args = append(args, compiled.Query.Filters.SetIDs[0])
				}
				if missing {
					pageFrom = photoBrowseDisplayFrom
					if sortField == "added_time" {
						continue
					}
					key = photoBrowseMissingKey(sortField)
					where = key + `=''`
					order = `a.asset_id ASC`
					if boundary != nil && boundary.Missing {
						where += ` AND a.asset_id>?`
						args = append(args, boundary.AssetID)
					}
				} else if boundary != nil {
					// The inclusive range gives SQLite a seek even when the tie-break
					// below is an OR involving a joined asset.
					where += ` AND ` + key + comparison + `=? AND (` + key + comparison +
						`? OR (` + key + `=? AND a.asset_id>?))`
					args = append(args, boundary.Key, boundary.Key, boundary.Key, boundary.AssetID)
				}
				args = append(args, limit-len(result))
				pageSQL, pageArgs, err := scope.bind(`SELECT `+photoBrowseSelect+`,`+key+` FROM `+pageFrom+` WHERE `+scope.where()+` AND `+where+
					` ORDER BY `+order+` LIMIT ?`, args...)
				if err != nil {
					return nil, err
				}
				rows, err := q.QueryContext(ctx, pageSQL, pageArgs...)
				if err != nil {
					return nil, err
				}
				items, err := scanPhotoBrowseRows(rows, PhotoBrowsePosition{QueryIdentity: identity, Total: page.Total, Missing: missing})
				if err != nil {
					return nil, err
				}
				result = append(result, items...)
				if len(result) >= limit {
					break
				}
			}
			args = append(args, request.PageSize+1-len(page.Items))
			pageSQL, pageArgs, err := scope.bind(`SELECT `+photoBrowseSelect+`,`+key+`,0 FROM `+pageFrom+` WHERE `+scope.where()+` AND `+where+
				` ORDER BY `+order+` LIMIT ?`, args...)
			if err != nil {
				return err
			}
			rows, err := q.QueryContext(ctx, pageSQL, pageArgs...)
			if err != nil {
				return err
			}
			items, err := scanPhotoBrowseRows(rows, PhotoBrowsePosition{QueryIdentity: identity, Total: page.Total, Missing: missing})
			if err != nil {
				return err
			}
			page.Items = append(page.Items, items...)
			if len(page.Items) > request.PageSize {
				page.Items = page.Items[:request.PageSize]
				position := page.Items[len(page.Items)-1].position
				page.Next = &position
			}
		}
		if err != nil {
			return err
		}
		if err := markSuggestionOnlyMatches(ctx, q, compiled, generation.ID, coverage, request.Hidden, page.Items); err != nil {
			return err
		}
		return s.hydratePhotoPreviews(ctx, q, request, page.Items)
	})
	return page, err
}

// Force the outer relation to be the indexed sort source. Every following join
// uses an ownership key; the member predicate only visits this asset's files.
const photoBrowseDisplayFrom = `photo_assets a
 CROSS JOIN photo_files f ON f.file_id=a.display_file_id
 CROSS JOIN nodes n ON n.id=f.node_id
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
 LEFT JOIN source_metadata_generations mg ON mg.generation_id=h.generation_id
 LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id
 LEFT JOIN photo_timeline pl ON pl.file_id=f.file_id`

const photoBrowseLiveDisplay = liveIncludedDisplayPredicate + ` AND n.kind='file'`

func photoBrowseEligibleMemberPredicate(hidden bool) string {
	return `EXISTS (
 SELECT 1 FROM photo_files member JOIN photo_assets a ON a.asset_id=member.asset_id
 JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id
 WHERE member.node_id=cv.node_id AND ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(hidden) + `)`
}

const photoBrowseNodeJoins = `
 CROSS JOIN photo_files f ON f.node_id=n.id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
 LEFT JOIN source_metadata_generations mg ON mg.generation_id=h.generation_id
 LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id
 LEFT JOIN photo_timeline pl ON pl.file_id=f.file_id`

const photoBrowseVersionJoins = `
 CROSS JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id
 CROSS JOIN photo_files f ON f.node_id=n.id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
 LEFT JOIN source_metadata_generations mg ON mg.generation_id=h.generation_id
 LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id
 LEFT JOIN photo_timeline pl ON pl.file_id=f.file_id`

func photoBrowseOrder(field string) (from, key string, ok bool) {
	switch field {
	case "added_time":
		return `photo_set_members sm CROSS JOIN photo_assets a ON a.asset_id=sm.asset_id ` + strings.TrimPrefix(photoBrowseDisplayFrom, `photo_assets a`), "sm.added_at", true
	case "capture_time":
		return `photo_timeline pl INDEXED BY photo_timeline_sort_key
 CROSS JOIN photo_files f ON f.file_id=pl.file_id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 CROSS JOIN nodes n ON n.id=f.node_id
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
 LEFT JOIN source_metadata_generations mg ON mg.generation_id=h.generation_id
 LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id`,
			"pl.sort_key", true
	case "import_time":
		return `nodes n INDEXED BY nodes_photo_import` + photoBrowseNodeJoins, "n.created_at", true
	case "name":
		return `nodes n INDEXED BY nodes_photo_name` + photoBrowseNodeJoins, "substr(n.name,1,1024)", true
	case "modified_at":
		return `nodes n INDEXED BY nodes_live_modified` + photoBrowseNodeJoins, "n.modified_at", true
	case "size":
		return `content_versions v INDEXED BY content_versions_photo_size` + photoBrowseVersionJoins,
			"printf('%020d',v.size)", true
	case "media_type":
		return `content_versions v INDEXED BY content_versions_photo_media_type` + photoBrowseVersionJoins,
			"substr(COALESCE(v.mime_type,''),1,1024)", true
	default:
		return "", "", false
	}
}

func validatePhotoQuerySort(value query.Query) error {
	if value.Sort.Field == "added_time" && len(value.Filters.SetIDs) != 1 {
		return fmt.Errorf("%w: added_time requires exactly one filters.set_ids album", ErrInvalidPhotoQuery)
	}
	return nil
}

func photoBrowseMissingKey(field string) string {
	if field == "capture_time" {
		return "COALESCE(pl.sort_key,'')"
	}
	_, key, _ := photoBrowseOrder(field)
	return key
}

// photoBrowseScope selects the live, visible assets that match a compiled
// photo query in one collection generation.
type photoBrowseScope struct {
	match      compiledQueryFragment
	coverage   CoverageSelection
	generation string
	hidden     bool
}

// newPhotoBrowseScope checks a configured coverage profile and binds the
// match predicate. The caller normalizes coverage and checks hidden access.
func newPhotoBrowseScope(
	ctx context.Context, q metadataQuerier, compiled CompiledQuery,
	generation string, coverage CoverageSelection, hidden bool,
) (photoBrowseScope, error) {
	if coverage.Configuration == photoBrowseConfiguredCoverage {
		if err := validateSnapshotCoverageProfile(ctx, q, coverage); err != nil {
			return photoBrowseScope{}, err
		}
	}
	match, err := photoBrowseMatch(compiled, generation, coverage)
	if err != nil {
		return photoBrowseScope{}, err
	}
	return photoBrowseScope{match: match, coverage: coverage, generation: generation, hidden: hidden}, nil
}

// where filters rows selected from photoBrowseDisplayFrom to the scope.
func (s photoBrowseScope) where() string {
	return photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(s.hidden) + ` AND ` + s.match.sql
}

// bind supplies the shared relations the match needs. The statement must use
// where() before any placeholder for extra.
func (s photoBrowseScope) bind(statement string, extra ...any) (string, []any, error) {
	args := append(append([]any(nil), s.match.args...), extra...)
	return bindQueryPopulation(compiledQueryFragment{
		sql: statement, args: args, relations: s.match.relations,
	}, s.coverage, s.generation)
}

func photoBrowsePopulation(compiled CompiledQuery, generation string, coverage CoverageSelection) (compiledQueryFragment, error) {
	var profile *string
	if coverage.Configuration == photoBrowseConfiguredCoverage {
		profile = &coverage.ProfileFingerprint
	}
	compiled.predicate = joinCompiledFragments([]compiledQueryFragment{compiled.predicate, {sql: photoBrowseEligibleMemberPredicate(compiled.photoHidden)}}, ` AND `)
	return matchedPopulation(compiled, generation, profile)
}

func photoBrowseMatch(compiled CompiledQuery, generation string, coverage CoverageSelection) (compiledQueryFragment, error) {
	if !compiled.Query.Filters.CollapseDuplicates {
		var profile *string
		if coverage.Configuration == photoBrowseConfiguredCoverage {
			profile = &coverage.ProfileFingerprint
		}
		predicate, err := compiled.bind(generation, profile)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files member JOIN nodes n ON n.id=member.node_id JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE member.asset_id=a.asset_id AND ` + predicate.sql + `)`, args: predicate.args, relations: predicate.relations}, nil
	}
	population, err := photoBrowsePopulation(compiled, generation, coverage)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files member CROSS JOIN (` + population.sql + `) matched ON matched.node_id=member.node_id WHERE member.asset_id=a.asset_id)`, args: population.args, relations: population.relations}, nil
}

// photoTextOperands joins the matches of the non-negated text operands, all
// of them and those that also search retained content.
func photoTextOperands(compiled CompiledQuery) (names, content string) {
	var all, retained []string
	for _, operand := range compiled.predicate.lexical {
		if operand.negated {
			continue
		}
		all = append(all, "("+operand.match+")")
		if operand.content {
			retained = append(retained, "("+operand.match+")")
		}
	}
	return strings.Join(all, " OR "), strings.Join(retained, " OR ")
}

// markSuggestionOnlyMatches marks the rows of a page that match the whole
// query only because of a pending suggestion: they drop out when the query
// is evaluated without suggestion text.
func markSuggestionOnlyMatches(
	ctx context.Context, q metadataQuerier, compiled CompiledQuery,
	generation string, coverage CoverageSelection, hidden bool, items []PhotoBrowseRow,
) error {
	if _, content := photoTextOperands(compiled); content == "" || len(items) == 0 {
		return nil
	}
	if suggestions, err := photoSuggestionsExist(ctx, q); err != nil || !suggestions {
		return err
	}
	compiled.withoutSuggestion = true
	scope, err := newPhotoBrowseScope(ctx, q, compiled, generation, coverage, hidden)
	if err != nil {
		return err
	}
	args := make([]any, len(items))
	for i, item := range items {
		args[i] = item.AssetID
	}
	statement, args, err := scope.bind(`SELECT a.asset_id FROM photo_assets a WHERE `+scope.match.sql+` AND a.asset_id IN (`+placeholders(len(items))+`)`, args...)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	plain := map[string]bool{}
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			return err
		}
		plain[assetID] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range items {
		items[i].MatchedAISuggestion = !plain[items[i].AssetID]
	}
	return nil
}

func rankedPhotoStatement(compiled CompiledQuery, generation string, coverage CoverageSelection, pageSize int, snapshot, suggestions bool) (string, []any, error) {
	names, content := photoTextOperands(compiled)
	population, err := photoBrowsePopulation(compiled, generation, coverage)
	if err != nil {
		return "", nil, err
	}
	candidates := sqlquery.Query{SQL: `SELECT 0 node_id,0 tier,0.0 score WHERE 0`}
	if names != "" {
		var profile *string
		if coverage.Configuration == photoBrowseConfiguredCoverage {
			profile = &coverage.ProfileFingerprint
		}
		candidates, err = photoSearchCandidates(names, content, generation, profile)
		if err != nil {
			return "", nil, err
		}
	}
	// A photo that matches the whole query only with suggestion text ranks no
	// higher than the suggestion tier, below any fully authored match.
	// Without suggestion-searchable text every match is a plain one.
	plain := compiledQueryFragment{sql: `SELECT node_id FROM eligible`}
	if content != "" && suggestions {
		withoutSuggestion := compiled
		withoutSuggestion.withoutSuggestion = true
		if plain, err = photoBrowsePopulation(withoutSuggestion, generation, coverage); err != nil {
			return "", nil, err
		}
	}
	ranking := `WITH eligible AS MATERIALIZED (` + population.sql + `), plain AS MATERIALIZED (` + plain.sql + `), candidates AS (` + candidates.SQL + `), ranked AS (
 SELECT member.asset_id,NOT MAX(plain.node_id IS NOT NULL) suggestion_only,
 CASE WHEN MAX(plain.node_id IS NOT NULL) THEN COALESCE(MAX(candidates.tier),0) ELSE MIN(COALESCE(MAX(candidates.tier),0),3) END tier,
 COALESCE(CASE WHEN MAX(plain.node_id IS NOT NULL) THEN MAX(CASE WHEN candidates.tier=5 THEN candidates.score END) END,CASE WHEN MAX(plain.node_id IS NOT NULL) THEN MAX(CASE WHEN candidates.tier=4 THEN candidates.score END) END,MAX(CASE WHEN candidates.tier=3 THEN candidates.score END),MAX(CASE WHEN candidates.tier=2 THEN candidates.score END),MAX(CASE WHEN candidates.tier=1 THEN candidates.score END),0) score
 FROM eligible JOIN photo_files member ON member.node_id=eligible.node_id
	 LEFT JOIN plain ON plain.node_id=eligible.node_id
	 LEFT JOIN candidates ON candidates.node_id=eligible.node_id GROUP BY member.asset_id)`
	args := append(append(append([]any(nil), population.args...), plain.args...), candidates.Args...)
	population.relations |= plain.relations
	order := "DESC"
	if compiled.Query.Sort.Direction == "asc" {
		order = "ASC"
	}
	orderBy := `member.tier ` + order + `,member.score ` + order + `,COALESCE(pl.sort_key,'') DESC,member.asset_id ASC`
	projection := photoBrowseSelect + `,'',member.suggestion_only`
	if snapshot {
		projection = `n.id node_id,v.version_id content_version_id,a.asset_id,row_number() OVER (ORDER BY ` + orderBy + `) photo_position`
	}
	ranking += ` SELECT ` + projection + ` FROM ranked member
 CROSS JOIN photo_assets a ON a.asset_id=member.asset_id ` + strings.TrimPrefix(photoBrowseDisplayFrom, `photo_assets a`) +
		` WHERE ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(compiled.photoHidden) + ` ORDER BY ` + orderBy + ` LIMIT ?`
	args = append(args, pageSize)
	statement, args, err := bindQueryPopulation(compiledQueryFragment{args: args, relations: population.relations}, coverage, generation)
	if err != nil {
		return "", nil, err
	}
	if statement == "" {
		statement = ranking
	} else {
		statement = strings.TrimSpace(statement) + ", " + strings.TrimPrefix(ranking, "WITH ")
	}
	return statement, args, nil
}

// photoSuggestionsExist reports whether any suggestion row exists, so a library
// without descriptions skips the suggestion-aware ranking pass.
func photoSuggestionsExist(ctx context.Context, q metadataQuerier) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_suggestions)`).Scan(&exists)
	return exists, err
}

func rankedPhotoRows(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, coverage CoverageSelection, pageSize int) ([]PhotoBrowseRow, error) {
	suggestions, err := photoSuggestionsExist(ctx, q)
	if err != nil {
		return nil, err
	}
	statement, args, err := rankedPhotoStatement(compiled, generation, coverage, pageSize, false, suggestions)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	return scanPhotoBrowseRows(rows, PhotoBrowsePosition{})
}

var photoBrowseSelect = `a.asset_id,a.kind,a.revision,f.file_id,n.id,v.version_id,n.name,COALESCE(v.mime_type,''),n.created_at,v.blob_hash,f.rotation,f.rating,f.flag,` +
	// Set only when the file's mtime, not a capture date, placed the photo.
	`pl.modified_time,CASE WHEN a.kind='photo' THEN COALESCE(pl.folder_key,'') ELSE '' END,` + photoStackCameraColumns + `,COALESCE(mg.extractor_fingerprint,''),` + photoTechnicalSelect

func scanPhotoBrowseRows(rows *sql.Rows, position PhotoBrowsePosition) ([]PhotoBrowseRow, error) {
	defer func() { _ = rows.Close() }()
	result := make([]PhotoBrowseRow, 0)
	for rows.Next() {
		row := PhotoBrowseRow{position: position}
		dest := []any{&row.AssetID, &row.Kind, &row.Revision, &row.DisplayFileID, &row.NodeID, &row.ContentVersionID, &row.Name, &row.MediaType, &row.ImportTime, &row.sourceHash, &row.Rotation, &row.Rating, &row.Flag, &row.FileModifiedTime, &row.folderKey, &row.cameraMake, &row.cameraModel, &row.bodySerialNumber, &row.metadataFingerprint}
		dest = append(dest, row.Fields.columnPointers()...)
		dest = append(dest, &row.position.Key, &row.MatchedAISuggestion)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row.position.AssetID = row.AssetID
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, rows.Close()
}

func (s *Store) hydratePhotoPreviews(ctx context.Context, q metadataQuerier, request PhotoBrowseRequest, items []PhotoBrowseRow) error {
	versions := make([]string, len(items))
	for i, row := range items {
		versions[i] = row.ContentVersionID
	}
	previews, err := photoPreviewGenerations(ctx, q, versions, request.Recipes)
	if err != nil {
		return err
	}
	qualities, err := photoQualityForVersions(ctx, q, versions)
	if err != nil {
		return err
	}
	for i := range items {
		row := &items[i]
		if signals, ok := qualities.signals[row.ContentVersionID]; ok {
			row.Quality = &signals
		}
		row.QualityUnavailable = qualities.unavailable[row.ContentVersionID]
		row.Previews = make(map[string]PhotoPreviewSlot, 3)
		for _, size := range []string{"grid", "fit", "large"} {
			slot := PhotoPreviewSlot{State: "missing"}
			if preview, ok := previews[row.ContentVersionID][request.Recipes[size]]; ok {
				if preview.VaultID != s.vaultID || preview.Preview.SourceSHA256 != row.sourceHash {
					return errors.New("preview source binding mismatch")
				}
				slot.State = string(preview.Preview.State)
				slot.Generation = &preview
				if preview.Preview.State == document.VisualPreviewReady && preview.Preview.Output == nil {
					return errors.New("ready preview lacks output")
				}
			}
			row.Previews[size] = slot
		}
	}
	return nil
}

func normalizePhotoBrowseRequest(request PhotoBrowseRequest) (PhotoBrowseRequest, error) {
	if request.PageSize == 0 {
		request.PageSize = DefaultDocumentCatalogPageSize
	}
	if request.PageSize < 1 || request.PageSize > MaxPhotoMapPageSize {
		return request, ErrInvalidPhotoQuery
	}
	var err error
	request.Coverage, err = normalizeCoverageSelection(request.Coverage)
	if err != nil {
		return request, err
	}
	request.Facets, err = normalizeFacetDimensions(request.Facets, photoFacetDimensions)
	if err != nil {
		return request, fmt.Errorf("%w: %w", ErrInvalidPhotoQuery, err)
	}
	return request, nil
}
