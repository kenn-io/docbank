package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"mime"
	"path"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/query"
)

// QualityBucket is a frequency value in a bounded collection census.
type QualityBucket struct {
	Value string
	Count int64
}

// QualityDimension retains the top values and accounts for every other member.
type QualityDimension struct {
	Field          string
	Values         []QualityBucket
	Missing, Other int64
}

// QualitySpike identifies a descriptive concentration in one dimension.
type QualitySpike struct {
	Field, Value string
	Count        int64
}

// CollectionQuality is an aggregate receipt from one bounded source snapshot.
type CollectionQuality struct {
	Collection                                Collection
	SourceFingerprint                         string
	Dimensions                                []QualityDimension
	ZeroBytes, Mismatches, DuplicateDocuments int64
	Spikes                                    []QualitySpike
}

var (
	// ErrQualityTooLarge rejects a census that cannot be returned in full.
	ErrQualityTooLarge = errors.New("collection quality too large")
	// ErrQualityUnavailable reports an interrupted or timed-out census.
	ErrQualityUnavailable = errors.New("collection quality unavailable")
	// ErrInvalidQualityFields rejects unknown or repeated dimensions.
	ErrInvalidQualityFields = errors.New("invalid collection quality fields")
)

const (
	maxCollectionQualityMembers = 250_000
	maxCollectionQualityBytes   = 64 << 20
	collectionQualityTimeout    = 5 * time.Second
)

var collectionQualityFields = []string{"duplicates", "extension", "media_family", "media_type", "modified_month", "size", "text_coverage"}

func normalizeQualityFields(fields []string) ([]string, error) {
	if len(fields) == 0 {
		return slices.Clone(collectionQualityFields), nil
	}
	if len(fields) > len(collectionQualityFields) {
		return nil, ErrInvalidQualityFields
	}
	result := slices.Clone(fields)
	slices.Sort(result)
	for i, field := range result {
		if !slices.Contains(collectionQualityFields, field) || i > 0 && result[i-1] == field {
			return nil, fmt.Errorf("%w: %q", ErrInvalidQualityFields, field)
		}
	}
	return result, nil
}

// CollectionQuality reads the full census before deriving any successful
// histogram. The timeout and both limits apply even when few fields are requested.
func (s *Store) CollectionQuality(ctx context.Context, id string, selection CoverageSelection, fields []string) (result CollectionQuality, retErr error) {
	selection, err := normalizeCoverageSelection([]CoverageSelection{selection})
	if err != nil {
		return CollectionQuality{}, err
	}
	fields, err = normalizeQualityFields(fields)
	if err != nil {
		return CollectionQuality{}, err
	}
	if err := validateUUIDv4(id); err != nil {
		return CollectionQuality{}, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, collectionQualityTimeout)
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			result = CollectionQuality{}
			retErr = errors.Join(ErrQualityUnavailable, ctx.Err(), retErr)
		}
	}()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CollectionQuality{}, err
	}
	defer func() { _ = tx.Rollback() }()
	census, err := collectionQualityCensusTx(ctx, tx, id, selection)
	if err != nil {
		return CollectionQuality{}, err
	}
	result, err = aggregateCollectionQuality(ctx, census, fields)
	if err != nil {
		return CollectionQuality{}, err
	}
	if err := tx.Commit(); err != nil {
		return CollectionQuality{}, err
	}
	return result, nil
}

// The census/aggregation boundary allows a cache to compare fresh source
// fingerprints before reusing an immutable receipt. Census rows never escape
// the store API and contain no document text or provider error payloads.
type collectionQualityCensus struct {
	collection  Collection
	fingerprint string
	rows        []collectionQualityRow
}

type collectionQualityRow struct {
	NodeID                                                                int64
	VersionID, BlobHash, Name, MediaType, ModifiedAt                      string
	Size                                                                  int64
	State, AttachmentID, PublishedAt, BuildID, Completeness               string
	PartialSuccess, Truncated, LexicalSegmentCount, Verified, Serving     int64
	WaiterID, JobID, WaiterState, JobState, WaiterUpdatedAt, JobUpdatedAt string
	DuplicateCount                                                        int64
}

const qualityProjectionCTE = `quality_projection AS (
 SELECT p.*,n.name,COALESCE(v.mime_type,'') media_type,n.modified_at,v.size,
  COALESCE(d.references_count,1) duplicate_count
 FROM processing_coverage p JOIN nodes n ON n.id=p.node_id
 JOIN content_versions v ON v.version_id=p.version_id
 LEFT JOIN duplicate_counts d ON d.blob_hash=p.blob_hash
)`

// Projection size includes fixed-width numeric storage and every selected
// string's UTF-8 bytes. Check before scanning strings into application memory.
const qualityProjectionBytes = `128+length(CAST(version_id AS BLOB))+length(CAST(blob_hash AS BLOB))+
 length(CAST(name AS BLOB))+length(CAST(media_type AS BLOB))+length(CAST(modified_at AS BLOB))+
 length(CAST(state AS BLOB))+length(CAST(attachment_id AS BLOB))+length(CAST(published_at AS BLOB))+
 length(CAST(build_id AS BLOB))+length(CAST(completeness AS BLOB))+length(CAST(waiter_id AS BLOB))+
 length(CAST(job_id AS BLOB))+length(CAST(waiter_state AS BLOB))+length(CAST(job_state AS BLOB))+
 length(CAST(waiter_updated_at AS BLOB))+length(CAST(job_updated_at AS BLOB))`

func collectionQualityCensusTx(ctx context.Context, q metadataQuerier, id string, selection CoverageSelection) (collectionQualityCensus, error) {
	collection, err := collectionSummaryByID(ctx, q, id)
	if err != nil {
		return collectionQualityCensus{}, err
	}
	if err := validateCollectionQualityBounds(collection.FileCount, 0); err != nil {
		return collectionQualityCensus{}, err
	}
	generation, err := collectionGenerationTx(ctx, q)
	if err != nil {
		return collectionQualityCensus{}, err
	}
	coverage, err := collectionCoverageTx(ctx, q, []string{id}, selection, generation)
	if err != nil {
		return collectionQualityCensus{}, err
	}
	collection.Coverage = coverage[id]
	cte := `WITH ` + CollectionMembershipCTE + `,` + CurrentContentMembershipCTE + `,
 coverage_members AS (SELECT node_id FROM collection_members WHERE ingest_id=?),
 ` + processingCoverageCTE() + `,
 duplicate_counts AS (SELECT blob_hash,COUNT(*) references_count FROM current_content_members GROUP BY blob_hash),
 ` + qualityProjectionCTE
	args := []any{id, selection.ProfileFingerprint, generation}
	var projectedBytes int64
	if err := q.QueryRowContext(ctx, cte+` SELECT COALESCE(SUM(`+qualityProjectionBytes+`),0) FROM quality_projection`, args...).Scan(&projectedBytes); err != nil {
		return collectionQualityCensus{}, err
	}
	if err := validateCollectionQualityBounds(collection.FileCount, projectedBytes); err != nil {
		return collectionQualityCensus{}, err
	}
	rows, err := q.QueryContext(ctx, cte+` SELECT node_id,version_id,blob_hash,name,media_type,modified_at,size,
 state,attachment_id,published_at,build_id,completeness,partial_success,truncated,lexical_segment_count,verified,serving,
 waiter_id,job_id,waiter_state,job_state,waiter_updated_at,job_updated_at,duplicate_count
 FROM quality_projection ORDER BY node_id`, args...)
	if err != nil {
		return collectionQualityCensus{}, err
	}
	defer func() { _ = rows.Close() }()
	census := collectionQualityCensus{collection: collection, rows: make([]collectionQualityRow, 0, int(collection.FileCount))}
	hash := sha256.New()
	if err := json.MarshalWrite(hash, collection, json.Deterministic(true)); err != nil {
		return collectionQualityCensus{}, err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return collectionQualityCensus{}, err
		}
		var row collectionQualityRow
		if err := rows.Scan(&row.NodeID, &row.VersionID, &row.BlobHash, &row.Name, &row.MediaType, &row.ModifiedAt, &row.Size,
			&row.State, &row.AttachmentID, &row.PublishedAt, &row.BuildID, &row.Completeness, &row.PartialSuccess, &row.Truncated, &row.LexicalSegmentCount, &row.Verified, &row.Serving,
			&row.WaiterID, &row.JobID, &row.WaiterState, &row.JobState, &row.WaiterUpdatedAt, &row.JobUpdatedAt, &row.DuplicateCount); err != nil {
			return collectionQualityCensus{}, err
		}
		if err := json.MarshalWrite(hash, row, json.Deterministic(true)); err != nil {
			return collectionQualityCensus{}, err
		}
		census.rows = append(census.rows, row)
	}
	if err := rows.Err(); err != nil {
		return collectionQualityCensus{}, err
	}
	if err := rows.Close(); err != nil {
		return collectionQualityCensus{}, err
	}
	if int64(len(census.rows)) != collection.FileCount {
		return collectionQualityCensus{}, fmt.Errorf("%w: incomplete current-member census", ErrQualityUnavailable)
	}
	census.fingerprint = hex.EncodeToString(hash.Sum(nil))
	return census, nil
}

func validateCollectionQualityBounds(members, bytes int64) error {
	if members > maxCollectionQualityMembers || bytes > maxCollectionQualityBytes {
		return ErrQualityTooLarge
	}
	return nil
}

func aggregateCollectionQuality(ctx context.Context, census collectionQualityCensus, fields []string) (CollectionQuality, error) {
	result := CollectionQuality{Collection: census.collection, SourceFingerprint: census.fingerprint,
		Dimensions: make([]QualityDimension, 0, len(fields)), Spikes: make([]QualitySpike, 0)}
	counts := make([]map[string]int64, len(fields))
	missing := make([]int64, len(fields))
	for i := range counts {
		counts[i] = make(map[string]int64)
	}
	for _, row := range census.rows {
		if err := ctx.Err(); err != nil {
			return CollectionQuality{}, err
		}
		if row.Size == 0 {
			result.ZeroBytes++
		}
		mimeFamily := query.ClassifyMedia(row.MediaType, "")
		extFamily := query.ClassifyMedia("", row.Name)
		if mimeFamily != "unknown" && extFamily != "unknown" && mimeFamily != extFamily {
			result.Mismatches++
		}
		if row.DuplicateCount > 1 {
			result.DuplicateDocuments++
		}
		for i, field := range fields {
			value := qualityValue(row, field, census.collection.Coverage.Configuration == "configured")
			if value == "" {
				missing[i]++
			} else {
				counts[i][value]++
			}
		}
	}
	for i, field := range fields {
		dimension := QualityDimension{Field: field, Values: make([]QualityBucket, 0, len(counts[i])), Missing: missing[i]}
		for value, count := range counts[i] {
			dimension.Values = append(dimension.Values, QualityBucket{value, count})
		}
		slices.SortFunc(dimension.Values, func(a, b QualityBucket) int {
			if a.Count > b.Count {
				return -1
			}
			if a.Count < b.Count {
				return 1
			}
			return strings.Compare(a.Value, b.Value)
		})
		for _, bucket := range dimension.Values {
			if bucket.Count >= 10 && bucket.Count*5 >= census.collection.FileCount*4 {
				result.Spikes = append(result.Spikes, QualitySpike{field, bucket.Value, bucket.Count})
			}
		}
		if len(dimension.Values) > 50 {
			for _, bucket := range dimension.Values[50:] {
				dimension.Other += bucket.Count
			}
			dimension.Values = dimension.Values[:50]
		}
		result.Dimensions = append(result.Dimensions, dimension)
	}
	return result, nil
}

func qualityValue(row collectionQualityRow, field string, configured bool) string {
	switch field {
	case "extension":
		return strings.ToLower(strings.TrimPrefix(path.Ext(row.Name), "."))
	case "media_type":
		mediaType, _, err := mime.ParseMediaType(row.MediaType)
		if err != nil {
			return ""
		}
		return strings.ToLower(mediaType)
	case "media_family":
		family := query.ClassifyMedia(row.MediaType, row.Name)
		if family == "unknown" {
			return ""
		}
		return family
	case "modified_month":
		modified, err := time.Parse(time.RFC3339Nano, row.ModifiedAt)
		if err != nil {
			return ""
		}
		return modified.UTC().Format("2006-01")
	case "size":
		return collectionQualitySize(row.Size)
	case "text_coverage":
		if !configured {
			return ""
		}
		return row.State
	case "duplicates":
		if row.DuplicateCount > 1 {
			return "duplicate"
		}
		return "unique"
	default:
		return ""
	}
}

func collectionQualitySize(size int64) string {
	switch {
	case size == 0:
		return "zero"
	case size < 1<<10:
		return "(0,1KiB)"
	case size < 1<<20:
		return "[1KiB,1MiB)"
	case size < 10<<20:
		return "[1MiB,10MiB)"
	case size < 100<<20:
		return "[10MiB,100MiB)"
	case size < 1<<30:
		return "[100MiB,1GiB)"
	default:
		return ">=1GiB"
	}
}
