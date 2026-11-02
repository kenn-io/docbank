package packstore

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// StoreID is an application-owned stable physical-store identity.
type StoreID string

// LocationGeneration changes whenever physical authority at one location is
// replaced or repaired.
type LocationGeneration string

// LooseLocation describes one catalog-authorized loose representation.
// Multi-location resolvers must provide an explicit encoding and sizes. The
// internal legacy adapter alone may defer representation discovery.
type LooseLocation struct {
	Encoding    LooseEncoding
	LogicalSize int64
	StoredSize  int64
	legacy      bool
}

// ReadLocation identifies one catalog-authorized physical representation.
type ReadLocation struct {
	StoreID    StoreID
	Generation LocationGeneration
	Loose      *LooseLocation
	Pack       *IndexEntry
}

// Validate checks that a location names one store generation and exactly one
// physical representation.
func (l ReadLocation) Validate() error {
	if l.StoreID == "" {
		return errors.New("packstore: empty store id")
	}
	if l.Generation == "" {
		return errors.New("packstore: empty location generation")
	}
	if (l.Loose == nil) == (l.Pack == nil) {
		return errors.New("packstore: location must select exactly one representation")
	}
	if l.Loose != nil {
		return l.Loose.validate()
	}
	return l.Pack.Validate()
}

func (l LooseLocation) validate() error {
	if l.LogicalSize < 0 || l.StoredSize < 0 {
		return errors.New("packstore: negative loose location size")
	}
	if l.Encoding == 0 {
		if !l.legacy {
			return fmt.Errorf(
				"%w: loose location requires an explicit encoding",
				ErrInvalidPolicy,
			)
		}
		if l.LogicalSize != 0 || l.StoredSize != 0 {
			return errors.New("packstore: legacy loose location must omit sizes")
		}
		return nil
	}
	if l.Encoding != LooseEncodingRaw && l.Encoding != LooseEncodingZstd {
		return fmt.Errorf("packstore: invalid loose encoding %d", l.Encoding)
	}
	return nil
}

// Resolution grants logical membership and returns candidates in the
// application's preferred stable order.
type Resolution struct {
	Member     bool
	Candidates []ReadLocation
}

// LocationResolver returns every currently authorized read candidate.
type LocationResolver interface {
	ResolveLocations(context.Context, Hash) (Resolution, error)
}

// ReadBackend opens verified loose and packed streams from one physical store.
// Physical failures must wrap the matching ErrStoreUnavailable,
// ErrStoreFenced, ErrPhysicalMissing, or ErrPhysicalCorrupt sentinel so Store
// can preserve evidence and safely try another candidate before exposure.
type ReadBackend interface {
	OpenLoose(context.Context, Hash, LooseLocation) (VerifiedReadCloser, int64, error)
	OpenPack(context.Context, Hash, IndexEntry) (VerifiedReadCloser, int64, error)
}

type seekableReadBackend interface {
	OpenSeekableLoose(context.Context, Hash, LooseLocation) (io.ReadSeekCloser, int64, error)
	OpenSeekablePack(context.Context, Hash, IndexEntry) (io.ReadSeekCloser, int64, error)
}

type boundedReadBackend interface {
	ReadLooseBounded(context.Context, Hash, LooseLocation, int64) ([]byte, int64, error)
	ReadPackBounded(context.Context, Hash, IndexEntry, int64) ([]byte, int64, error)
}

// BackendRegistry resolves configured read backends by stable store identity.
type BackendRegistry interface {
	Backend(StoreID) (ReadBackend, bool)
}

// MultiStoreOptions configures multi-location reads.
type MultiStoreOptions struct {
	Limits      Limits
	ReaderSlots int
	Health      *Health
}

// NewMultiStore constructs a reader over application-ordered physical stores.
func NewMultiStore(
	resolver LocationResolver,
	backends BackendRegistry,
	opts MultiStoreOptions,
) (*Store, error) {
	if resolver == nil {
		return nil, errors.New("packstore: location resolver is nil")
	}
	if backends == nil {
		return nil, errors.New("packstore: backend registry is nil")
	}
	store, err := newStoreOptions(opts.Limits, opts.ReaderSlots)
	if err != nil {
		return nil, err
	}
	store.locationResolver = resolver
	store.backends = backends
	store.health = opts.Health
	store.retryResolution = true
	store.observeStreams = true
	if store.health == nil {
		store.health = NewHealth()
	}
	return store, nil
}

type candidateOpen[T any] func(ReadBackend, ReadLocation) (T, int64, error)

func resolveCandidates[T any](
	ctx context.Context,
	store *Store,
	hash Hash,
	open candidateOpen[T],
) (T, int64, ReadLocation, error) {
	var zero T
	resolution, err := store.locationResolver.ResolveLocations(ctx, hash)
	if err != nil {
		return zero, 0, ReadLocation{}, err
	}
	value, size, location, exhausted, err := attemptResolution(store, hash, resolution, open)
	if err != nil || exhausted == nil {
		return value, size, location, err
	}
	if !store.retryResolution || !errors.Is(exhausted, ErrPhysicalMissing) {
		return zero, 0, ReadLocation{}, exhausted
	}
	refreshed, err := store.locationResolver.ResolveLocations(ctx, hash)
	if err != nil {
		return zero, 0, ReadLocation{}, err
	}
	if sameResolution(hash, resolution, refreshed) {
		return zero, 0, ReadLocation{}, exhausted
	}
	value, size, location, refreshedExhausted, err := attemptResolution(store, hash, refreshed, open)
	if err != nil || refreshedExhausted == nil {
		return value, size, location, err
	}
	return zero, 0, ReadLocation{}, refreshedExhausted
}

func attemptResolution[T any](
	store *Store,
	hash Hash,
	resolution Resolution,
	open candidateOpen[T],
) (T, int64, ReadLocation, *ExhaustedError, error) {
	var zero T
	if err := validateResolution(hash, resolution); err != nil {
		return zero, 0, ReadLocation{}, nil, err
	}
	if !resolution.Member {
		return zero, 0, ReadLocation{}, nil, blobNotFound(hash)
	}
	if len(resolution.Candidates) == 0 {
		return zero, 0, ReadLocation{}, nil, ErrPhysicalAuthorityMissing
	}
	var attempts []AttemptError
	candidates := resolution.Candidates
	if store.observeStreams {
		candidates = store.health.Order(hash, candidates)
	}
	for _, location := range candidates {
		backend, ok := store.backends.Backend(location.StoreID)
		if !ok || backend == nil {
			err := fmt.Errorf("%w: store %s has no read backend", ErrStoreUnavailable, location.StoreID)
			attempts = append(attempts, AttemptError{Location: location, Err: err})
			store.health.Observe(hash, location, err)
			continue
		}
		value, size, err := open(backend, location)
		if err == nil {
			return value, size, location, nil, nil
		}
		if !isCandidateFailure(err) {
			return zero, 0, ReadLocation{}, nil, err
		}
		store.health.Observe(hash, location, err)
		attempts = append(attempts, AttemptError{Location: location, Err: err})
	}
	return zero, 0, ReadLocation{}, newExhaustedError(attempts), nil
}

func validateResolution(hash Hash, resolution Resolution) error {
	if !resolution.Member && len(resolution.Candidates) != 0 {
		return errors.New("packstore: non-member resolution contains physical candidates")
	}
	var seen map[locationHealthKey]struct{}
	if len(resolution.Candidates) > 1 {
		seen = make(map[locationHealthKey]struct{}, len(resolution.Candidates))
	}
	for _, location := range resolution.Candidates {
		if err := location.Validate(); err != nil {
			return err
		}
		if location.Pack != nil && location.Pack.Hash != hash {
			return fmt.Errorf(
				"packstore: candidate hash %s does not match requested hash %s",
				location.Pack.Hash,
				hash,
			)
		}
		if seen != nil {
			key := healthKey(hash, location)
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf(
					"packstore: duplicate physical candidate for store %s generation %s",
					location.StoreID,
					location.Generation,
				)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

func sameResolution(hash Hash, left, right Resolution) bool {
	if left.Member != right.Member || len(left.Candidates) != len(right.Candidates) {
		return false
	}
	for i := range left.Candidates {
		if healthKey(hash, left.Candidates[i]) != healthKey(hash, right.Candidates[i]) {
			return false
		}
	}
	return true
}

func openBackendStream(
	ctx context.Context,
	backend ReadBackend,
	hash Hash,
	location ReadLocation,
) (VerifiedReadCloser, int64, error) {
	var stream VerifiedReadCloser
	var size int64
	var err error
	if location.Loose != nil {
		stream, size, err = backend.OpenLoose(ctx, hash, *location.Loose)
	} else {
		stream, size, err = backend.OpenPack(ctx, hash, *location.Pack)
	}
	if err != nil {
		return nil, 0, err
	}
	if stream == nil {
		return nil, 0, errors.New("packstore: backend returned a nil verified stream")
	}
	if size < 0 {
		return nil, 0, errors.Join(
			fmt.Errorf("packstore: backend returned negative content size %d", size),
			stream.Close(),
		)
	}
	var expectedSize int64
	var checkSize bool
	if location.Loose != nil {
		expectedSize = location.Loose.LogicalSize
		checkSize = location.Loose.Encoding != 0
	} else {
		expectedSize = location.Pack.RawLen
		checkSize = true
	}
	if checkSize && size != expectedSize {
		return nil, 0, errors.Join(
			ErrPhysicalCorrupt,
			fmt.Errorf(
				"packstore: backend logical size %d does not match catalog size %d",
				size,
				expectedSize,
			),
			stream.Close(),
		)
	}
	return stream, size, nil
}
