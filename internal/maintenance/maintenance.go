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
	"os"
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
	repackScanMultiplier   = 8
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
	// Catalog overrides the catalog used by scoped Kit rewrites. It supports
	// focused fault injection without changing the public embedded API.
	Catalog packstore.Catalog
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
	Phase   string    `json:"phase,omitempty"`
	Hash    string    `json:"hash"`
	PackID  string    `json:"pack_id,omitempty"`
	Set     bool      `json:"set"`
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

func decodeCursor(raw string, kind operation) (cursor, error) {
	if raw == "" {
		return cursor{Version: 1, Kind: kind}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: malformed encoding", ErrInvalidCursor)
	}
	var decoded cursor
	if err := json.Unmarshal(data, &decoded); err != nil {
		return cursor{}, fmt.Errorf("%w: malformed value", ErrInvalidCursor)
	}
	if decoded.Version != 1 || decoded.Kind != kind || !decoded.Set {
		return cursor{}, fmt.Errorf("%w: invalid or mismatched fields", ErrInvalidCursor)
	}
	if kind != operationRepack {
		if decoded.Phase != "" || decoded.PackID != "" {
			return cursor{}, fmt.Errorf("%w: invalid or mismatched fields", ErrInvalidCursor)
		}
		return decoded, nil
	}
	switch decoded.Phase {
	case "mappings":
		if decoded.PackID != "" {
			return cursor{}, fmt.Errorf("%w: invalid or mismatched fields", ErrInvalidCursor)
		}
	case "dead":
		if decoded.Hash != "" || decoded.PackID != "" {
			return cursor{}, fmt.Errorf("%w: invalid or mismatched fields", ErrInvalidCursor)
		}
	case "sparse":
		parsed, err := packstore.ParseHash(decoded.Hash)
		if err != nil || parsed.String() != decoded.Hash || !pack.IsValidPackID(decoded.PackID) {
			return cursor{}, fmt.Errorf("%w: invalid or mismatched fields", ErrInvalidCursor)
		}
	default:
		return cursor{}, fmt.Errorf("%w: invalid or mismatched fields", ErrInvalidCursor)
	}
	return decoded, nil
}

func encodeCursor(kind operation, hash string) string {
	return encodePhaseCursor(kind, "", hash)
}

func encodePhaseCursor(kind operation, phase, hash string) string {
	data, err := json.Marshal(cursor{Version: 1, Kind: kind, Phase: phase, Hash: hash, Set: true})
	if err != nil {
		panic("maintenance cursor fields are not JSON encodable")
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func encodeSparseCursor(hash, packID string) string {
	data, err := json.Marshal(cursor{
		Version: 1, Kind: operationRepack, Phase: "sparse",
		Hash: hash, PackID: packID, Set: true,
	})
	if err != nil {
		panic("maintenance cursor fields are not JSON encodable")
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// GarbageCollect processes one bounded canonical-hash page of unreachable
// catalog rows. Physical orphan enumeration remains daemon-only because a
// filesystem directory offers no bounded canonical keyset primitive.
func GarbageCollect(
	ctx context.Context, metadata *store.Store, blobs *blob.Store, opts GCOptions,
) (GCReport, error) {
	budget, err := normalizeBudget(opts.Budget)
	if err != nil {
		return GCReport{}, err
	}
	state, err := decodeCursor(budget.Cursor, operationGC)
	if err != nil {
		return GCReport{}, err
	}
	scan, err := metadata.UnreachableBlobsPageFrom(
		ctx, cursorPosition(state), budget.MaxObjects)
	if err != nil {
		return GCReport{}, err
	}
	report := GCReport{DryRun: opts.DryRun}
	tracked := scan.Items
	trackedHashes := make([]string, 0, budget.MaxObjects)
	processedBytes := int64(0)
	processed := 0
	for _, candidate := range tracked {
		if processed == budget.MaxObjects ||
			(processed > 0 && budget.MaxBytes > 0 && processedBytes >= budget.MaxBytes) {
			break
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		packedSize, packed, err := metadata.PackedBlobStoredByte(ctx, candidate.Hash)
		if err != nil {
			return report, err
		}
		report.CandidateBlobs++
		trackedHashes = append(trackedHashes, candidate.Hash)
		if packed {
			report.PendingPackedBlobs++
			report.PendingPackedBytes += packedSize
		}
		report.ReclaimableBytes += candidate.LooseStoredSize
		processedBytes += candidate.LooseStoredSize + packedSize
		if !opts.DryRun && candidate.Loose {
			removed, err := blobs.RemoveIfExists(candidate.Hash)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return report, err
			}
			report.ReclaimedFiles += removed
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
	report.More = processed < len(tracked) || scan.More
	if report.More {
		resumeHash := scan.HighWater
		if processed < len(tracked) {
			resumeHash = tracked[processed-1].Hash
		}
		report.NextCursor = encodeCursor(operationGC, resumeHash)
	}
	return report, nil
}

// Verify re-hashes one bounded canonical-hash page of catalog-authorized
// content. Whole-catalog metadata verification remains daemon-only.
func Verify(
	ctx context.Context, metadata *store.Store, blobs *blob.Store, opts VerifyOptions,
) (VerifyReport, error) {
	budget, err := normalizeBudget(opts.Budget)
	if err != nil {
		return VerifyReport{}, err
	}
	state, err := decodeCursor(budget.Cursor, operationVerify)
	if err != nil {
		return VerifyReport{}, err
	}
	report := VerifyReport{}
	hashes, pageMore, err := metadata.BlobHashesPageFrom(
		ctx, cursorPosition(state), budget.MaxObjects)
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

func cursorPosition(state cursor) *string {
	if !state.Set {
		return nil
	}
	return &state.Hash
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
	state, err := decodeCursor(budget.Cursor, operationRepack)
	if err != nil {
		return RepackReport{}, err
	}
	phase := state.Phase
	if phase == "" {
		phase = "mappings"
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
	sparseAfterHash := ""
	sparseAfterPack := ""
	if phase == "sparse" {
		sparseAfterHash = state.Hash
		sparseAfterPack = state.PackID
	}
	baseCatalog := opts.Catalog
	if baseCatalog == nil {
		baseCatalog = store.NewPackCatalog(metadata)
	}

	if phase == "mappings" {
		mappingPage, pageErr := metadata.UnreferencedPackMappingsPage(
			ctx, cursorPosition(state), remaining)
		if pageErr != nil {
			return report, pageErr
		}
		mappings := mappingPage.Items
		if len(mappings) > 0 {
			removed, deleteErr := metadata.DeleteUnreferencedPackMappings(ctx, mappings)
			if deleteErr != nil {
				return report, deleteErr
			}
			report.MappingsPruned += removed
		}
		remaining -= mappingPage.Examined
		if mappingPage.Examined > 0 {
			state.Hash = mappingPage.HighWater
			state.Set = true
		}
		if mappingPage.More || remaining == 0 {
			report.More = mappingPage.More
			if !report.More {
				report.More, err = repackWorkRemains(
					ctx, metadata, cursorPosition(state), "", "", now, minAge, minDead, true, true)
				if err != nil {
					return report, err
				}
			}
			if report.More && state.Set {
				report.NextCursor = encodePhaseCursor(operationRepack, "mappings", state.Hash)
			}
			return report, nil
		}
		phase = "dead"
	}

	if phase != "sparse" {
		dead, deadMore, err := metadata.DeadPackUsagePage(ctx, remaining)
		if err != nil {
			return report, err
		}
		if len(dead) > 0 {
			stats, runErr := blobs.RepackWithCatalog(ctx,
				&scopedCatalog{Catalog: baseCatalog, usages: dead},
				packstore.RepackOptions{Now: now, Selection: packstore.RepackSelection{
					MinAge: minAge, MinDeadStored: minDead,
				}})
			addRepackStats(&report, stats)
			remaining -= len(dead)
			if runErr != nil {
				report.More, err = repackWorkRemains(ctx, metadata, nil,
					sparseAfterHash, sparseAfterPack, now, minAge, minDead, false, true)
				if err != nil {
					return report, errors.Join(runErr, err)
				}
				if report.More {
					report.NextCursor = repackPhaseCursor(phase, sparseAfterHash, sparseAfterPack)
				}
				return report, runErr
			}
		}
		if deadMore {
			report.More = true
			report.NextCursor = repackPhaseCursor(phase, sparseAfterHash, sparseAfterPack)
			return report, nil
		}
		if remaining == 0 {
			report.More, err = repackWorkRemains(ctx, metadata, nil,
				sparseAfterHash, sparseAfterPack, now, minAge, minDead, false, true)
			if err != nil {
				return report, err
			}
			if report.More {
				report.NextCursor = repackPhaseCursor(phase, sparseAfterHash, sparseAfterPack)
			}
			return report, nil
		}
	}
	// Candidate thresholds can leave long runs of ineligible summaries. Scan a
	// deterministic multiple of the caller's remaining object budget, while
	// still allowing at most remaining eligible physical mutations.
	scanLimit := remaining * repackScanMultiplier
	if scanLimit/remaining != repackScanMultiplier {
		scanLimit = int(^uint(0) >> 1)
	}
	scan, err := metadata.SparseRepackScanPage(
		ctx, sparseAfterHash, sparseAfterPack, scanLimit, now, minAge, minDead)
	if err != nil {
		return report, err
	}
	lastHash := sparseAfterHash
	lastPack := sparseAfterPack
	examined := 0
	selected := 0
	cursorBlocked := false
	var runErr error
	for _, candidate := range scan.Items {
		if candidate.Eligible && selected == remaining {
			break
		}
		if examined > 0 && budget.MaxBytes > 0 && report.BytesRepacked >= budget.MaxBytes {
			report.BudgetExhausted = true
			break
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if !candidate.Eligible {
			if !cursorBlocked {
				lastHash, lastPack = candidate.Hash, candidate.Usage.PackID
			}
			examined++
			continue
		}
		stats, sourceErr := blobs.RepackWithCatalog(ctx,
			&scopedCatalog{Catalog: baseCatalog, usages: []packstore.PackUsage{candidate.Usage}},
			packstore.RepackOptions{MaxBytes: budget.MaxBytes, Now: now,
				Selection: packstore.RepackSelection{MinAge: minAge, MinDeadStored: minDead}})
		addRepackStats(&report, stats)
		selected++
		if sourceErr != nil {
			runErr = errors.Join(runErr, sourceErr)
			examined++
			if !isRepackSourceContentError(sourceErr) || budget.MaxBytes == 0 {
				report.More, err = repackWorkRemains(ctx, metadata, nil,
					lastHash, lastPack, now, minAge, minDead, false, false)
				if err != nil {
					return report, errors.Join(runErr, err)
				}
				if report.More {
					report.NextCursor = repackPhaseCursor("sparse", lastHash, lastPack)
				}
				return report, runErr
			}
			cursorBlocked = true
			continue
		}
		if !cursorBlocked {
			lastHash, lastPack = candidate.Hash, candidate.Usage.PackID
		}
		examined++
	}
	if runErr != nil {
		report.More, err = repackWorkRemains(ctx, metadata, nil,
			lastHash, lastPack, now, minAge, minDead, false, false)
		if err != nil {
			return report, errors.Join(runErr, err)
		}
		if report.More {
			report.NextCursor = repackPhaseCursor("sparse", lastHash, lastPack)
		}
		return report, runErr
	}
	report.More = examined < len(scan.Items) || scan.More
	if report.More {
		report.NextCursor = repackPhaseCursor("sparse", lastHash, lastPack)
	}
	return report, nil
}

func repackWorkRemains(
	ctx context.Context,
	metadata *store.Store,
	mappingAfter *string,
	sparseAfterHash string,
	sparseAfterPack string,
	now time.Time,
	minAge time.Duration,
	minDead int64,
	includeMappings bool,
	includeDead bool,
) (bool, error) {
	if includeMappings {
		mappings, err := metadata.UnreferencedPackMappingsPage(ctx, mappingAfter, 1)
		if err != nil || len(mappings.Items) > 0 || mappings.More {
			return len(mappings.Items) > 0 || mappings.More, err
		}
	}
	if includeDead {
		dead, _, err := metadata.DeadPackUsagePage(ctx, 1)
		if err != nil || len(dead) > 0 {
			return len(dead) > 0, err
		}
	}
	sparse, err := metadata.SparseRepackScanPage(
		ctx, sparseAfterHash, sparseAfterPack, 1, now, minAge, minDead)
	return len(sparse.Items) > 0, err
}

func repackPhaseCursor(phase, sparseHash, sparsePack string) string {
	if phase == "sparse" && sparseHash != "" {
		return encodeSparseCursor(sparseHash, sparsePack)
	}
	return encodePhaseCursor(operationRepack, "dead", "")
}

func isRepackSourceContentError(err error) bool {
	for _, known := range []error{fs.ErrNotExist, pack.ErrBadMagic, pack.ErrUnsupportedVersion,
		pack.ErrTruncated, pack.ErrChecksum, pack.ErrCorrupt, pack.ErrBlobMismatch,
		packstore.ErrContentMismatch} {
		if errors.Is(err, known) {
			return true
		}
	}
	var pathErr *os.PathError
	return errors.As(err, &pathErr) && errors.Is(pathErr, fs.ErrNotExist)
}

type scopedCatalog struct {
	packstore.Catalog

	usages []packstore.PackUsage
}

func (catalog *scopedCatalog) ListPackUsage(context.Context) ([]packstore.PackUsage, error) {
	return append([]packstore.PackUsage(nil), catalog.usages...), nil
}

func (*scopedCatalog) PruneUnreferenced(context.Context) (int64, error) { return 0, nil }

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
