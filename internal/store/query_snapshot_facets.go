package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/query"
	"golang.org/x/text/cases"
)

var snapshotFacetDimensions = [...]string{
	"collections", snapshotFacetTags, snapshotFacetMediaFamily, "extension", "modified", "size", snapshotFacetTextCoverage, "duplicates",
}

const (
	snapshotFacetTags         = "tags"
	snapshotFacetMediaFamily  = "media_family"
	snapshotFacetTextCoverage = "text_coverage"
	snapshotFacetYear         = "year"
)

func normalizeSnapshotFacets(values []string) ([]string, error) {
	return normalizeFacetDimensions(values, snapshotFacetDimensions[:])
}

func normalizeFacetDimensions(values, dimensions []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !slices.Contains(dimensions, value) {
			return nil, errors.New("unknown snapshot facet dimension")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, errors.New("duplicate snapshot facet dimension")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func materializeSnapshotFacets(
	ctx context.Context, q metadataQuerier, compiled CompiledQuery, generationID string,
	coverage CoverageSelection, dimensions []string, rows []SnapshotRow, options snapshotMaterializeOptions, used *int64,
) ([]SnapshotFacet, error) {
	result := make([]SnapshotFacet, 0, len(dimensions))
	if err := chargeSnapshotMaterialization(options, used, 0, 2); err != nil {
		return nil, err
	}
	if len(dimensions) == 0 {
		return result, nil
	}
	facetCtx, cancel := context.WithTimeout(ctx, options.FacetTimeout)
	defer cancel()
	for index, dimension := range dimensions {
		if dimension == snapshotFacetTextCoverage && coverage.Configuration != "configured" {
			facet := unavailableSnapshotFacet(dimension, "coverage_unconfigured")
			if err := appendChargedSnapshotFacet(&result, facet, options, used); err != nil {
				return nil, err
			}
			continue
		}
		var facet SnapshotFacet
		var err error
		if options.MaterializeFacet != nil {
			facet, err = options.MaterializeFacet(facetCtx, dimension)
		} else {
			facet, err = materializeSnapshotFacet(facetCtx, q, compiled, generationID, coverage, dimension, rows, options.FacetMemberLimit)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if facetCtx.Err() != nil {
				for _, remaining := range dimensions[index:] {
					facet := unavailableSnapshotFacet(remaining, "time_budget_exceeded")
					if err := appendChargedSnapshotFacet(&result, facet, options, used); err != nil {
						return nil, err
					}
				}
				break
			}
			return nil, err
		}
		if err := appendChargedSnapshotFacet(&result, facet, options, used); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func appendChargedSnapshotFacet(
	result *[]SnapshotFacet, facet SnapshotFacet, options snapshotMaterializeOptions, used *int64,
) error {
	encoded, err := json.Marshal(facet)
	if err != nil {
		return fmt.Errorf("encoding snapshot facet: %w", err)
	}
	bytes := int64(len(encoded))
	if len(*result) != 0 {
		bytes++ // JSON array separator; container brackets are charged by the caller.
	}
	if err := chargeSnapshotMaterialization(options, used, 0, bytes); err != nil {
		return err
	}
	*result = append(*result, facet)
	return nil
}

type snapshotFacetObservation struct {
	counts  map[string]int64
	labels  map[string]string
	total   int64
	missing int64
}

func materializeSnapshotFacet(
	ctx context.Context, q metadataQuerier, compiled CompiledQuery, generationID string,
	coverage CoverageSelection, dimension string, snapshotRows []SnapshotRow, memberLimit int64,
) (SnapshotFacet, error) {
	facetQuery := queryWithoutSnapshotFacet(compiled.Query, dimension)
	if dimension != "duplicates" && reflect.DeepEqual(facetQuery.Filters, compiled.Query.Filters) {
		return materializeSnapshotFacetFromRows(ctx, q, compiled.Query, dimension, snapshotRows, memberLimit)
	}
	facetCompiled, err := compileQuery(ctx, facetQuery, queryResolver{q: q})
	if err != nil {
		return SnapshotFacet{}, err
	}
	population, err := matchedPopulation(facetCompiled, generationID, nil)
	if err != nil {
		return SnapshotFacet{}, err
	}
	statement, args, err := bindQueryPopulation(population, coverage, generationID)
	if err != nil {
		return SnapshotFacet{}, err
	}
	ctes, join, key, label := "", "", `''`, `''`
	switch dimension {
	case "collections":
		ctes = `, ` + CollectionMembershipCTE
		join = `LEFT JOIN collection_members cm ON cm.node_id=n.id LEFT JOIN collection_labels cl ON cl.ingest_id=cm.ingest_id`
		key, label = `COALESCE(cm.ingest_id,'')`, `COALESCE(cl.label,cm.ingest_id,'')`
	case snapshotFacetTags:
		join = `LEFT JOIN node_tags nt ON nt.node_id=n.id LEFT JOIN tags t ON t.id=nt.tag_id`
		key, label = `COALESCE(t.id,'')`, `COALESCE(t.name,'')`
	case snapshotFacetTextCoverage:
		ctes = `, coverage_members AS (SELECT node_id FROM matched_members), ` + processingCoverageCTE()
		join = `JOIN processing_coverage pc ON pc.node_id=n.id AND pc.version_id=cv.version_id`
		key, label = `pc.state`, `pc.state`
		args = append(args, coverage.ProfileFingerprint, generationID)
	case "duplicates":
		ctes = `, ` + CurrentContentMembershipCTE
		key = `CASE WHEN EXISTS(SELECT 1 FROM current_content_members peer WHERE peer.blob_hash=cv.blob_hash AND peer.node_id<>n.id) THEN 'duplicate' ELSE 'unique' END`
		label = key
	}
	rows, err := q.QueryContext(ctx, `WITH matched_members AS (`+statement+`)`+ctes+`
		SELECT n.id,n.name,COALESCE(cv.mime_type,''),cv.size,n.modified_at,`+key+`,`+label+`
		FROM matched_members m JOIN nodes n ON n.id=m.node_id
		JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=m.content_version_id
		`+join+` ORDER BY n.id,`+key, args...)
	if err != nil {
		return SnapshotFacet{}, fmt.Errorf("materializing %s facet: %w", dimension, err)
	}
	defer func() { _ = rows.Close() }()
	observation, limited, err := observeSnapshotFacet(rows, dimension, memberLimit, func() (string, string, string, error) {
		var nodeID, size int64
		var name, mimeType, modifiedAt, value, label string
		err := rows.Scan(&nodeID, &name, &mimeType, &size, &modifiedAt, &value, &label)
		if dimension != "collections" && dimension != snapshotFacetTags && dimension != snapshotFacetTextCoverage && dimension != "duplicates" {
			value = snapshotFacetScalarValue(dimension, name, mimeType, modifiedAt, size)
			label = value
		}
		return strconv.FormatInt(nodeID, 10), value, label, err
	})
	if err != nil {
		return SnapshotFacet{}, fmt.Errorf("materializing %s facet: %w", dimension, err)
	}
	if limited {
		return unavailableSnapshotFacet(dimension, "member_budget_exceeded"), nil
	}
	return finishSnapshotFacet(ctx, q, compiled.Query, dimension, observation)
}

func materializeSnapshotFacetFromRows(
	ctx context.Context, q metadataQuerier, value query.Query, dimension string, rows []SnapshotRow, memberLimit int64,
) (SnapshotFacet, error) {
	observation := snapshotFacetObservation{counts: make(map[string]int64), labels: make(map[string]string)}
	var projectedRows int64
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return SnapshotFacet{}, err
		}
		memberships := 1
		switch dimension {
		case "collections":
			memberships = max(1, len(row.CollectionIDs))
		case snapshotFacetTags:
			memberships = max(1, len(row.Tags))
		}
		projectedRows += int64(memberships)
		if projectedRows > memberLimit {
			return unavailableSnapshotFacet(dimension, "member_budget_exceeded"), nil
		}
		observation.total++
		switch dimension {
		case "collections":
			for _, id := range row.CollectionIDs {
				observation.counts[id]++
			}
			if len(row.CollectionIDs) == 0 {
				observation.missing++
			}
		case snapshotFacetTags:
			for _, tag := range row.Tags {
				observation.counts[tag.ID]++
				observation.labels[tag.ID] = tag.Name
			}
			if len(row.Tags) == 0 {
				observation.missing++
			}
		default:
			key := row.CoverageState
			if dimension != snapshotFacetTextCoverage {
				key = snapshotFacetScalarValue(dimension, row.Name, row.MIMEType, row.ModifiedAt, row.Size)
			}
			if key == "" {
				observation.missing++
			} else {
				observation.counts[key]++
				observation.labels[key] = key
			}
		}
	}
	return finishSnapshotFacet(ctx, q, value, dimension, observation)
}

func finishSnapshotFacet(
	ctx context.Context, q metadataQuerier, value query.Query, dimension string, observation snapshotFacetObservation,
) (SnapshotFacet, error) {
	if err := ctx.Err(); err != nil {
		return SnapshotFacet{}, err
	}
	selected := snapshotFacetSelected(value, dimension)
	if dimension == compiledCameraField || dimension == compiledLensField {
		raw := value.Filters.Cameras
		if dimension == compiledLensField {
			raw = value.Filters.Lenses
		}
		for _, label := range raw {
			key := cases.Fold().String(label)
			if observed, ok := observation.labels[key]; !ok || query.ValidateTextOperand(dimension, observed) != nil {
				observation.labels[key] = label
			}
		}
	}
	fixed := fixedSnapshotFacetValues(dimension)
	for _, value := range fixed {
		if _, ok := observation.counts[value]; !ok {
			observation.counts[value] = 0
		}
		if _, ok := observation.labels[value]; !ok {
			observation.labels[value] = value
		}
	}
	values, other := finalizeSnapshotFacetValues(observation.counts, observation.labels, selected, len(fixed) != 0)
	visible := make(map[string]bool, len(values))
	for _, value := range values {
		visible[value.Key] = true
	}
	if err := fillSelectedSnapshotFacetLabels(ctx, q, dimension, visible, observation.labels); err != nil {
		return SnapshotFacet{}, err
	}
	for i := range values {
		values[i].Label = observation.labels[values[i].Key]
		if dimension == compiledCameraField || dimension == compiledLensField {
			values[i].Key = values[i].Label
		}
	}
	total, missing := observation.total, observation.missing
	return SnapshotFacet{
		Dimension: dimension, Available: true, Total: &total, Values: values,
		Missing: &missing, Other: &other,
	}, nil
}

// Facet counts omit only their root structured filter. Expression operands and
// nested saved queries retain their constraints in the same read transaction.
func queryWithoutSnapshotFacet(value query.Query, dimension string) query.Query {
	switch dimension {
	case compiledCameraField:
		value.Filters.Cameras = nil
	case compiledLensField:
		value.Filters.Lenses = nil
	case compiledLocationField:
		value.Filters.Locations = nil
	case snapshotFacetYear:
		if wholeCaptureYear(value) != "" {
			value.Filters.CaptureAfter, value.Filters.CaptureBefore = "", ""
		}
	case compiledSetField:
		value.Filters.SetIDs = nil
	case "collections":
		value.Filters.CollectionIDs, value.Filters.ExcludeCollectionIDs = nil, nil
	case snapshotFacetTags:
		value.Filters.TagIDs, value.Filters.ExcludeTagIDs = nil, nil
		value.Filters.NoTags = false
	case snapshotFacetMediaFamily:
		value.Filters.MediaFamilies = nil
	case "extension":
		value.Filters.Extensions = nil
	case "modified":
		value.Filters.ModifiedAfter, value.Filters.ModifiedBefore = "", ""
	case "size":
		value.Filters.SizeMin, value.Filters.SizeMax = 0, nil
	case snapshotFacetTextCoverage:
		value.Filters.TextCoverage = nil
	case "duplicates":
		value.Filters.HasDuplicates = false
	}
	return value
}

func unavailableSnapshotFacet(dimension, reason string) SnapshotFacet {
	return SnapshotFacet{Dimension: dimension, Available: false, Reason: reason}
}

func snapshotFacetScalarValue(dimension, name, mimeType, modifiedAt string, size int64) string {
	switch dimension {
	case snapshotFacetMediaFamily:
		return query.ClassifyMedia(mimeType, name)
	case "extension":
		return query.FilenameExtension(name)
	case "modified":
		parsed, err := time.Parse(time.RFC3339Nano, modifiedAt)
		if err != nil || parsed.UTC().Year() < 0 || parsed.UTC().Year() > 9999 {
			return ""
		}
		return parsed.UTC().Format("2006-01")
	case "size":
		switch {
		case size < 1<<20:
			return "lt_1_mib"
		case size < 10<<20:
			return "1_mib_to_10_mib"
		case size < 100<<20:
			return "10_mib_to_100_mib"
		case size < 1<<30:
			return "100_mib_to_1_gib"
		default:
			return "gte_1_gib"
		}
	}
	return ""
}

func fixedSnapshotFacetValues(dimension string) []string {
	switch dimension {
	case snapshotFacetMediaFamily:
		return []string{"email", "document", "spreadsheet", "presentation", "image", "audio_video", "text", "source_code", "web", "calendar", "archive", "cad", "unknown"}
	case "size":
		return []string{"lt_1_mib", "1_mib_to_10_mib", "10_mib_to_100_mib", "100_mib_to_1_gib", "gte_1_gib"}
	case snapshotFacetTextCoverage:
		return []string{"complete", "partial", "failed", "unprocessed", "none", "unavailable"}
	case "duplicates":
		return []string{"duplicate", "unique"}
	default:
		return nil
	}
}

func snapshotFacetSelected(value query.Query, dimension string) map[string]bool {
	selected := make(map[string]bool)
	var values []string
	switch dimension {
	case compiledCameraField:
		values = value.Filters.Cameras
	case compiledLensField:
		values = value.Filters.Lenses
	case compiledLocationField:
		values = value.Filters.Locations
	case compiledSetField:
		values = value.Filters.SetIDs
	case snapshotFacetYear:
		if year := wholeCaptureYear(value); year != "" {
			values = []string{year}
		}
	case "collections":
		values = value.Filters.CollectionIDs
	case snapshotFacetTags:
		values = value.Filters.TagIDs
	case snapshotFacetMediaFamily:
		values = value.Filters.MediaFamilies
	case "extension":
		values = value.Filters.Extensions
	case snapshotFacetTextCoverage:
		values = value.Filters.TextCoverage
	case "duplicates":
		if value.Filters.HasDuplicates {
			values = []string{"duplicate"}
		}
	}
	for _, item := range values {
		if dimension == compiledCameraField || dimension == compiledLensField {
			item = cases.Fold().String(item)
		}
		selected[item] = true
	}
	return selected
}

func fillSelectedSnapshotFacetLabels(
	ctx context.Context, q metadataQuerier, dimension string, selected map[string]bool, labels map[string]string,
) error {
	for key := range selected {
		if _, ok := labels[key]; ok {
			continue
		}
		switch dimension {
		case compiledSetField:
			var name string
			if err := q.QueryRowContext(ctx, `SELECT name FROM photo_sets WHERE set_id=?`, key).Scan(&name); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				name = key
			}
			labels[key] = name
		case snapshotFacetTags:
			var name string
			if err := q.QueryRowContext(ctx, `SELECT name FROM tags WHERE id=?`, key).Scan(&name); err != nil {
				return fmt.Errorf("reading selected tag facet label: %w", err)
			}
			labels[key] = name
		case "collections":
			var value sql.NullString
			if err := q.QueryRowContext(ctx, `SELECT label FROM collection_labels WHERE ingest_id=?`, key).Scan(&value); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("reading selected collection facet label: %w", err)
				}
				labels[key] = key
			} else if value.Valid {
				labels[key] = value.String
			} else {
				labels[key] = key
			}
		default:
			labels[key] = key
		}
	}
	return nil
}

func finalizeSnapshotFacetValues(
	counts map[string]int64, labels map[string]string, selected map[string]bool, fixed bool,
) ([]SnapshotFacetValue, int64) {
	observed := make([]SnapshotFacetValue, 0, len(counts))
	for key, count := range counts {
		observed = append(observed, SnapshotFacetValue{Key: key, Label: labels[key], Count: count, Selected: selected[key]})
	}
	slices.SortFunc(observed, func(left, right SnapshotFacetValue) int {
		if result := cmp.Compare(right.Count, left.Count); result != 0 {
			return result
		}
		return strings.Compare(left.Key, right.Key)
	})
	limit := 50
	if fixed || len(observed) < limit {
		limit = len(observed)
	}
	values := slices.Clone(observed[:limit])
	included := make(map[string]bool, len(values))
	for _, value := range values {
		included[value.Key] = true
	}
	var appended []SnapshotFacetValue
	for key := range selected {
		if included[key] {
			continue
		}
		appended = append(appended, SnapshotFacetValue{Key: key, Label: labels[key], Count: counts[key], Selected: true})
		included[key] = true
	}
	slices.SortFunc(appended, func(left, right SnapshotFacetValue) int { return strings.Compare(left.Key, right.Key) })
	values = append(values, appended...)
	var other int64
	for _, value := range observed {
		if !included[value.Key] {
			other += value.Count
		}
	}
	if values == nil {
		values = make([]SnapshotFacetValue, 0)
	}
	return values, other
}

func materializePhotoFacets(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, coverage CoverageSelection, dimensions []string, options snapshotMaterializeOptions) ([]SnapshotFacet, error) {
	results := make(map[string]SnapshotFacet)
	options.MaterializeFacet = func(ctx context.Context, dimension string) (SnapshotFacet, error) {
		if facet, ok := results[dimension]; ok {
			return facet, nil
		}
		filters := queryWithoutSnapshotFacet(compiled.Query, dimension).Filters
		var shared []string
		for _, candidate := range dimensions {
			if (candidate == compiledSetField) == (dimension == compiledSetField) && reflect.DeepEqual(filters, queryWithoutSnapshotFacet(compiled.Query, candidate).Filters) {
				shared = append(shared, candidate)
			}
		}
		facets, err := materializeSharedPhotoFacets(ctx, q, compiled, generation, coverage, shared, options)
		if err != nil {
			return SnapshotFacet{}, err
		}
		for _, facet := range facets {
			results[facet.Dimension] = facet
		}
		return results[dimension], nil
	}
	var used int64
	return materializeSnapshotFacets(ctx, q, compiled, generation, coverage, dimensions, nil, options, &used)
}

func materializeSharedPhotoFacets(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, coverage CoverageSelection, dimensions []string, options snapshotMaterializeOptions) ([]SnapshotFacet, error) {
	value := queryWithoutSnapshotFacet(compiled.Query, dimensions[0])
	resolved, err := (queryCompiler{photoDisplayMetadata: true, photoHidden: compiled.photoHidden, photoOuterDisplay: !value.Filters.CollapseDuplicates}).compile(ctx, value, queryResolver{q: q})
	if err != nil {
		return nil, err
	}
	match, err := photoBrowseMatch(resolved, generation, coverage)
	if err != nil {
		return nil, err
	}
	memberFilters := value.Filters
	memberFilters.Cameras, memberFilters.Lenses, memberFilters.Locations = nil, nil, nil
	memberFilters.ISOMin, memberFilters.ISOMax, memberFilters.GPSBounds = nil, nil, nil
	memberFilters.CaptureAfter, memberFilters.CaptureBefore = "", ""
	if value.Text == "" && reflect.DeepEqual(memberFilters, query.Filters{}) {
		match = resolved.predicate
		if match.sql == "" {
			match = trueCompiledFragment()
		}
	}
	albumJoin, albumValues := "", `'', ''`
	if slices.Contains(dimensions, compiledSetField) {
		albumJoin = ` LEFT JOIN photo_set_members sm ON sm.asset_id=a.asset_id LEFT JOIN photo_sets ps ON ps.set_id=sm.set_id AND ps.deleted_at IS NULL`
		albumValues = `COALESCE(ps.set_id,''),COALESCE(ps.name,'')`
	}
	statement, args, err := bindQueryPopulation(compiledQueryFragment{sql: `SELECT a.asset_id,COALESCE(p.camera_make,''),COALESCE(p.camera_model,''),COALESCE(p.lens_make,''),COALESCE(p.lens_model,''),substr(COALESCE(p.capture_date,''),1,4),COALESCE(p.location_label,''),` + albumValues + ` FROM ` + photoBrowseDisplayFrom + albumJoin + ` WHERE ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(compiled.photoHidden) + ` AND ` + match.sql + ` ORDER BY a.asset_id LIMIT ?`, args: append(match.args, options.FacetMemberLimit+1), relations: match.relations}, coverage, generation)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	observations := make(map[string]*snapshotFacetAccumulator, len(dimensions))
	for _, dimension := range dimensions {
		observations[dimension] = newSnapshotFacetAccumulator()
	}
	var projected, bytes int64
	exceeded := false
	for rows.Next() {
		var id, cameraMake, cameraModel, lensMake, lensModel, year, location, set, name string
		if err := rows.Scan(&id, &cameraMake, &cameraModel, &lensMake, &lensModel, &year, &location, &set, &name); err != nil {
			return nil, err
		}
		projected++
		bytes += int64(len(id) + len(cameraMake) + len(cameraModel) + len(lensMake) + len(lensModel) + len(year) + len(location) + len(set) + len(name))
		if projected > options.FacetMemberLimit || bytes > options.MaxSerializedBytes {
			exceeded = true
			break
		}
		for _, dimension := range dimensions {
			values := []string{}
			switch dimension {
			case compiledCameraField:
				values = []string{cameraMake, cameraModel}
			case compiledLensField:
				values = []string{lensMake, lensModel}
			case snapshotFacetYear:
				values = []string{year}
			case compiledLocationField:
				values = []string{location}
			case compiledSetField:
				values = []string{set}
			}
			have := false
			for _, label := range values {
				if label == "" {
					continue
				}
				have = true
				key := label
				if dimension == compiledCameraField || dimension == compiledLensField {
					key = cases.Fold().String(label)
				}
				if dimension == compiledSetField {
					label = name
				}
				observations[dimension].add(dimension, id, key, label, options.FacetMemberLimit)
			}
			if !have {
				observations[dimension].add(dimension, id, "", "", options.FacetMemberLimit)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]SnapshotFacet, 0, len(dimensions))
	for _, dimension := range dimensions {
		observation := observations[dimension]
		if exceeded || observation.limited {
			reason := "member_budget_exceeded"
			if bytes > options.MaxSerializedBytes {
				reason = "byte_budget_exceeded"
			}
			result = append(result, unavailableSnapshotFacet(dimension, reason))
			continue
		}
		facet, err := finishSnapshotFacet(ctx, q, compiled.Query, dimension, observation.snapshotFacetObservation)
		if err != nil {
			return nil, err
		}
		result = append(result, facet)
	}
	return result, nil
}

type snapshotFacetAccumulator struct {
	snapshotFacetObservation

	previous  string
	seen      map[string]bool
	projected int64
	limited   bool
}

func newSnapshotFacetAccumulator() *snapshotFacetAccumulator {
	return &snapshotFacetAccumulator{counts: map[string]int64{}, labels: map[string]string{}, seen: map[string]bool{}}
}

func (observation *snapshotFacetAccumulator) add(dimension, id, value, label string, limit int64) {
	if observation.limited {
		return
	}
	if id != observation.previous {
		observation.total++
		observation.missing++
		observation.previous = id
		clear(observation.seen)
	}
	if !observation.seen[value] {
		observation.projected++
		if observation.projected > limit {
			observation.limited = true
			return
		}
		if value != "" {
			if len(observation.seen) == 0 || len(observation.seen) == 1 && observation.seen[""] {
				observation.missing--
			}
			observation.counts[value]++
		}
		observation.seen[value] = true
	}
	if value == "" {
		return
	}
	prior, ok := observation.labels[value]
	if ok && label == prior {
		return
	}
	prefer := label < prior
	if dimension == compiledCameraField || dimension == compiledLensField {
		valid, priorValid := query.ValidateTextOperand(dimension, label) == nil, query.ValidateTextOperand(dimension, prior) == nil
		prefer = valid && !priorValid || valid == priorValid && prefer
	}
	if !ok || prefer {
		observation.labels[value] = label
	}
}

func observeSnapshotFacet(rows *sql.Rows, dimension string, limit int64, scan func() (string, string, string, error)) (snapshotFacetObservation, bool, error) {
	defer func() { _ = rows.Close() }()
	observation := newSnapshotFacetAccumulator()
	for rows.Next() {
		id, value, label, err := scan()
		if err != nil {
			return observation.snapshotFacetObservation, false, err
		}
		observation.add(dimension, id, value, label, limit)
		if observation.limited {
			return observation.snapshotFacetObservation, true, rows.Close()
		}
	}
	if err := rows.Err(); err != nil {
		return observation.snapshotFacetObservation, false, err
	}
	return observation.snapshotFacetObservation, false, rows.Close()
}

func wholeCaptureYear(value query.Query) string {
	if len(value.Filters.CaptureAfter) != 10 || value.Filters.CaptureAfter[4:] != "-01-01" {
		return ""
	}
	year, _ := strconv.Atoi(value.Filters.CaptureAfter[:4])
	before := ""
	if year < 9999 {
		before = fmt.Sprintf("%04d-01-01", year+1)
	}
	if value.Filters.CaptureBefore != before {
		return ""
	}
	return value.Filters.CaptureAfter[:4]
}
