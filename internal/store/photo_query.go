package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"strings"
)

const photoBrowseConfiguredCoverage = "configured"

// PhotoBrowseRequest supplies query meaning and exact preview recipe identities.
type PhotoBrowseRequest struct {
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

// ListPhotoAssets projects complete matching members into one row per included asset.
func (s *Store) ListPhotoAssets(ctx context.Context, request PhotoBrowseRequest, boundary *PhotoBrowsePosition) (PhotoBrowsePage, error) {
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
		compiled, err := compileQuery(ctx, request.Query, queryResolver{q: q})
		if err != nil {
			return err
		}
		sortField := compiled.Query.Sort.Field
		sortKeys := map[string]string{"capture_time": photoCaptureKeySQL, "import_time": "n.created_at", "name": "n.name", "modified_at": "n.modified_at", "size": "printf('%020d',v.size)", "media_type": "COALESCE(v.mime_type,'')"}
		sortKey, ok := sortKeys[sortField]
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
		}{canonical, compiled.Dependencies, coverage, request.PageSize})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(binding)
		identity := hex.EncodeToString(digest[:])
		if boundary != nil {
			if boundary.QueryIdentity != identity || validateUUIDv4(boundary.AssetID) != nil || len(boundary.Key) > MaxWalkPathBytes {
				return ErrInvalidPhotoCursor
			}
		}
		scope := compiledQueryFragment{sql: `EXISTS (SELECT 1 FROM photo_files member JOIN photo_assets a ON a.asset_id=member.asset_id JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id JOIN content_versions v ON v.version_id=n.current_version_id WHERE member.node_id=cv.node_id AND ` + liveIncludedDisplayPredicate + `)`}
		compiled.predicate = joinCompiledFragments([]compiledQueryFragment{compiled.predicate, scope}, ` AND `)
		var profile *string
		if coverage.Configuration == photoBrowseConfiguredCoverage {
			profile = &coverage.ProfileFingerprint
		}
		population, err := matchedPopulation(compiled, generation.ID, profile)
		if err != nil {
			return err
		}
		populationSQL, args, err := bindQueryPopulation(population, coverage, generation.ID)
		if err != nil {
			return err
		}
		cte := `WITH matched AS (` + populationSQL + `), assets AS (
   SELECT DISTINCT pf.asset_id FROM matched m JOIN photo_files pf ON pf.node_id=m.node_id
  ), displayed AS (
   SELECT a.asset_id,a.kind,a.revision,f.file_id,n.id node_id,v.version_id,n.name,COALESCE(v.mime_type,'') mime_type,n.created_at,
    v.blob_hash,` + photoTechnicalSelect + `,` + sortKey + ` sort_key
   FROM assets matched_asset JOIN photo_assets a ON a.asset_id=matched_asset.asset_id
   JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id
   JOIN content_versions v ON v.version_id=n.current_version_id
   LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
   LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id
   WHERE ` + liveIncludedDisplayPredicate + `
  ), ordered AS (SELECT *,CASE WHEN sort_key='' THEN 1 ELSE 0 END missing FROM displayed) `
		if err := q.QueryRowContext(ctx, cte+`SELECT COUNT(*) FROM ordered`, args...).Scan(&page.Total); err != nil {
			return err
		}
		primary := "ASC"
		comparison := ">"
		if compiled.Query.Sort.Direction == "desc" {
			primary = "DESC"
			comparison = "<"
		}
		where := ""
		pageArgs := append([]any(nil), args...)
		if boundary != nil {
			missing := 0
			if boundary.Missing {
				missing = 1
			}
			where = `WHERE missing>? OR (missing=? AND (sort_key ` + comparison + ` ? OR (sort_key=? AND asset_id>?)))`
			pageArgs = append(pageArgs, missing, missing, boundary.Key, boundary.Key, boundary.AssetID)
		}
		pageArgs = append(pageArgs, request.PageSize+1)
		rows, err := q.QueryContext(ctx, cte+`SELECT asset_id,kind,revision,file_id,node_id,version_id,name,mime_type,created_at,blob_hash,`+strings.Join(photoTechnicalColumns, ",")+`,sort_key,missing FROM ordered `+where+` ORDER BY missing ASC,sort_key `+primary+`,asset_id ASC LIMIT ?`, pageArgs...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		page.Items = make([]PhotoBrowseRow, 0, request.PageSize+1)
		for rows.Next() {
			var row PhotoBrowseRow
			row.position.QueryIdentity = identity
			var missing int
			dest := []any{&row.AssetID, &row.Kind, &row.Revision, &row.DisplayFileID, &row.NodeID, &row.ContentVersionID, &row.Name, &row.MediaType, &row.ImportTime, &row.sourceHash}
			dest = append(dest, row.Fields.columnPointers()...)
			dest = append(dest, &row.position.Key, &missing)
			if err := rows.Scan(dest...); err != nil {
				_ = rows.Close()
				return err
			}
			row.position.Missing = missing != 0
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
