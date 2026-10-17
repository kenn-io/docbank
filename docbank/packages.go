package docbank

import (
	"context"
	"fmt"
	"strconv"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

// PackagePreflightRequest describes a load-file source to validate. The
// embedded API accepts the same request as POST /api/v1/packages/preflights.
type PackagePreflightRequest = api.PackagePreflightRequest

// PackagePreflight is the retained validation result for one source.
type PackagePreflight = api.PackagePreflight

// PackageImportRequest starts a durable import from a successful preflight.
type PackageImportRequest = api.PackageImportRequest

// PackageImportJob reports the progress of one admitted import operation.
type PackageImportJob = api.PackageImportJob

// PackageListRequest pages through retained packages. Direction filters to
// "received" or "produced"; Cursor continues after a package ID returned as
// NextCursor; Limit is 1 to 250 and defaults to 100.
type PackageListRequest struct {
	Direction string
	Cursor    string
	Limit     int
}

// PackageMemberRequest pages through the immutable members of one package.
// Cursor is the ordinal returned as NextCursor; Limit is 1 to 250.
type PackageMemberRequest struct {
	PackageID string
	Cursor    string
	Limit     int
}

// LabelLookupRequest finds every scoped match for one exact Bates label.
type LabelLookupRequest struct {
	Label      string
	PackageID  string
	LabelSet   string
	Provenance string
	Cursor     string
	Limit      int
}

// PackageVolume is one sender volume and the root the operator mapped it to.
type PackageVolume struct {
	Ordinal            int    `json:"ordinal"`
	VolumeName         string `json:"volume_name"`
	DeclaredRoot       string `json:"declared_root"`
	MappedRoot         string `json:"mapped_root"`
	ResolvedRootSHA256 string `json:"resolved_root_sha256"`
}

// Package is one retained received or produced load-file package.
type Package struct {
	PackageID            string          `json:"package_id"`
	SnapshotID           string          `json:"snapshot_id,omitempty"`
	Direction            string          `json:"direction"`
	PackageName          string          `json:"package_name"`
	PartyLabel           string          `json:"party_label"`
	State                string          `json:"state"`
	ProfileSHA256        string          `json:"profile_sha256"`
	MappingSHA256        string          `json:"mapping_sha256"`
	ManifestSHA256       string          `json:"manifest_sha256"`
	PredecessorPackageID string          `json:"predecessor_package_id,omitempty"`
	Relation             string          `json:"relation,omitempty"`
	IngestID             string          `json:"ingest_id,omitempty"`
	ExportPlanID         string          `json:"export_plan_id,omitempty"`
	ProducedOn           string          `json:"produced_on,omitempty"`
	CreatedAt            string          `json:"created_at"`
	CompletedAt          string          `json:"completed_at,omitempty"`
	Volumes              []PackageVolume `json:"volumes"`
	DocumentCount        int             `json:"document_count"`
	PageCount            int             `json:"page_count"`
}

// PackagePage is one bounded page of packages.
type PackagePage struct {
	Items      []Package `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// PackageRepresentation is one declared file role of a member and whether the
// package supplied it.
type PackageRepresentation struct {
	Role             string `json:"role"`
	Status           string `json:"status"`
	TextAuthority    string `json:"text_authority"`
	ContentVersionID string `json:"content_version_id,omitempty"`
	BlobSHA256       string `json:"blob_sha256,omitempty"`
	MediaType        string `json:"media_type,omitempty"`
	Size             int64  `json:"size,omitempty"`
	PageNumber       int    `json:"page_number,omitempty"`
}

// PackageMember is one immutable document occurrence inside a package.
type PackageMember struct {
	RowID              string                  `json:"row_id,omitempty"`
	Ordinal            int                     `json:"ordinal"`
	OccurrenceID       string                  `json:"occurrence_id"`
	NodeID             int64                   `json:"node_id"`
	ContentVersionID   string                  `json:"content_version_id"`
	DocumentKind       string                  `json:"document_kind"`
	FamilyID           string                  `json:"family_id"`
	ParentOccurrenceID string                  `json:"parent_occurrence_id,omitempty"`
	FamilyOrder        int                     `json:"family_order"`
	Representations    []PackageRepresentation `json:"representations"`
}

// PackageMemberPage is one bounded page of package members.
type PackageMemberPage struct {
	Items      []PackageMember `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// PackageRecord is one retained sender row. Embedded callers own the vault, so
// every column is returned; Sensitive reports whether the retained mapping
// classified any field as sensitive.
type PackageRecord struct {
	PackageID    string               `json:"package_id"`
	RowID        string               `json:"row_id"`
	LoadFile     string               `json:"load_file"`
	RowOrdinal   int                  `json:"row_ordinal"`
	OccurrenceID string               `json:"occurrence_id"`
	Columns      map[string]string    `json:"columns"`
	Fields       []PackageRecordField `json:"fields"`
	Sensitive    bool                 `json:"sensitive"`
}

// PackageRecordField is one column of a retained sender row.
type PackageRecordField struct {
	Ordinal   int    `json:"ordinal"`
	Column    string `json:"column"`
	Canonical string `json:"canonical,omitempty"`
	Value     string `json:"value"`
	Sensitive bool   `json:"sensitive"`
}

// LabelLookupMatch is one occurrence or page that carries the looked-up label.
type LabelLookupMatch struct {
	PackageID        string `json:"package_id"`
	LabelSet         string `json:"label_set"`
	Provenance       string `json:"provenance"`
	OccurrenceID     string `json:"occurrence_id"`
	ContentVersionID string `json:"content_version_id"`
	ArtifactID       string `json:"artifact_id,omitempty"`
	PageNumber       int    `json:"page_number,omitempty"`
	PageState        string `json:"page_state"`
	Endpoint         string `json:"endpoint"`
}

// LabelLookupResult is one bounded page of label matches.
type LabelLookupResult struct {
	Label      string             `json:"label"`
	Matches    []LabelLookupMatch `json:"matches"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// PreflightPackage validates a load-file source and retains the result for a
// later ImportPackage call.
func (v *Vault) PreflightPackage(ctx context.Context, request PackagePreflightRequest) (PackagePreflight, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return PackagePreflight{}, ErrClosed
	}
	return api.PreflightPackage(ctx, api.Deps{Store: v.metadata, Blobs: v.blobs, VaultRoot: v.vaultRoot},
		embeddedMutationGate{vault: v}, v.packageOwner(), request)
}

// PackagePreflight reads one retained preflight owned by this vault.
func (v *Vault) PackagePreflight(ctx context.Context, preflightID string) (PackagePreflight, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return PackagePreflight{}, ErrClosed
	}
	return api.ReadPackagePreflight(ctx, v.metadata, v.packageOwner(), preflightID)
}

// ImportPackage admits one durable import. Repeating the same OperationID
// returns the original job.
func (v *Vault) ImportPackage(ctx context.Context, request PackageImportRequest) (PackageImportJob, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return PackageImportJob{}, ErrClosed
	}
	return api.AdmitPackageImport(ctx, api.Deps{Store: v.metadata, Blobs: v.blobs, VaultRoot: v.vaultRoot},
		embeddedMutationGate{vault: v}, v.packageOwner(), request)
}

// PackageImportStatus reports progress for one admitted operation.
func (v *Vault) PackageImportStatus(ctx context.Context, operationID string) (PackageImportJob, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return PackageImportJob{}, ErrClosed
	}
	return api.ReadPackageImport(ctx, v.metadata, v.packageOwner(), operationID)
}

// Packages lists retained packages in stable ID order.
func (v *Vault) Packages(ctx context.Context, request PackageListRequest) (PackagePage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return PackagePage{}, ErrClosed
	}
	limit, err := packagePageLimit(request.Limit)
	if err != nil {
		return PackagePage{}, err
	}
	items, err := v.metadata.Packages(ctx, request.Direction, request.Cursor, limit)
	if err != nil {
		return PackagePage{}, err
	}
	page := PackagePage{Items: make([]Package, 0, len(items))}
	for _, item := range items {
		page.Items = append(page.Items, packageFromStore(item))
	}
	if len(items) == limit {
		page.NextCursor = items[len(items)-1].PackageID
	}
	return page, nil
}

// Package reads one retained package by ID.
func (v *Vault) Package(ctx context.Context, packageID string) (Package, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return Package{}, ErrClosed
	}
	row, err := v.metadata.Package(ctx, packageID)
	if err != nil {
		return Package{}, err
	}
	return packageFromStore(row), nil
}

// PackageMembers lists the immutable members of one package in ordinal order.
func (v *Vault) PackageMembers(ctx context.Context, request PackageMemberRequest) (PackageMemberPage, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return PackageMemberPage{}, ErrClosed
	}
	after := 0
	if request.Cursor != "" {
		parsed, err := strconv.Atoi(request.Cursor)
		if err != nil || parsed < 0 {
			return PackageMemberPage{}, fmt.Errorf("%w: member cursor %q", ErrInvalidArgument, request.Cursor)
		}
		after = parsed
	}
	limit, err := packagePageLimit(request.Limit)
	if err != nil {
		return PackageMemberPage{}, err
	}
	items, err := v.metadata.PackageMembers(ctx, request.PackageID, after, limit)
	if err != nil {
		return PackageMemberPage{}, err
	}
	occurrences := make([]string, 0, len(items))
	for _, item := range items {
		occurrences = append(occurrences, item.OccurrenceID)
	}
	rowIDs, err := v.metadata.PackageRecordRowIDs(ctx, request.PackageID, occurrences)
	if err != nil {
		return PackageMemberPage{}, err
	}
	page := PackageMemberPage{Items: make([]PackageMember, 0, len(items))}
	for _, item := range items {
		view := packageMemberFromStore(item)
		view.RowID = rowIDs[item.OccurrenceID]
		page.Items = append(page.Items, view)
	}
	if len(items) == limit {
		page.NextCursor = strconv.Itoa(items[len(items)-1].Ordinal)
	}
	return page, nil
}

// PackageRecord reads one retained sender row with every column.
func (v *Vault) PackageRecord(ctx context.Context, packageID, rowID string) (PackageRecord, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return PackageRecord{}, ErrClosed
	}
	row, err := v.metadata.PackageRecord(ctx, packageID, rowID)
	if err != nil {
		return PackageRecord{}, err
	}
	record, err := canonical.Decode[loadfile.Record](row.RawJSON)
	if err != nil {
		return PackageRecord{}, err
	}
	columns := make(map[string]string, len(record.Fields))
	fields := make([]PackageRecordField, 0, len(record.Fields))
	explicitSensitive := map[int]bool{}
	pkg, err := v.metadata.Package(ctx, packageID)
	if err != nil {
		return PackageRecord{}, err
	}
	mapping, err := canonical.Decode[loadfile.Mapping]([]byte(pkg.MappingJSON))
	if err != nil {
		return PackageRecord{}, err
	}
	for _, column := range mapping.Columns {
		if column.SourceOrdinal != nil && column.Sensitive {
			explicitSensitive[*column.SourceOrdinal] = true
		}
	}
	for _, field := range record.Fields {
		columns[field.Column] = field.Raw
		fields = append(fields, PackageRecordField{Ordinal: field.Ordinal, Column: field.Column,
			Canonical: field.Canonical, Value: field.Raw,
			Sensitive: loadfile.PackageFieldSensitive(field.Canonical, explicitSensitive[field.Ordinal])})
	}
	return PackageRecord{PackageID: row.PackageID, RowID: row.RowID, LoadFile: row.LoadFile,
		RowOrdinal: row.RowOrdinal, OccurrenceID: row.OccurrenceID, Columns: columns, Fields: fields,
		Sensitive: row.Sensitive}, nil
}

// LookupLabel finds every scoped match for one exact Bates label.
func (v *Vault) LookupLabel(ctx context.Context, request LabelLookupRequest) (LabelLookupResult, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return LabelLookupResult{}, ErrClosed
	}
	limit, err := packagePageLimit(request.Limit)
	if err != nil {
		return LabelLookupResult{}, err
	}
	items, next, err := v.metadata.PackageLabelCandidates(ctx, request.Label, request.PackageID,
		request.LabelSet, request.Provenance, request.Cursor, limit)
	if err != nil {
		return LabelLookupResult{}, err
	}
	result := LabelLookupResult{Label: request.Label, Matches: make([]LabelLookupMatch, 0, len(items)), NextCursor: next}
	for _, item := range items {
		result.Matches = append(result.Matches, LabelLookupMatch{PackageID: item.PackageID,
			LabelSet: item.LabelSet, Provenance: item.Provenance, OccurrenceID: item.OccurrenceID,
			ContentVersionID: item.ContentVersionID, ArtifactID: item.ArtifactID,
			PageNumber: item.PageNumber, PageState: item.PageState, Endpoint: item.Endpoint})
	}
	return result, nil
}

func (v *Vault) packageOwner() string { return "embedded-package:" + v.metadata.VaultID() }

func packagePageLimit(limit int) (int, error) {
	if limit == 0 {
		return 100, nil
	}
	if limit < 1 || limit > 250 {
		return 0, fmt.Errorf("%w: page limit %d is outside 1..250", ErrInvalidArgument, limit)
	}
	return limit, nil
}

func packageFromStore(row store.Package) Package {
	volumes := make([]PackageVolume, 0, len(row.Volumes))
	for _, volume := range row.Volumes {
		volumes = append(volumes, PackageVolume{Ordinal: volume.Ordinal, VolumeName: volume.VolumeName,
			DeclaredRoot: volume.DeclaredRoot, MappedRoot: volume.MappedRoot,
			ResolvedRootSHA256: volume.ResolvedRootSHA256})
	}
	return Package{PackageID: row.PackageID, SnapshotID: row.SnapshotID, Direction: row.Direction,
		PackageName: row.PackageName, PartyLabel: row.PartyLabel, State: row.State,
		ProfileSHA256: row.ProfileSHA256, MappingSHA256: row.MappingSHA256,
		ManifestSHA256: row.ManifestSHA256, PredecessorPackageID: row.PredecessorPackageID,
		Relation: row.Relation, IngestID: row.IngestID, ExportPlanID: row.ExportPlanID,
		ProducedOn: row.ProducedOn, CreatedAt: row.CreatedAt, CompletedAt: row.CompletedAt,
		Volumes: volumes, DocumentCount: row.MemberCount, PageCount: row.PageCount}
}

func packageMemberFromStore(row store.CollectionSnapshotMember) PackageMember {
	representations := make([]PackageRepresentation, 0, len(row.Representations))
	for _, item := range row.Representations {
		representations = append(representations, PackageRepresentation{Role: item.Role, Status: item.Status,
			TextAuthority: item.TextAuthority, ContentVersionID: item.ContentVersionID,
			BlobSHA256: item.BlobSHA256, MediaType: item.MediaType, Size: item.Size, PageNumber: item.PageNumber})
	}
	return PackageMember{Ordinal: row.Ordinal, OccurrenceID: row.OccurrenceID, NodeID: row.NodeID,
		ContentVersionID: row.ContentVersionID, DocumentKind: row.DocumentKind, FamilyID: row.FamilyID,
		ParentOccurrenceID: row.ParentOccurrenceID, FamilyOrder: row.FamilyOrder, Representations: representations}
}
