package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

const photoBrowseConfiguredCoverage = "configured"

// PhotoBrowseRequest supplies query meaning and exact preview recipe identities.
type PhotoBrowseRequest struct {
	SetID    string
	Query    query.Query
	Coverage CoverageSelection
	PageSize int
	Recipes  map[string]string
}

// PhotoBrowsePosition is the selected SQL key bound to the resolved query.
type PhotoBrowsePosition struct {
	Key           string `json:"key"`
	Missing       bool   `json:"missing"`
	AssetID       string `json:"asset_id"`
	QueryIdentity string `json:"query_identity"`
	Total         int64  `json:"total"`
}

type PhotoPreviewSlot struct {
	State      string
	Generation *VisualPreviewGeneration
}

type PhotoBrowseRow struct {
	AssetID          string
	Kind             string
	Revision         int64
	DisplayFileID    string
	NodeID           int64
	ContentVersionID string
	Name             string
	MediaType        string
	ImportTime       string
	Fields           PhotoTechnicalFields
	Previews         map[string]PhotoPreviewSlot
	position         PhotoBrowsePosition
	sourceHash       string
}

type PhotoBrowsePage struct {
	Items []PhotoBrowseRow
	Total int64
	Next  *PhotoBrowsePosition
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
	if request.PageSize == 0 {
		request.PageSize = DefaultDocumentCatalogPageSize
	}
	if request.PageSize < 1 || request.PageSize > MaxDocumentCatalogPageSize {
		return PhotoBrowsePage{}, ErrInvalidPhotoQuery
	}
	coverage, err := normalizeCoverageSelection(request.Coverage)
	if err != nil {
		return PhotoBrowsePage{}, err
	}
	var page PhotoBrowsePage
	err = s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) error {
		compiled, err := (queryCompiler{photoDisplayMetadata: true}).compile(
			ctx, request.Query, queryResolver{q: q})
		if err != nil {
			return err
		}
		sortField := compiled.Query.Sort.Field
		from, sortKey, ok := photoBrowseOrder(sortField)
		if request.SetID != "" {
			if _, err := photoSetByID(ctx, q, request.SetID); err != nil {
				return err
			}
			scope, err := (queryCompiler{}).compilePhotoScalarPredicate("set", request.SetID)
			if err != nil {
				return err
			}
			compiled.predicate = joinCompiledFragments([]compiledQueryFragment{compiled.predicate, scope}, ` AND `)
		}
		if sortField == "added_time" {
			if request.SetID == "" {
				return ErrInvalidPhotoQuery
			}
			from = `photo_set_members sm CROSS JOIN photo_assets a ON a.asset_id=sm.asset_id ` + strings.TrimPrefix(photoBrowseDisplayFrom, `photo_assets a`)
			sortKey, ok = "sm.added_at", true
		}
		if !ok {
			return fmt.Errorf("%w: unsupported sort", ErrInvalidPhotoQuery)
		}
		if coverage.Configuration == photoBrowseConfiguredCoverage {
			if err := validateSnapshotCoverageProfile(ctx, q, coverage); err != nil {
				return err
			}
		}
		canonical, err := query.Canonical(compiled.Query)
		if err != nil {
			return err
		}
		binding, err := json.Marshal(struct {
			Query        []byte
			Dependencies []query.Dependency
			Coverage     CoverageSelection
			PageSize     int
			SetID        string
		}{canonical, compiled.Dependencies, coverage, request.PageSize, request.SetID})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(binding)
		identity := hex.EncodeToString(digest[:])
		if boundary != nil {
			if boundary.QueryIdentity != identity || validateUUIDv4(boundary.AssetID) != nil ||
				len(boundary.Key) > MaxPhotoSortKeyBytes || boundary.Total < 0 {
				return ErrInvalidPhotoCursor
			}
		}
		match, err := photoBrowseMatch(compiled, generation.ID, coverage)
		if err != nil {
			return err
		}
		bind := func(sql string, args []any) (string, []any, error) {
			return bindQueryPopulation(compiledQueryFragment{
				sql: sql, args: args, relations: match.relations,
			}, coverage, generation.ID)
		}
		if boundary == nil {
			countSQL, countArgs, err := bind(`SELECT COUNT(*) FROM `+photoBrowseDisplayFrom+
				` WHERE `+photoBrowseLiveDisplay+` AND `+match.sql, match.args)
			if err != nil {
				return err
			}
			if err := q.QueryRowContext(ctx, countSQL, countArgs...).Scan(&page.Total); err != nil {
				return err
			}
		} else {
			page.Total = boundary.Total
		}
		primary, comparison := "ASC", ">"
		if compiled.Query.Sort.Direction == "desc" {
			primary, comparison = "DESC", "<"
		}
		page.Items = make([]PhotoBrowseRow, 0, request.PageSize+1)
		// Read known keys from their index, then missing keys in asset order.
		// Keeping these separate avoids sorting the entire library to put NULLs last.
		for _, missing := range []bool{false, true} {
			if !missing && boundary != nil && boundary.Missing {
				continue
			}
			pageFrom, key := from, sortKey
			order := key + ` ` + primary + `,a.asset_id ASC`
			where := key + `>''`
			args := append([]any(nil), match.args...)
			if sortField == "added_time" {
				where = `sm.set_id=? AND ` + where
				args = append(args, request.SetID)
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
			args = append(args, request.PageSize+1-len(page.Items))
			pageSQL, pageArgs, err := bind(`SELECT a.asset_id,a.kind,a.revision,f.file_id,
 n.id,v.version_id,n.name,COALESCE(v.mime_type,''),n.created_at,v.blob_hash,`+
				photoTechnicalSelect+`,`+key+` FROM `+pageFrom+` WHERE `+
				photoBrowseLiveDisplay+` AND `+match.sql+` AND `+where+
				` ORDER BY `+order+` LIMIT ?`, args)
			if err != nil {
				return err
			}
			rows, err := q.QueryContext(ctx, pageSQL, pageArgs...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				row := PhotoBrowseRow{position: PhotoBrowsePosition{
					QueryIdentity: identity, Total: page.Total, Missing: missing,
				}}
				dest := []any{&row.AssetID, &row.Kind, &row.Revision, &row.DisplayFileID,
					&row.NodeID, &row.ContentVersionID, &row.Name, &row.MediaType,
					&row.ImportTime, &row.sourceHash}
				dest = append(dest, row.Fields.columnPointers()...)
				dest = append(dest, &row.position.Key)
				if err := rows.Scan(dest...); err != nil {
					_ = rows.Close()
					return err
				}
				row.position.AssetID = row.AssetID
				page.Items = append(page.Items, row)
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if len(page.Items) > request.PageSize {
				break
			}
		}
		if len(page.Items) > request.PageSize {
			page.Items = page.Items[:request.PageSize]
			position := page.Items[len(page.Items)-1].position
			page.Next = &position
		}
		versions := make([]string, len(page.Items))
		for i, row := range page.Items {
			versions[i] = row.ContentVersionID
		}
		previews, err := photoPreviewGenerations(ctx, q, versions, request.Recipes)
		if err != nil {
			return err
		}
		for i := range page.Items {
			row := &page.Items[i]
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
	})
	if err != nil {
		return PhotoBrowsePage{}, err
	}
	return page, nil
}

// Force the outer relation to be the indexed sort source. Every following join
// uses an ownership key; the member predicate only visits this asset's files.
const photoBrowseDisplayFrom = `photo_assets a
 CROSS JOIN photo_files f ON f.file_id=a.display_file_id
 CROSS JOIN nodes n ON n.id=f.node_id
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
 LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id`

const photoBrowseLiveDisplay = liveIncludedDisplayPredicate + ` AND n.kind='file'`

const photoBrowseEligibleMemberPredicate = `EXISTS (
 SELECT 1 FROM photo_files member JOIN photo_assets a ON a.asset_id=member.asset_id
 JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id
 WHERE member.node_id=cv.node_id AND ` + photoBrowseLiveDisplay + `)`

const photoBrowseNodeJoins = `
 CROSS JOIN photo_files f ON f.node_id=n.id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
 LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id`

const photoBrowseVersionJoins = `
 CROSS JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id
 CROSS JOIN photo_files f ON f.node_id=n.id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
 LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id`

func photoBrowseOrder(field string) (from, key string, ok bool) {
	switch field {
	case "capture_time":
		return `photo_technical_metadata p INDEXED BY photo_technical_metadata_capture_sort_key
 CROSS JOIN source_metadata_heads h ON h.generation_id=p.generation_id
 CROSS JOIN content_versions v ON v.blob_hash=h.source_sha256
 CROSS JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id
 CROSS JOIN photo_files f ON f.node_id=n.id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id`,
			"p.capture_sort_key", true
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

func photoBrowseMissingKey(field string) string {
	if field == "capture_time" {
		return "COALESCE(p.capture_sort_key,'')"
	}
	_, key, _ := photoBrowseOrder(field)
	return key
}

func photoBrowseMatch(
	compiled CompiledQuery, generation string, coverage CoverageSelection,
) (compiledQueryFragment, error) {
	var profile *string
	if coverage.Configuration == photoBrowseConfiguredCoverage {
		profile = &coverage.ProfileFingerprint
	}
	if !compiled.Query.Filters.CollapseDuplicates {
		predicate, err := compiled.bind(generation, profile)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files member
 JOIN nodes n ON n.id=member.node_id
 JOIN content_versions cv ON cv.version_id=n.current_version_id
 WHERE member.asset_id=a.asset_id AND ` + predicate.sql + `)`,
			args: predicate.args, relations: predicate.relations}, nil
	}
	// Duplicate representatives are selected from the complete matching photo
	// population, as for document queries, before projecting assets.
	compiled.predicate = joinCompiledFragments([]compiledQueryFragment{
		compiled.predicate, {sql: photoBrowseEligibleMemberPredicate},
	}, ` AND `)
	population, err := matchedPopulation(compiled, generation, profile)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files member
 CROSS JOIN (` + population.sql + `) matched ON matched.node_id=member.node_id
 WHERE member.asset_id=a.asset_id)`,
		args: population.args, relations: population.relations}, nil
}
