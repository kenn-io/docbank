package packstore

import (
	"context"
	"io"
)

// OwnershipFormatV1 is the canonical ownership-marker format.
const OwnershipFormatV1 uint32 = 1

// BlobIdentity binds physical movement to immutable logical content.
type BlobIdentity struct {
	Hash Hash
	Size int64
}

// Validate checks the complete logical identity.
func (i BlobIdentity) Validate() error {
	if err := i.Hash.Validate(); err != nil {
		return err
	}
	if i.Size < 0 {
		return ErrInvalidPolicy
	}
	return nil
}

// PublishOptions controls verified loose or pack publication.
type PublishOptions struct {
	Durability   Durability
	Dedup        DedupVerification
	ExpectedSize int64
	SizeKnown    bool
	MaxBytes     int64
	Compression  LooseCompressionOptions
}

// LooseReceipt describes one verified canonical loose publication.
type LooseReceipt struct {
	StoreID    StoreID
	Generation LocationGeneration
	Hash       Hash
	Location   LooseLocation
	Created    bool
}

// PackReceipt describes one verified canonical pack publication.
type PackReceipt struct {
	StoreID    StoreID
	Generation LocationGeneration
	PackID     string
	Size       int64
	Created    bool
}

// ObjectRef identifies exactly one canonical physical object.
type ObjectRef struct {
	LooseHash     Hash
	LooseEncoding LooseEncoding
	PackID        string
}

// InventoryCursor is an opaque backend inventory continuation.
type InventoryCursor string

// InventoryObject describes one recognized canonical physical object.
type InventoryObject struct {
	Ref        ObjectRef
	StoredSize int64
}

// InventoryPage reports recognized canonical objects and preserves unknown
// names as non-authoritative observations.
type InventoryPage struct {
	Objects    []InventoryObject
	Unknown    []string
	NextCursor InventoryCursor
}

// Backend owns byte mechanics for one store. Applications remain responsible
// for logical membership, location authority, and short catalog transactions.
type Backend interface {
	ReadBackend
	Ownership(context.Context) (Ownership, error)
	ReplaceOwnership(context.Context, Ownership, *Ownership) error
	PublishLoose(context.Context, Hash, io.Reader, PublishOptions) (LooseReceipt, error)
	PublishPack(context.Context, string, io.Reader, PublishOptions) (PackReceipt, error)
	Retire(context.Context, ObjectRef) error
	Inventory(context.Context, InventoryCursor) (InventoryPage, error)
	StoreID() StoreID
}

// NamespaceInspector reports whether a configured physical namespace contains
// any object before ownership is attached. Applications use it to avoid
// claiming unrelated or abandoned data merely because no marker exists.
type NamespaceInspector interface {
	NamespaceEmpty(context.Context) (bool, error)
}

// RepairBackend is a writable backend that can deliberately replace a
// damaged canonical loose object. Ordinary publication remains immutable;
// applications must gate this exception behind an explicit repair workflow.
type RepairBackend interface {
	Backend
	RepairLoose(context.Context, Hash, io.Reader, PublishOptions) (LooseReceipt, error)
}

// MoveRequest asks Kit to copy and independently verify one physical
// candidate. It does not authorize a catalog change or source retirement.
type MoveRequest struct {
	Source      ReadLocation
	Destination StoreID
	Identity    BlobIdentity
}

// MoveReceipt is physical evidence suitable for a later application-owned
// catalog transaction.
type MoveReceipt struct {
	Destination ReadLocation
	Verified    bool
	Created     bool
}
