package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/query"
)

var snapshotFacetDimensions = [...]string{
	"collections", "tags", snapshotFacetMediaFamily, "extension", "modified", "size", snapshotFacetTextCoverage, "duplicates",
}

const (
	snapshotFacetMediaFamily  = "media_family"
	snapshotFacetTextCoverage = "text_coverage"
)

func normalizeSnapshotFacets(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		known := false
		for _, dimension := range snapshotFacetDimensions {
			if value == dimension {
				known = true
				break
			}
		}
		if !known {
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
	coverage CoverageSelection, dimensions []string, options snapshotMaterializeOptions, used *int64,
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
		facet, err := materializeSnapshotFacet(facetCtx, q, compiled, generationID, coverage, dimension, options.FacetMemberLimit)
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
	coverage CoverageSelection, dimension string, memberLimit int64,
) (SnapshotFacet, error) {
	population, err := matchedPopulation(compiled, generationID, dimension)
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
	case "tags":
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
	observation := snapshotFacetObservation{counts: make(map[string]int64), labels: make(map[string]string)}
	var priorNodeID int64
	havePrior := false
	var projectedRows int64
	for rows.Next() {
		var nodeID, size int64
		var name, mimeType, modifiedAt, value, valueLabel string
		if err := rows.Scan(&nodeID, &name, &mimeType, &size, &modifiedAt, &value, &valueLabel); err != nil {
			return SnapshotFacet{}, fmt.Errorf("scanning %s facet: %w", dimension, err)
		}
		projectedRows++
		if projectedRows > memberLimit {
			return unavailableSnapshotFacet(dimension, "member_budget_exceeded"), rows.Close()
		}
		if !havePrior || nodeID != priorNodeID {
			observation.total++
			if observation.total > memberLimit {
				return unavailableSnapshotFacet(dimension, "member_budget_exceeded"), rows.Close()
			}
			priorNodeID, havePrior = nodeID, true
		}
		if dimension != "collections" && dimension != "tags" && dimension != snapshotFacetTextCoverage && dimension != "duplicates" {
			value = snapshotFacetScalarValue(dimension, name, mimeType, modifiedAt, size)
			valueLabel = value
		}
		if value == "" {
			observation.missing++
			continue
		}
		observation.counts[value]++
		observation.labels[value] = valueLabel
	}
	if err := rows.Err(); err != nil {
		return SnapshotFacet{}, fmt.Errorf("materializing %s facet: %w", dimension, err)
	}
	if err := rows.Close(); err != nil {
		return SnapshotFacet{}, fmt.Errorf("closing %s facet: %w", dimension, err)
	}
	selected := snapshotFacetSelected(compiled.Query, dimension)
	if err := fillSelectedSnapshotFacetLabels(ctx, q, dimension, selected, observation.labels); err != nil {
		return SnapshotFacet{}, err
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
	total, missing := observation.total, observation.missing
	return SnapshotFacet{
		Dimension: dimension, Available: true, Total: &total, Values: values,
		Missing: &missing, Other: &other,
	}, nil
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
	case "collections":
		values = value.Filters.CollectionIDs
	case "tags":
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
		case "tags":
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
