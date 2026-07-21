// Package maintenance contains storage lifecycle operations shared by the
// embedded Vault and daemon HTTP adapters.
package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"time"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

const (
	// DefaultMaxObjects is the finite object count used by an embedded
	// maintenance call whose object budget is zero.
	DefaultMaxObjects = 100

	defaultRepackMinAge    = 24 * time.Hour
	defaultRepackDeadBytes = int64(8 << 20)
)

var (
	ErrInvalidCursor = errors.New("invalid maintenance cursor")
	ErrInvalidBudget = errors.New("invalid maintenance budget")
)

type Budget struct {
	MaxObjects int
	MaxBytes   int64
	Cursor     string
}

type Progress struct {
	NextCursor string
	More       bool
}

type GCOptions struct {
	Budget Budget
	DryRun bool
}

type GCReport struct {
	Progress

	CandidateBlobs     int
	UntrackedFiles     int
	ReclaimableBytes   int64
	PendingPackedBlobs int
	PendingPackedBytes int64
	ReclaimedFiles     int
	RemovedBlobs       int
	Removed            int
	DryRun             bool
}

type VerifyOptions struct{ Budget Budget }

type VerifyProblem struct {
	Hash    string
	Problem string
}

type VerifyReport struct {
	Progress

	OK               int
	Problems         []VerifyProblem
	MetadataProblems []string
}

type RepackOptions struct {
	Budget       Budget
	MinAge       time.Duration
	MinDeadBytes int64
}

type RepackReport struct {
	Progress

	MappingsPruned         int64
	PacksSelected          int
	PacksRewritten         int
	PacksSealed            int
	PacksRemoved           int
	PacksDeferredOversized int
	BlobsRepacked          int
	BytesRepacked          int64
	BudgetExhausted        bool
}

type PackReport struct {
	Stats packstore.PackStats
	More  bool
}

type operation string

const (
	operationGC     operation = "gc"
	operationVerify operation = "verify"
	operationRepack operation = "repack"
)

type cursor struct {
	Version int       `json:"v"`
	Kind    operation `json:"op"`
	Hash    string    `json:"hash"`
}

func normalizeBudget(budget Budget) (Budget, error) {
	if budget.MaxObjects < 0 || budget.MaxBytes < 0 {
		return Budget{}, ErrInvalidBudget
	}
	if budget.MaxObjects == 0 {
		budget.MaxObjects = DefaultMaxObjects
	}
	return budget, nil
}

func decodeCursor(raw string, kind operation) (string, error) {
	if raw == "" {
		return "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("%w: malformed encoding", ErrInvalidCursor)
	}
	var decoded cursor
	if err := json.Unmarshal(data, &decoded); err != nil {
		return "", fmt.Errorf("%w: malformed value", ErrInvalidCursor)
	}
	parsed, err := packstore.ParseHash(decoded.Hash)
	if err != nil || parsed.String() != decoded.Hash || decoded.Version != 1 || decoded.Kind != kind {
		return "", fmt.Errorf("%w: invalid or mismatched fields", ErrInvalidCursor)
	}
	return decoded.Hash, nil
}

func encodeCursor(kind operation, hash string) string {
	data := []byte(`{"v":1,"op":"` + string(kind) + `","hash":"` + hash + `"}`)
	return base64.RawURLEncoding.EncodeToString(data)
}

type gcCandidate struct {
	hash      string
	tracked   bool
	loose     bool
	recorded  bool
	looseSize int64
}

// GarbageCollect processes one bounded canonical-hash page of unreachable
// catalog rows and untracked canonical loose files.
func GarbageCollect(
	ctx context.Context, metadata *store.Store, blobs *blob.Store, opts GCOptions,
) (GCReport, error) {
	budget, err := normalizeBudget(opts.Budget)
	if err != nil {
		return GCReport{}, err
	}
	after, err := decodeCursor(budget.Cursor, operationGC)
	if err != nil {
		return GCReport{}, err
	}
	tracked, trackedMore, err := metadata.UnreachableBlobsPage(ctx, after, budget.MaxObjects)
	if err != nil {
		return GCReport{}, err
	}
	loose, looseMore, err := blobs.ListPage(after, budget.MaxObjects)
	if err != nil {
		return GCReport{}, err
	}
	candidates := make(map[string]gcCandidate, len(tracked))
	for _, candidate := range tracked {
		candidates[candidate.Hash] = gcCandidate{
			hash: candidate.Hash, tracked: true, recorded: true,
		}
	}
	for _, looseObject := range loose {
		candidate := candidates[looseObject.Hash]
		candidate.hash = looseObject.Hash
		candidate.loose = true
		candidate.looseSize = looseObject.Size
		if !candidate.tracked {
			candidate.recorded, err = metadata.HasBlob(ctx, looseObject.Hash)
		}
		if err != nil {
			return GCReport{}, err
		}
		candidates[looseObject.Hash] = candidate
	}
	ordered := make([]gcCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		ordered = append(ordered, candidate)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].hash < ordered[j].hash })

	report := GCReport{DryRun: opts.DryRun}
	trackedHashes := make([]string, 0, budget.MaxObjects)
	processedBytes := int64(0)
	processed := 0
	for _, candidate := range ordered {
		if processed == budget.MaxObjects ||
			(processed > 0 && budget.MaxBytes > 0 && processedBytes >= budget.MaxBytes) {
			break
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if !candidate.tracked && candidate.recorded {
			processed++
			continue
		}
		packedSize, packed, err := metadata.PackedBlobStoredByte(ctx, candidate.hash)
		if err != nil {
			return report, err
		}
		if candidate.tracked {
			report.CandidateBlobs++
			trackedHashes = append(trackedHashes, candidate.hash)
			if packed {
				report.PendingPackedBlobs++
				report.PendingPackedBytes += packedSize
			}
		} else {
			report.UntrackedFiles++
		}
		report.ReclaimableBytes += candidate.looseSize
		processedBytes += candidate.looseSize + packedSize
		if !opts.DryRun && candidate.loose {
			if err := blobs.Remove(candidate.hash); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return report, err
			}
			report.ReclaimedFiles++
		}
		if !opts.DryRun && !candidate.tracked {
			report.Removed++
		}
		processed++
	}
	if !opts.DryRun && len(trackedHashes) > 0 {
		if err := metadata.DeleteBlobRows(ctx, trackedHashes); err != nil {
			return report, err
		}
		report.RemovedBlobs = len(trackedHashes)
		report.Removed += len(trackedHashes)
	}
	report.More = processed < len(ordered) || trackedMore || looseMore
	if report.More && processed > 0 {
		report.NextCursor = encodeCursor(operationGC, ordered[processed-1].hash)
	}
	return report, nil
}

// Verify validates metadata once at the start of a cycle and re-hashes one
// bounded canonical-hash page of catalog-authorized content.
func Verify(
	ctx context.Context, metadata *store.Store, blobs *blob.Store, opts VerifyOptions,
) (VerifyReport, error) {
	budget, err := normalizeBudget(opts.Budget)
	if err != nil {
		return VerifyReport{}, err
	}
	after, err := decodeCursor(budget.Cursor, operationVerify)
	if err != nil {
		return VerifyReport{}, err
	}
	report := VerifyReport{}
	if after == "" {
		if err := metadata.ValidateMetadata(ctx); err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			report.MetadataProblems = append(report.MetadataProblems, err.Error())
		}
	}
	hashes, pageMore, err := metadata.BlobHashesPage(ctx, after, budget.MaxObjects)
	if err != nil {
		return report, err
	}
	processedBytes := int64(0)
	processed := 0
	for _, hash := range hashes {
		if processed > 0 && budget.MaxBytes > 0 && processedBytes >= budget.MaxBytes {
			break
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		problem, bytesRead := checkBlob(ctx, blobs, hash)
		processedBytes += bytesRead
		if problem == "" {
			report.OK++
		} else {
			report.Problems = append(report.Problems, VerifyProblem{Hash: hash, Problem: problem})
		}
		processed++
	}
	report.More = processed < len(hashes) || pageMore
	if report.More && processed > 0 {
		report.NextCursor = encodeCursor(operationVerify, hashes[processed-1])
	}
	return report, nil
}

func checkBlob(ctx context.Context, blobs *blob.Store, hash string) (string, int64) {
	reader, _, err := blobs.OpenStreamContext(ctx, hash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing", 0
		}
		return "unreadable", 0
	}
	defer func() { _ = reader.Close() }()
	digest := sha256.New()
	read, err := io.Copy(digest, reader)
	if err != nil {
		if isContentCorruption(err) {
			return "corrupt", read
		}
		return "unreadable", read
	}
	if hex.EncodeToString(digest.Sum(nil)) != hash {
		return "corrupt", read
	}
	return "", read
}

func isContentCorruption(err error) bool {
	return errors.Is(err, packstore.ErrContentMismatch) ||
		errors.Is(err, pack.ErrTruncated) ||
		errors.Is(err, pack.ErrChecksum) ||
		errors.Is(err, pack.ErrCorrupt) ||
		errors.Is(err, pack.ErrBlobMismatch)
}

// Pack preserves Kit's existing raw-byte policy and derives remaining work
// from indexed loose authority rather than a filesystem scan.
func Pack(
	ctx context.Context, metadata *store.Store, blobs *blob.Store, maxBytes int64,
) (PackReport, error) {
	stats, err := blobs.Maintainer().Pack(ctx, packstore.PackOptions{MaxBytes: maxBytes})
	if err != nil {
		return PackReport{Stats: stats}, fmt.Errorf("packing blobs: %w", err)
	}
	backlog, err := metadata.LooseBacklog(ctx)
	if err != nil {
		return PackReport{Stats: stats}, err
	}
	return PackReport{Stats: stats, More: backlog.EligibleObjects > 0}, nil
}

// Repack retires a bounded dead-pack batch, then rewrites bounded sparse packs
// one at a time in canonical-live-hash order. Kit still performs every physical
// rewrite and enforces the existing soft raw-byte budget.
func Repack(
	ctx context.Context, metadata *store.Store, blobs *blob.Store, opts RepackOptions,
) (RepackReport, error) {
	budget, err := normalizeBudget(opts.Budget)
	if err != nil {
		return RepackReport{}, err
	}
	if opts.MinAge < 0 || opts.MinDeadBytes < 0 {
		return RepackReport{}, ErrInvalidBudget
	}
	after, err := decodeCursor(budget.Cursor, operationRepack)
	if err != nil {
		return RepackReport{}, err
	}
	minAge := opts.MinAge
	if minAge == 0 {
		minAge = defaultRepackMinAge
	}
	minDead := opts.MinDeadBytes
	if minDead == 0 {
		minDead = defaultRepackDeadBytes
	}
	now := time.Now().UTC()
	report := RepackReport{}
	remaining := budget.MaxObjects

	dead, deadMore, err := metadata.DeadPackUsagePage(ctx, remaining)
	if err != nil {
		return report, err
	}
	if len(dead) > 0 {
		stats, runErr := blobs.RepackWithCatalog(ctx,
			&scopedCatalog{Catalog: store.NewPackCatalog(metadata), usages: dead},
			packstore.RepackOptions{Now: now, Selection: packstore.RepackSelection{
				MinAge: minAge, MinDeadStored: minDead,
			}})
		addRepackStats(&report, stats)
		remaining -= len(dead)
		if runErr != nil {
			report.More, err = repackWorkRemains(ctx, metadata, after, now, minAge, minDead)
			if err != nil {
				return report, errors.Join(runErr, err)
			}
			if report.More && after != "" {
				report.NextCursor = encodeCursor(operationRepack, after)
			}
			return report, runErr
		}
	}
	if deadMore {
		report.More = true
		if after != "" {
			report.NextCursor = encodeCursor(operationRepack, after)
		}
		return report, nil
	}
	if remaining == 0 {
		report.More, err = repackWorkRemains(ctx, metadata, after, now, minAge, minDead)
		if err != nil {
			return report, err
		}
		if report.More && after != "" {
			report.NextCursor = encodeCursor(operationRepack, after)
		}
		return report, nil
	}

	candidates, candidateMore, err := metadata.SparseRepackPage(
		ctx, after, remaining, now, minAge, minDead)
	if err != nil {
		return report, err
	}
	last := after
	processed := 0
	cursorBlocked := false
	var runErr error
	for _, candidate := range candidates {
		if processed > 0 && budget.MaxBytes > 0 && report.BytesRepacked >= budget.MaxBytes {
			report.BudgetExhausted = true
			break
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		stats, sourceErr := blobs.RepackWithCatalog(ctx,
			&scopedCatalog{Catalog: store.NewPackCatalog(metadata), usages: []packstore.PackUsage{candidate.Usage}},
			packstore.RepackOptions{MaxBytes: budget.MaxBytes, Now: now,
				Selection: packstore.RepackSelection{MinAge: minAge, MinDeadStored: minDead}})
		addRepackStats(&report, stats)
		if sourceErr != nil {
			runErr = errors.Join(runErr, sourceErr)
			cursorBlocked = true
			processed++
			if budget.MaxBytes == 0 {
				report.More = true
				if last != "" {
					report.NextCursor = encodeCursor(operationRepack, last)
				}
				return report, runErr
			}
			continue
		}
		if !cursorBlocked {
			last = candidate.Hash
		}
		processed++
	}
	if runErr != nil {
		report.More, err = repackWorkRemains(ctx, metadata, last, now, minAge, minDead)
		if err != nil {
			return report, errors.Join(runErr, err)
		}
		if report.More && last != "" {
			report.NextCursor = encodeCursor(operationRepack, last)
		}
		return report, runErr
	}
	report.More = processed < len(candidates) || candidateMore
	if report.More && last != "" {
		report.NextCursor = encodeCursor(operationRepack, last)
	}
	return report, nil
}

func repackWorkRemains(
	ctx context.Context,
	metadata *store.Store,
	after string,
	now time.Time,
	minAge time.Duration,
	minDead int64,
) (bool, error) {
	dead, _, err := metadata.DeadPackUsagePage(ctx, 1)
	if err != nil || len(dead) > 0 {
		return len(dead) > 0, err
	}
	sparse, _, err := metadata.SparseRepackPage(ctx, after, 1, now, minAge, minDead)
	return len(sparse) > 0, err
}

type scopedCatalog struct {
	packstore.Catalog

	usages []packstore.PackUsage
}

func (catalog *scopedCatalog) ListPackUsage(context.Context) ([]packstore.PackUsage, error) {
	return append([]packstore.PackUsage(nil), catalog.usages...), nil
}

func addRepackStats(report *RepackReport, stats packstore.RepackStats) {
	report.MappingsPruned += stats.MappingsPruned
	report.PacksSelected += stats.PacksSelected
	report.PacksRewritten += stats.PacksRewritten
	report.PacksSealed += stats.PacksSealed
	report.PacksRemoved += stats.PacksRemoved
	report.PacksDeferredOversized += stats.PacksDeferredOversized
	report.BlobsRepacked += stats.BlobsRepacked
	report.BytesRepacked += stats.BytesRepacked
	report.BudgetExhausted = report.BudgetExhausted || stats.BudgetExhausted
}
