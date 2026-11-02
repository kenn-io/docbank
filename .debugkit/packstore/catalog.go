package packstore

import "context"

// Resolver grants read authority and returns an optional current pack mapping.
type Resolver interface {
	Resolve(context.Context, Hash) (Location, error)
}

// InventoryCatalog supplies stable snapshots for repair and packing.
// ListUnpacked order is preserved as an application-provided physical-locality
// hint; callers should return candidates in a useful dominant read order.
type InventoryCatalog interface {
	ListReferences(context.Context) (ReferenceInventory, error)
	ListUnpacked(context.Context) ([]Candidate, error)
	ListIndexed(context.Context) ([]IndexEntry, error)
	ListPackRecords(context.Context) ([]PackRecord, error)
	ListPackEntries(context.Context, string) ([]IndexEntry, error)
	HasPackRecord(context.Context, string) (bool, error)
}

// PackingCatalog owns atomic application metadata changes used by repair and packing.
type PackingCatalog interface {
	PruneUnreferenced(context.Context) (int64, error)
	RecordPack(context.Context, PackRecord, []Adoption) error
	AdoptPack(context.Context, PackRecord, []Adoption) error
	DeletePackRecord(context.Context, string) error
	DeleteIndexEntry(context.Context, Hash) error
}

// RepackCatalog owns live accounting and exact-set replacement transactions.
type RepackCatalog interface {
	ListPackUsage(context.Context) ([]PackUsage, error)
	ListLivePackEntries(context.Context, string) ([]IndexEntry, error)
	CommitRepack(context.Context, []string, []PackRecord, []RepackMove) error
	DeleteEmptyPackRecord(context.Context, string) (bool, error)
}

// UnpackCatalog drops packed authority after durable loose materialization.
type UnpackCatalog interface {
	ClearPackMetadata(context.Context) error
}

// Catalog composes every application capability required by maintenance.
type Catalog interface {
	Resolver
	InventoryCatalog
	PackingCatalog
	RepackCatalog
	UnpackCatalog
}
