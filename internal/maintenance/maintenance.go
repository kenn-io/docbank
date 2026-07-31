// Package maintenance contains storage lifecycle operations shared by the
// embedded Vault and daemon HTTP adapters.
package maintenance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	// MaxObjectsPerOperation caps one embedded maintenance allocation and
	// storage page. Callers resume larger jobs with the returned cursor.
	MaxObjectsPerOperation = 10_000

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
	StoreID string
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
	if budget.MaxObjects < 0 || budget.MaxObjects > MaxObjectsPerOperation || budget.MaxBytes < 0 {
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
	primary, err := metadata.PrimaryBlobStore(ctx)
	if err != nil {
		return report, err
	}
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
		hash, err := packstore.ParseHash(candidate.Hash)
		if err != nil {
			return report, fmt.Errorf("parsing GC candidate hash %s: %w", candidate.Hash, err)
		}
		resolution, err := metadata.ResolveBlobLocations(ctx, hash)
		if err != nil {
			return report, err
		}
		var looseSize, packedSize int64
		var packed bool
		for _, location := range resolution.Candidates {
			if location.Loose != nil {
				looseSize += location.Loose.StoredSize
			} else if location.Pack != nil {
				packed = true
				packedSize += location.Pack.StoredLen
			}
		}
		report.CandidateBlobs++
		trackedHashes = append(trackedHashes, candidate.Hash)
		if packed {
			report.PendingPackedBlobs++
			report.PendingPackedBytes += packedSize
		}
		report.ReclaimableBytes += looseSize
		processedBytes += looseSize + packedSize
		if !opts.DryRun {
			removed, err := retireUnreachableLooseLocations(
				ctx, blobs, primary.ID, hash, resolution,
			)
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

func retireUnreachableLooseLocations(
	ctx context.Context,
	blobs *blob.Store,
	primaryStoreID string,
	hash packstore.Hash,
	resolution packstore.Resolution,
) (int, error) {
	backends := make(map[packstore.StoreID]packstore.Backend)
	primaryLoose := false
	for _, location := range resolution.Candidates {
		if location.Loose == nil {
			continue
		}
		if location.StoreID == packstore.StoreID(primaryStoreID) {
			primaryLoose = true
			continue
		}
		backend, ok := blobs.WritableBackend(location.StoreID)
		if !ok {
			return 0, fmt.Errorf(
				"%w: gc cleanup store %s is not writable",
				packstore.ErrStoreUnavailable, location.StoreID,
			)
		}
		backends[location.StoreID] = backend
	}
	retired := 0
	for _, location := range resolution.Candidates {
		if location.Loose == nil ||
			location.StoreID == packstore.StoreID(primaryStoreID) {
			continue
		}
		if err := backends[location.StoreID].Retire(ctx, packstore.ObjectRef{
			LooseHash: hash, LooseEncoding: location.Loose.Encoding,
		}); err != nil {
			return 0, fmt.Errorf(
				"retiring unreachable blob %s from store %s: %w",
				hash, location.StoreID, err,
			)
		}
		retired++
	}
	if !primaryLoose {
		return retired, nil
	}
	primaryRemoved, err := blobs.RemoveIfExists(hash.String())
	return retired + primaryRemoved, err
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
	locationVerifier := NewBlobLocationVerifier(metadata, blobs)
	processedBytes := int64(0)
	processed := 0
	for _, hash := range hashes {
		if processed > 0 && budget.MaxBytes > 0 && processedBytes >= budget.MaxBytes {
			break
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		problems, bytesRead, err := locationVerifier.Verify(ctx, hash)
		if err != nil {
			return report, err
		}
		processedBytes += bytesRead
		if len(problems) == 0 {
			report.OK++
		} else {
			report.Problems = append(report.Problems, problems...)
		}
		processed++
	}
	report.More = processed < len(hashes) || pageMore
	if report.More && processed > 0 {
		report.NextCursor = encodeCursor(operationVerify, hashes[processed-1])
	}
	return report, nil
}

// BlobLocationVerifier independently checks every catalog-authorized physical
// candidate while refreshing each secondary's ownership once per verification
// run. A healthy redundant copy never hides a damaged or fenced peer.
type BlobLocationVerifier struct {
	metadata     *store.Store
	blobs        *blob.Store
	primaryID    string
	observations map[string]blob.StoreObservation
}

// NewBlobLocationVerifier starts one bounded verification observation set.
func NewBlobLocationVerifier(
	metadata *store.Store, blobs *blob.Store,
) *BlobLocationVerifier {
	return &BlobLocationVerifier{
		metadata: metadata, blobs: blobs,
		primaryID:    metadata.PrimaryBlobStoreID(),
		observations: make(map[string]blob.StoreObservation),
	}
}

// Verify checks every current physical candidate for one logical blob.
func (v *BlobLocationVerifier) Verify(
	ctx context.Context, hash string,
) ([]VerifyProblem, int64, error) {
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		// Metadata validation owns the malformed identity detail; verification
		// must still advance past that row and report its bytes as unreadable.
		return []VerifyProblem{{Hash: hash, Problem: "unreadable"}}, 0, nil //nolint:nilerr
	}
	resolution, err := v.metadata.ResolveBlobLocations(ctx, parsed)
	if err != nil {
		return nil, 0, err
	}
	if !resolution.Member {
		return nil, 0, store.ErrNotFound
	}
	if len(resolution.Candidates) == 0 {
		return []VerifyProblem{{Hash: hash, Problem: "missing"}}, 0, nil
	}
	problems := make([]VerifyProblem, 0)
	var totalRead int64
	for _, location := range resolution.Candidates {
		storeID := string(location.StoreID)
		if storeID != v.primaryID {
			observation, observed := v.observations[storeID]
			if !observed {
				observation = v.blobs.RefreshStore(ctx, storeID)
				v.observations[storeID] = observation
			}
			if observation.State != blob.StoreOnline {
				problems = append(problems, VerifyProblem{
					Hash: hash, StoreID: storeID, Problem: "unreadable",
				})
				continue
			}
		}
		read, err := v.blobs.VerifyLocation(ctx, hash, location)
		totalRead += read
		if err == nil {
			continue
		}
		problems = append(problems, VerifyProblem{
			Hash: hash, StoreID: storeID,
			Problem: classifyBlobProblem(err),
		})
	}
	return problems, totalRead, nil
}

func cursorPosition(state cursor) *string {
	if !state.Set {
		return nil
	}
	return &state.Hash
}

func classifyBlobProblem(err error) string {
	if isContentCorruption(err) {
		return "corrupt"
	}
	if errors.Is(err, packstore.ErrPhysicalMissing) || errors.Is(err, fs.ErrNotExist) {
		return "missing"
	}
	return "unreadable"
}

func isContentCorruption(err error) bool {
	return errors.Is(err, packstore.ErrPhysicalCorrupt) ||
		errors.Is(err, packstore.ErrContentMismatch) ||
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
