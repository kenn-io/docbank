package packstore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"go.kenn.io/kit/pack"
)

const (
	// PackExt is the stable extension shared with Kit backup repositories.
	PackExt = ".mvpack"

	defaultBlobBytes   = int64(64 << 20)
	defaultPackBytes   = int64(128 << 20)
	defaultFooterBytes = int64(8 << 20)
	defaultPackEntries = 100_000
)

var (
	// ErrInvalidHash reports a value that is not canonical lowercase SHA-256.
	ErrInvalidHash = errors.New("packstore: invalid content hash")
	// ErrDuplicateHash reports duplicate canonical IDs in one immutable pack.
	ErrDuplicateHash = errors.New("packstore: duplicate content hash")
)

// Hash is a canonical lowercase SHA-256 content identity.
type Hash string

// LooseEncoding identifies the physical representation of a loose object.
type LooseEncoding uint8

const (
	// LooseEncodingRaw stores the logical bytes without compression.
	LooseEncodingRaw LooseEncoding = iota + 1
	// LooseEncodingZstd stores the logical bytes in the compressed-loose format.
	LooseEncodingZstd
)

// LooseCompressionOptions controls whether a loose write may select zstd.
// The zero value disables compression for backward compatibility.
type LooseCompressionOptions struct {
	Enabled           bool
	MinBytes          int64
	MinSavingsPercent int
}

// ParseHash validates and returns a canonical content hash.
func ParseHash(value string) (Hash, error) {
	hash := Hash(value)
	if err := hash.Validate(); err != nil {
		return "", err
	}
	return hash, nil
}

// String returns the lowercase hexadecimal identity.
func (h Hash) String() string { return string(h) }

// Validate checks that h is exactly 64 lowercase hexadecimal characters.
func (h Hash) Validate() error {
	if len(h) != 64 {
		return fmt.Errorf("%w %q", ErrInvalidHash, h)
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w %q", ErrInvalidHash, h)
		}
	}
	return nil
}

// Bytes decodes h. It returns nil if a Hash value was forged without ParseHash.
func (h Hash) Bytes() []byte {
	if err := h.Validate(); err != nil {
		return nil
	}
	decoded, err := hex.DecodeString(string(h))
	if err != nil {
		return nil
	}
	return decoded
}

// IndexEntry mirrors one immutable pack footer entry in an application catalog.
type IndexEntry struct {
	Hash      Hash
	PackID    string
	Offset    int64
	StoredLen int64
	RawLen    int64
	Flags     uint8
	CRC32C    uint32
}

// Validate checks the catalog-safe bounds that do not require opening a pack.
func (e IndexEntry) Validate() error {
	if err := e.Hash.Validate(); err != nil {
		return err
	}
	if !pack.IsValidPackID(e.PackID) {
		return fmt.Errorf("packstore: invalid pack id %q", e.PackID)
	}
	if e.Offset < 0 || e.StoredLen < 0 || e.RawLen < 0 {
		return fmt.Errorf("packstore: negative entry metadata for %s", e.Hash)
	}
	if uint64(e.Offset) < uint64(pack.MinEntryOffset) {
		return fmt.Errorf("packstore: entry offset %d precedes pack data", e.Offset)
	}
	if uint64(e.StoredLen) > uint64(pack.MaxStoredLen) {
		return fmt.Errorf("packstore: stored length %d exceeds format maximum %d", e.StoredLen, uint64(pack.MaxStoredLen))
	}
	if e.Offset > math.MaxInt64-e.StoredLen {
		return fmt.Errorf("packstore: entry span overflows for %s", e.Hash)
	}
	if uint64(e.RawLen) > pack.MaxRawLen {
		return fmt.Errorf("packstore: raw length %d exceeds format maximum %d", e.RawLen, uint64(pack.MaxRawLen))
	}
	return nil
}

// ValidateIndexEntries validates entries and rejects duplicate canonical IDs.
func ValidateIndexEntries(entries []IndexEntry) error {
	seen := make(map[Hash]struct{}, len(entries))
	for _, entry := range entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if _, ok := seen[entry.Hash]; ok {
			return fmt.Errorf("%w %s", ErrDuplicateHash, entry.Hash)
		}
		seen[entry.Hash] = struct{}{}
	}
	return nil
}

// PackRecord records immutable totals for one sealed pack.
type PackRecord struct {
	PackID      string
	EntryCount  int64
	StoredBytes int64
	CreatedAt   time.Time
}

// Validate checks catalog-safe pack record fields.
func (r PackRecord) Validate() error {
	if !pack.IsValidPackID(r.PackID) {
		return fmt.Errorf("packstore: invalid pack id %q", r.PackID)
	}
	if r.EntryCount < 0 || r.StoredBytes < 0 {
		return fmt.Errorf("packstore: negative totals for pack %s", r.PackID)
	}
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("packstore: pack %s has zero creation time", r.PackID)
	}
	return nil
}

// Location is the catalog-authorized physical location of a hash.
type Location struct {
	Member bool
	Pack   *IndexEntry
}

// Reference preserves every product spelling that grants canonical membership.
type Reference struct {
	Hash           Hash
	OriginalHashes []string
}

// ReferenceInventory reports canonical membership and whether it was complete
// enough to authorize deletion of otherwise-unreferenced loose objects.
// Complete must be false when an application skipped malformed or otherwise
// unclassifiable references; packing valid candidates remains safe in that
// state, but orphan sweeping does not.
type ReferenceInventory struct {
	References []Reference
	Complete   bool
}

// Candidate describes one unpacked catalog member and its physical candidates.
type Candidate struct {
	Hash           Hash
	OriginalHashes []string
	Paths          []string
	Size           int64
}

// Adoption couples a canonical pack entry to product-specific aliases.
type Adoption struct {
	Entry          IndexEntry
	OriginalHashes []string
}

// PackUsage combines immutable totals with the currently cataloged live subset.
type PackUsage struct {
	PackRecord
	LiveEntries      int64
	LiveStoredBytes  int64
	LiveRawBytes     int64
	MaxLiveStoredLen int64
	MaxLiveRawLen    int64
}

// RepackMove describes one exact compare-and-swap to a replacement entry.
type RepackMove struct {
	OldPackID string
	NewEntry  IndexEntry
}

// Limits bounds allocation and parsing for Store.ReadBounded, packed
// Store.OpenStream, and maintenance. Store.Open is the buffered compatibility
// path and does not enforce these limits. BlobBytes also does not cap
// OpenStream for authorized loose files; use ReadBounded or caller policy when
// a loose-read work limit is required.
type Limits struct {
	BlobBytes   int64
	PackBytes   int64
	FooterBytes int64
	PackEntries int
}

// DefaultLimits returns conservative bounded-read, packed-stream, and
// maintenance ceilings for applications that do not supply a custom policy.
func DefaultLimits() Limits {
	return Limits{
		BlobBytes:   defaultBlobBytes,
		PackBytes:   defaultPackBytes,
		FooterBytes: defaultFooterBytes,
		PackEntries: defaultPackEntries,
	}
}
