package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/pdfstamp"
	"go.kenn.io/docbank/internal/store"
)

const (
	loadFileExportContract    = "docbank-loadfile-export/v1"
	loadFileCrosswalkContract = "docbank-loadfile-crosswalk/v1"
	loadFileExportVolume      = "VOL001"
	loadFileExportManifest    = "docbank/manifest.jsonl"
	loadFileExportCrosswalk   = "docbank/crosswalk.json"
	loadFileExportReceipt     = "docbank/receipt.json"
	maxLoadFileTextBytes      = 256 << 20
)

var loadFileExportTargets = []string{
	"loadfile.document.id", "loadfile.family.parent", "loadfile.family.id", "loadfile.label.set",
	"loadfile.label.begin", "loadfile.label.end", "loadfile.label.assigned.set", "loadfile.label.assigned.begin",
	"loadfile.label.assigned.end", "loadfile.file.native", "loadfile.file.supplied_text", "loadfile.file.produced_pdf", "", "loadfile.custodian",
}

type LoadFileExportRequest struct {
	SnapshotID        string `json:"snapshot_id"`
	SourcePackageID   string `json:"source_package_id,omitzero"`
	ProfileID         string `json:"profile_id"`
	BatesAllocationID string `json:"bates_allocation_id,omitzero"`
}

type LoadFileExportLabel struct {
	Provenance string `json:"provenance"`
	LabelSet   string `json:"label_set"`
	Label      string `json:"label"`
	Endpoint   string `json:"endpoint"`
	PageNumber int    `json:"page_number,omitzero"`
}

type LoadFileExportRole struct {
	Role       string `json:"role"`
	RelPath    string `json:"rel_path"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	SourcePage int    `json:"source_page,omitzero"`
	OutputPage int    `json:"output_page,omitzero"`
}

type LoadFileExportCustodian struct {
	AssignmentID string `json:"assignment_id"`
	ScopeKind    string `json:"scope_kind"`
	RawLabel     string `json:"raw_label"`
	Rank         string `json:"rank"`
	Basis        string `json:"basis"`
	SourceRef    string `json:"source_ref"`
}

type LoadFileExportBatesPage struct {
	Label                string `json:"label"`
	SourcePage           int    `json:"source_page"`
	ArtifactOutputPage   int    `json:"artifact_output_page"`
	DerivativeOutputPage int    `json:"derivative_output_page"`
}

type LoadFileExportArtifact struct {
	RelPath string `json:"rel_path"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Pages   int    `json:"pages"`
}

type LoadFileExportMember struct {
	Ordinal                int                       `json:"ordinal"`
	OccurrenceID           string                    `json:"occurrence_id"`
	DocumentID             string                    `json:"document_id"`
	ParentDocumentID       string                    `json:"parent_document_id,omitzero"`
	FamilyID               string                    `json:"family_id"`
	ContentVersionID       string                    `json:"content_version_id"`
	SourceBlobSHA256       string                    `json:"source_blob_sha256"`
	Roles                  []LoadFileExportRole      `json:"roles"`
	Labels                 []LoadFileExportLabel     `json:"labels"`
	BatesPages             []LoadFileExportBatesPage `json:"bates_pages"`
	PrimaryCustodian       *LoadFileExportCustodian  `json:"primary_custodian,omitzero"`
	Custodians             []LoadFileExportCustodian `json:"custodians"`
	OmittedCustodianClaims []string                  `json:"omitted_custodian_claims"`
	OmittedRoles           []string                  `json:"omitted_roles"`
}

type LoadFileExportCrosswalk struct {
	Contract          string                  `json:"contract"`
	SnapshotID        string                  `json:"snapshot_id"`
	SourcePackageID   string                  `json:"source_package_id,omitzero"`
	BatesAllocationID string                  `json:"bates_allocation_id,omitzero"`
	ProfileID         string                  `json:"profile_id"`
	CombinedArtifact  *LoadFileExportArtifact `json:"combined_artifact,omitzero"`
	Members           []LoadFileExportMember  `json:"members"`
}

type LoadFileExportReceipt struct {
	Contract          string           `json:"contract"`
	SnapshotID        string           `json:"snapshot_id"`
	SourcePackageID   string           `json:"source_package_id,omitzero"`
	ProfileID         string           `json:"profile_id"`
	BatesAllocationID string           `json:"bates_allocation_id,omitzero"`
	LoadFile          string           `json:"load_file"`
	PageMap           string           `json:"page_map,omitzero"`
	Profile           loadfile.Profile `json:"profile"`
	Mapping           loadfile.Mapping `json:"mapping"`
	ProfileSHA256     string           `json:"profile_sha256"`
	MappingSHA256     string           `json:"mapping_sha256"`
	ManifestSHA256    string           `json:"manifest_sha256"`
	CrosswalkSHA256   string           `json:"crosswalk_sha256"`
	ArchiveSHA256     string           `json:"archive_sha256,omitzero"`
	Size              int64            `json:"size,omitzero"`
	RecordCount       int              `json:"record_count"`
	PageCount         int              `json:"page_count"`
}

type LoadFileExportResult struct {
	Receipt   LoadFileExportReceipt
	Manifest  loadfile.Manifest
	Crosswalk LoadFileExportCrosswalk
}

type loadFileArchiveEntry struct {
	name string
	data []byte
	hash string
	size int64
}

type batesLoadFileMember struct {
	entry     loadFileArchiveEntry
	labels    []LoadFileExportLabel
	pageMap   []LoadFileExportBatesPage
	pageCount int
}

// WriteLoadFileExport writes one deterministic verified load-file ZIP from a
// sealed snapshot. The archive contains the normalized manifest, mapping and
// source-to-output crosswalk used by fresh-vault import verification.
func WriteLoadFileExport(ctx context.Context, catalog *store.Store, blobs *blob.Store,
	request LoadFileExportRequest, destination io.Writer,
) (LoadFileExportResult, error) {
	var result LoadFileExportResult
	if catalog == nil || blobs == nil || destination == nil || request.SnapshotID == "" || request.ProfileID == "" {
		return result, store.ErrPackageConflict
	}
	snapshot, err := catalog.CollectionSnapshot(ctx, request.SnapshotID)
	if err != nil {
		return result, err
	}
	if snapshot.MemberCount < 1 || snapshot.MemberCount > store.MaxSnapshotMembers {
		return result, store.ErrPackageConflict
	}
	if request.SourcePackageID != "" {
		pkg, err := catalog.Package(ctx, request.SourcePackageID)
		if err != nil || pkg.SnapshotID != request.SnapshotID || pkg.State != "complete" && pkg.State != "partial" {
			return result, store.ErrPackageConflict
		}
	}
	members, err := loadAllSnapshotMembers(ctx, catalog, request.SnapshotID, snapshot.MemberCount)
	if err != nil {
		return result, err
	}
	exportProfile, err := loadfile.ReadExportProfile(request.ProfileID)
	if err != nil {
		return result, err
	}
	batesMembers, combinedArtifact, combinedEntry, err := loadBatesPackageMembers(ctx, catalog, blobs, request, members)
	if err != nil {
		return result, err
	}
	profile, mapping, mappingSHA, err := loadFileOutputContract(exportProfile)
	if err != nil {
		return result, err
	}
	profileSHA, err := profile.SHA256()
	if err != nil {
		return result, err
	}
	documentIDs := make(map[string]string, len(members))
	familyIDs := make(map[string]string, len(members))
	for _, member := range members {
		documentIDs[member.OccurrenceID] = fmt.Sprintf("DOC%06d", member.Ordinal)
	}
	for _, member := range members {
		familyIDs[member.OccurrenceID] = documentIDs[member.FamilyID]
		if familyIDs[member.OccurrenceID] == "" {
			return result, store.ErrPackageConflict
		}
	}
	loadFileName := loadFileExportVolume + "/DATA.DAT"
	if exportProfile.DAT == "csv-rfc4180-v1" {
		loadFileName = loadFileExportVolume + "/DATA.CSV"
	}
	pageMapName := ""
	switch exportProfile.PageMap {
	case "opt-standard-v1":
		pageMapName = loadFileExportVolume + "/DATA.OPT"
	case "lfp-ipro-v1":
		pageMapName = loadFileExportVolume + "/DATA.LFP"
	}
	manifest := loadfile.Manifest{ProfileSHA256: profileSHA, MappingSHA256: mappingSHA, Mapping: mapping,
		Volumes: []loadfile.Volume{{Name: loadFileExportVolume, DeclaredRoot: loadFileExportVolume, Ordinal: 1}}}
	crosswalk := LoadFileExportCrosswalk{Contract: loadFileCrosswalkContract, SnapshotID: request.SnapshotID,
		SourcePackageID: request.SourcePackageID, BatesAllocationID: request.BatesAllocationID, ProfileID: request.ProfileID,
		CombinedArtifact: combinedArtifact,
		Members:          make([]LoadFileExportMember, 0, len(members))}
	artifacts := make([]loadFileArchiveEntry, 0)
	if combinedEntry.name != "" {
		artifacts = append(artifacts, combinedEntry)
	}
	pageCount := 0
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		docID := documentIDs[member.OccurrenceID]
		labels, err := loadExportLabels(ctx, catalog, request.SourcePackageID, member)
		if err != nil {
			return result, err
		}
		recordLabels := labels
		if batesMember, ok := batesMembers[member.OccurrenceID]; ok {
			recordLabels = append(slices.Clone(batesMember.labels), labels...)
			labels = append(labels, batesMember.labels...)
		}
		entry := LoadFileExportMember{Ordinal: member.Ordinal, OccurrenceID: member.OccurrenceID,
			DocumentID: docID, ParentDocumentID: documentIDs[member.ParentOccurrenceID],
			FamilyID: familyIDs[member.OccurrenceID], ContentVersionID: member.ContentVersionID,
			SourceBlobSHA256: member.BlobSHA256, Labels: labels}
		entry.PrimaryCustodian, entry.Custodians, entry.OmittedCustodianClaims, err = loadExportCustodians(
			ctx, catalog, snapshot, request.SourcePackageID, member)
		if err != nil {
			return result, err
		}
		if batesMember, ok := batesMembers[member.OccurrenceID]; ok {
			entry.BatesPages = batesMember.pageMap
		}
		record, roles, files, images, entries, err := buildLoadFileRecord(ctx, blobs, exportProfile, profile,
			member, entry, recordLabels, loadFileName, batesMembers[member.OccurrenceID])
		if err != nil {
			return result, err
		}
		entry.Roles = roles
		available := make([]string, 0, len(roles))
		for _, role := range roles {
			if !slices.Contains(available, role.Role) {
				available = append(available, role.Role)
			}
		}
		entry.OmittedRoles, err = loadfile.CheckExportRoles(request.ProfileID, available, request.BatesAllocationID != "")
		if err != nil {
			return result, err
		}
		manifest.Records = append(manifest.Records, record)
		manifest.Files = append(manifest.Files, files...)
		manifest.Images = append(manifest.Images, images...)
		if batesMember, ok := batesMembers[member.OccurrenceID]; ok {
			pageCount += batesMember.pageCount
		} else if rep := selectedPDFRepresentation(member, availableRepresentations(member)["produced_pdf"]); rep != nil &&
			slices.Contains(exportProfile.RequiredRoles, "produced_pdf") {
			pageCount += rep.VerifiedPageCount
		} else {
			pageCount += len(images)
		}
		artifacts = append(artifacts, entries...)
		crosswalk.Members = append(crosswalk.Members, entry)
	}
	loadFileBytes, err := serializeLoadFile(manifest.Records, profile)
	if err != nil {
		return result, err
	}
	entries := []loadFileArchiveEntry{{name: loadFileName, data: loadFileBytes, size: int64(len(loadFileBytes))}}
	if pageMapName != "" {
		pageMapBytes, err := serializePageMap(manifest.Images, exportProfile.PageMap)
		if err != nil {
			return result, err
		}
		entries = append(entries, loadFileArchiveEntry{name: pageMapName, data: pageMapBytes, size: int64(len(pageMapBytes))})
	}
	entries = append(entries, artifacts...)
	var manifestBytes bytes.Buffer
	if err := manifest.WriteJSONL(&manifestBytes); err != nil {
		return result, err
	}
	manifestSHA := sha256HexBytes(manifestBytes.Bytes())
	crosswalkBytes, err := canonical.Marshal(crosswalk)
	if err != nil {
		return result, err
	}
	receipt := LoadFileExportReceipt{Contract: loadFileExportContract, SnapshotID: request.SnapshotID,
		SourcePackageID: request.SourcePackageID, ProfileID: request.ProfileID, BatesAllocationID: request.BatesAllocationID,
		LoadFile: loadFileName, PageMap: pageMapName, Profile: profile, Mapping: mapping,
		ProfileSHA256: profileSHA, MappingSHA256: mappingSHA, ManifestSHA256: manifestSHA,
		CrosswalkSHA256: sha256HexBytes(crosswalkBytes), RecordCount: len(manifest.Records),
		PageCount: pageCount}
	receiptBytes, err := canonical.Marshal(receipt)
	if err != nil {
		return result, err
	}
	entries = append(entries,
		loadFileArchiveEntry{name: loadFileExportManifest, data: manifestBytes.Bytes(), size: int64(manifestBytes.Len())},
		loadFileArchiveEntry{name: loadFileExportCrosswalk, data: crosswalkBytes, size: int64(len(crosswalkBytes))},
		loadFileArchiveEntry{name: loadFileExportReceipt, data: receiptBytes, size: int64(len(receiptBytes))})
	slices.SortFunc(entries, func(left, right loadFileArchiveEntry) int { return strings.Compare(left.name, right.name) })
	digest := sha256.New()
	counting := &countingWriter{writer: io.MultiWriter(destination, digest)}
	archive := zip.NewWriter(counting)
	for _, entry := range entries {
		if err := writeLoadFileArchiveEntry(ctx, archive, blobs, entry); err != nil {
			_ = archive.Close()
			return result, err
		}
	}
	if err := archive.Close(); err != nil {
		return result, fmt.Errorf("close load-file export archive: %w", err)
	}
	receipt.ArchiveSHA256 = hex.EncodeToString(digest.Sum(nil))
	receipt.Size = counting.count
	return LoadFileExportResult{Receipt: receipt, Manifest: manifest, Crosswalk: crosswalk}, nil
}

func loadAllSnapshotMembers(ctx context.Context, catalog *store.Store, snapshotID string, count int) ([]store.CollectionSnapshotMember, error) {
	members := make([]store.CollectionSnapshotMember, 0, count)
	for after := 0; len(members) < count; {
		page, err := catalog.SnapshotMembers(ctx, snapshotID, after, min(250, count-len(members)))
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return nil, store.ErrPackageConflict
		}
		members = append(members, page...)
		after = page[len(page)-1].Ordinal
	}
	return members, nil
}

func loadFileOutputContract(exportProfile loadfile.ExportProfile) (loadfile.Profile, loadfile.Mapping, string, error) {
	profile, err := loadfile.ReadProfile(exportProfile.DAT)
	if err != nil {
		return profile, loadfile.Mapping{}, "", err
	}
	profile.Columns = []string{"DOCID", "PARENTID", "FAMILYID", "RECEIVEDLABELSET", "RECEIVEDBEG", "RECEIVEDEND",
		"ASSIGNEDLABELSET", "ASSIGNEDBEG", "ASSIGNEDEND", "NATIVE", "TEXT", "PDF", "PAGECOUNT", "CUSTODIAN"}
	mapping := loadfile.Mapping{Contract: loadfile.MappingContractV1, Columns: make([]loadfile.MappingColumn, 0, len(loadFileExportTargets))}
	mapping.CustodianColumn = "CUSTODIAN"
	for ordinal, target := range loadFileExportTargets {
		if target == "" {
			continue
		}
		value := target
		mapping.Columns = append(mapping.Columns, loadfile.MappingColumn{Source: profile.Columns[ordinal], SourceOrdinal: new(ordinal), Canonical: &value})
	}
	raw, err := canonical.Marshal(mapping)
	if err != nil {
		return profile, loadfile.Mapping{}, "", err
	}
	mapping, mappingSHA, err := loadfile.DecodeMapping(raw, profile.Columns)
	return profile, mapping, mappingSHA, err
}

func loadExportLabels(ctx context.Context, catalog *store.Store, packageID string,
	member store.CollectionSnapshotMember,
) ([]LoadFileExportLabel, error) {
	if packageID == "" {
		return []LoadFileExportLabel{}, nil
	}
	labels, err := catalog.PackageLabels(ctx, packageID, member.OccurrenceID)
	if err != nil {
		return nil, err
	}
	result := make([]LoadFileExportLabel, len(labels))
	for index, label := range labels {
		if label.ContentVersionID != member.ContentVersionID {
			return nil, store.ErrPackageConflict
		}
		result[index] = LoadFileExportLabel{Provenance: label.Provenance, LabelSet: label.LabelSet,
			Label: label.Label, Endpoint: label.Endpoint, PageNumber: label.PageNumber}
	}
	return result, nil
}

func loadExportCustodians(
	ctx context.Context,
	catalog *store.Store,
	snapshot store.CollectionSnapshot,
	packageID string,
	member store.CollectionSnapshotMember,
) (*LoadFileExportCustodian, []LoadFileExportCustodian, []string, error) {
	levels := make([][]store.CustodianAssignment, 0, 3+len(snapshot.SourceCollectionIDs))
	load := func(scope store.CustodianScope) ([]store.CustodianAssignment, error) {
		claims, total, err := catalog.Custodians(ctx, scope, false, 250, 0)
		if err != nil {
			return nil, err
		}
		if total != int64(len(claims)) {
			return nil, store.ErrPackageConflict
		}
		return claims, nil
	}
	if packageID != "" {
		record, err := catalog.PackageRecordByOccurrence(ctx, packageID, member.OccurrenceID)
		if err != nil {
			return nil, nil, nil, err
		}
		claims, err := load(store.CustodianScope{Kind: "package", PackageID: packageID,
			PackageRecordID: record.RowID, HasPackageRecordID: true})
		if err != nil {
			return nil, nil, nil, err
		}
		levels = append(levels, claims)
		claims, err = load(store.CustodianScope{Kind: "package", PackageID: packageID,
			HasPackageRecordID: true})
		if err != nil {
			return nil, nil, nil, err
		}
		levels = append(levels, claims)
	}
	documentClaims, err := load(store.CustodianScope{Kind: "document", NodeID: member.NodeID,
		ContentVersionID: member.ContentVersionID})
	if err != nil {
		return nil, nil, nil, err
	}
	levels = append(levels, slices.DeleteFunc(documentClaims, func(claim store.CustodianAssignment) bool {
		return claim.Basis != "operator_assigned"
	}))
	for _, sourceID := range member.SourceCollectionIDs {
		claims, err := load(store.CustodianScope{Kind: "collection", IngestID: sourceID})
		if err != nil {
			return nil, nil, nil, err
		}
		levels = append(levels, claims)
	}
	var primary *LoadFileExportCustodian
	all := make([]LoadFileExportCustodian, 0)
	seen := make(map[string]bool)
	for _, level := range levels {
		levelPrimaries := make([]store.CustodianAssignment, 0, 1)
		for _, claim := range level {
			if claim.Rank == "primary" {
				levelPrimaries = append(levelPrimaries, claim)
			}
			if seen[claim.AssignmentID] {
				continue
			}
			seen[claim.AssignmentID] = true
			all = append(all, exportCustodian(claim))
		}
		if len(levelPrimaries) > 1 {
			return nil, nil, nil, store.ErrPackageConflict
		}
		if primary == nil && len(levelPrimaries) == 1 {
			value := exportCustodian(levelPrimaries[0])
			primary = &value
		}
	}
	omitted := make([]string, 0, len(all))
	for _, claim := range all {
		if primary == nil || claim.AssignmentID != primary.AssignmentID {
			omitted = append(omitted, claim.AssignmentID)
		}
	}
	return primary, all, omitted, nil
}

func exportCustodian(value store.CustodianAssignment) LoadFileExportCustodian {
	return LoadFileExportCustodian{AssignmentID: value.AssignmentID, ScopeKind: value.ScopeKind,
		RawLabel: value.RawLabel, Rank: value.Rank, Basis: value.Basis, SourceRef: value.SourceRef}
}

func buildLoadFileRecord(ctx context.Context, blobs *blob.Store, exportProfile loadfile.ExportProfile,
	profile loadfile.Profile, member store.CollectionSnapshotMember, crosswalk LoadFileExportMember,
	labels []LoadFileExportLabel, loadFileName string, bates batesLoadFileMember,
) (loadfile.Record, []LoadFileExportRole, []loadfile.FileRef, []loadfile.ImageRef, []loadFileArchiveEntry, error) {
	values := make([]string, len(profile.Columns))
	values[0], values[1], values[2] = crosswalk.DocumentID, crosswalk.ParentDocumentID, crosswalk.FamilyID
	if crosswalk.PrimaryCustodian != nil {
		values[13] = crosswalk.PrimaryCustodian.RawLabel
	}
	applyLoadFileExportLabels(values, labels)
	roles := make([]LoadFileExportRole, 0)
	files := make([]loadfile.FileRef, 0)
	images := make([]loadfile.ImageRef, 0)
	entries := make([]loadFileArchiveEntry, 0)
	addBlob := func(role, relPath, hash string, size int64, sourcePage, outputPage int) {
		files = append(files, loadfile.FileRef{Role: role, Volume: loadFileExportVolume, RelPath: relPath,
			Declared: loadFileExportVolume + "/" + relPath, SHA256: hash, Size: size, Status: packageStateAvailable})
		roles = append(roles, LoadFileExportRole{Role: role, RelPath: relPath, SHA256: hash, Size: size,
			SourcePage: sourcePage, OutputPage: outputPage})
		entries = append(entries, loadFileArchiveEntry{name: loadFileExportVolume + "/" + relPath, hash: hash, size: size})
	}
	available := availableRepresentations(member)
	if slices.Contains(exportProfile.RequiredRoles, "native") || slices.Contains(exportProfile.OptionalRoles, "native") {
		rep := firstRepresentation(available["native"])
		hash, size := member.BlobSHA256, member.Size
		extension := safeExportExtension(member.DisplayName, ".bin")
		if rep != nil {
			hash, size = rep.BlobSHA256, rep.Size
			extension = exportMediaExtension(rep.MediaType, extension)
		}
		relPath := "NATIVE/" + crosswalk.DocumentID + extension
		values[9] = loadFileExportVolume + "/" + relPath
		addBlob("native", relPath, hash, size, 0, 0)
	}
	textBytes, textSourceRole, err := selectedTextBytes(ctx, blobs, member, available)
	if err != nil {
		return loadfile.Record{}, nil, nil, nil, nil, err
	}
	if len(textBytes) > 0 && (slices.Contains(exportProfile.OptionalRoles, "supplied_text") ||
		slices.Contains(exportProfile.OptionalRoles, "rendition_text")) {
		relPath := "TEXT/" + crosswalk.DocumentID + ".txt"
		hash := sha256HexBytes(textBytes)
		values[10] = loadFileExportVolume + "/" + relPath
		files = append(files, loadfile.FileRef{Role: "supplied_text", Volume: loadFileExportVolume,
			RelPath: relPath, Declared: values[10], SHA256: hash, Size: int64(len(textBytes)), Status: packageStateAvailable})
		roles = append(roles, LoadFileExportRole{Role: textSourceRole, RelPath: relPath, SHA256: hash, Size: int64(len(textBytes))})
		entries = append(entries, loadFileArchiveEntry{name: loadFileExportVolume + "/" + relPath, data: textBytes, size: int64(len(textBytes))})
	}
	if slices.Contains(exportProfile.RequiredRoles, "produced_pdf") || slices.Contains(exportProfile.OptionalRoles, "produced_pdf") {
		if bates.entry.hash != "" {
			relPath := "PDF/" + crosswalk.DocumentID + ".pdf"
			values[11], values[12] = loadFileExportVolume+"/"+relPath, strconv.Itoa(bates.pageCount)
			files = append(files, loadfile.FileRef{Role: "produced_pdf", Volume: loadFileExportVolume, RelPath: relPath,
				Declared: values[11], SHA256: bates.entry.hash, Size: bates.entry.size, Status: packageStateAvailable,
				VerifiedPageCount: bates.pageCount})
			roles = append(roles, LoadFileExportRole{Role: "stamped_pdf", RelPath: relPath,
				SHA256: bates.entry.hash, Size: bates.entry.size})
			bates.entry.name = loadFileExportVolume + "/" + relPath
			entries = append(entries, bates.entry)
			for index, page := range bates.pageMap {
				images = append(images, loadfile.ImageRef{ImageKey: page.Label, Volume: loadFileExportVolume,
					RelPath: relPath, DocumentBreak: index == 0, PageOrdinal: index + 1,
					SourcePage: index + 1, DeclaredPageCount: bates.pageCount})
			}
		} else if rep := selectedPDFRepresentation(member, available["produced_pdf"]); rep != nil {
			relPath := "PDF/" + crosswalk.DocumentID + ".pdf"
			values[11] = loadFileExportVolume + "/" + relPath
			values[12] = strconv.Itoa(rep.VerifiedPageCount)
			addBlob("produced_pdf", relPath, rep.BlobSHA256, rep.Size, 0, 0)
			files[len(files)-1].VerifiedPageCount = rep.VerifiedPageCount
			for page := 1; page <= rep.VerifiedPageCount; page++ {
				imageKey := exportPageLabel(labels, page)
				if imageKey == "" {
					imageKey = fmt.Sprintf("%s-%06d", crosswalk.DocumentID, page)
				}
				images = append(images, loadfile.ImageRef{ImageKey: imageKey, Volume: loadFileExportVolume,
					RelPath: relPath, DocumentBreak: page == 1, PageOrdinal: page,
					SourcePage: page, DeclaredPageCount: rep.VerifiedPageCount})
			}
		}
	}
	if slices.Contains(exportProfile.RequiredRoles, "page_image") || slices.Contains(exportProfile.OptionalRoles, "page_image") {
		pageRepresentations, err := selectedPageRepresentations(member, available["page_image"])
		if err != nil {
			return loadfile.Record{}, nil, nil, nil, nil, err
		}
		for index, rep := range pageRepresentations {
			relPath := fmt.Sprintf("IMAGES/%s-%06d.tif", crosswalk.DocumentID, index+1)
			addBlob("page_image", relPath, rep.BlobSHA256, rep.Size, rep.PageNumber, index+1)
			imageKey := exportPageLabel(labels, rep.PageNumber)
			if imageKey == "" {
				imageKey = fmt.Sprintf("%s-%06d", crosswalk.DocumentID, index+1)
			}
			boundary := ""
			if index == 0 {
				boundary = "document"
				if crosswalk.ParentDocumentID != "" {
					boundary = "child"
				}
			}
			images = append(images, loadfile.ImageRef{ImageKey: imageKey, Volume: loadFileExportVolume,
				RelPath: relPath, DocumentBreak: index == 0, PageOrdinal: index + 1,
				SourcePage: rep.PageNumber, DeclaredPageCount: len(pageRepresentations), Boundary: boundary})
		}
	}
	fields := make([]loadfile.Field, len(values))
	for index, value := range values {
		fields[index] = loadfile.Field{Column: profile.Columns[index], Ordinal: index, Canonical: loadFileExportTargets[index], Raw: value,
			Value: loadfile.Value{Kind: "text", Text: value}}
	}
	recordFiles := make([]loadfile.FileRef, 0, len(files))
	for _, file := range files {
		if file.Role != "page_image" {
			recordFiles = append(recordFiles, file)
		}
	}
	record := loadfile.Record{DocID: crosswalk.DocumentID, LoadFile: loadFileName,
		RowOrdinal: member.Ordinal + 1, ColumnOrder: slices.Clone(profile.Columns), Fields: fields,
		Files:  recordFiles,
		Family: loadfile.Family{ParentDocID: crosswalk.ParentDocumentID, GroupID: crosswalk.FamilyID}}
	return record, roles, files, images, entries, nil
}

func applyLoadFileExportLabels(values []string, labels []LoadFileExportLabel) {
	for _, label := range labels {
		setColumn, labelColumn := -1, -1
		switch {
		case label.Provenance == packageDirectionReceived && label.Endpoint == "begin":
			setColumn, labelColumn = 3, 4
		case label.Provenance == packageDirectionReceived && label.Endpoint == "end":
			setColumn, labelColumn = 3, 5
		case label.Provenance == packageLabelProvenanceAssigned && label.Endpoint == "begin":
			setColumn, labelColumn = 6, 7
		case label.Provenance == packageLabelProvenanceAssigned && label.Endpoint == "end":
			setColumn, labelColumn = 6, 8
		}
		if setColumn >= 0 && labelColumn >= 0 && setColumn < len(values) && labelColumn < len(values) &&
			values[labelColumn] == "" && (values[setColumn] == "" || values[setColumn] == label.LabelSet) {
			values[setColumn] = label.LabelSet
			values[labelColumn] = label.Label
		}
	}
}

func loadBatesPackageMembers(ctx context.Context, catalog *store.Store, blobs *blob.Store,
	request LoadFileExportRequest, members []store.CollectionSnapshotMember,
) (map[string]batesLoadFileMember, *LoadFileExportArtifact, loadFileArchiveEntry, error) {
	result := make(map[string]batesLoadFileMember)
	if request.BatesAllocationID == "" {
		return result, nil, loadFileArchiveEntry{}, nil
	}
	if request.ProfileID != "export-dat-pdf-v1" {
		return nil, nil, loadFileArchiveEntry{}, loadfile.ErrInvalidProfile
	}
	allocation, err := catalog.BatesAllocation(ctx, request.BatesAllocationID)
	if err != nil || allocation.SnapshotID != request.SnapshotID || allocation.State != "committed" {
		return nil, nil, loadFileArchiveEntry{}, store.ErrPackageConflict
	}
	data, artifact, err := ReadBatesExport(ctx, catalog, blobs, request.BatesAllocationID)
	if err != nil {
		return nil, nil, loadFileArchiveEntry{}, err
	}
	combinedPath := loadFileExportVolume + "/ARTIFACTS/" + request.BatesAllocationID + ".pdf"
	combined := &LoadFileExportArtifact{RelPath: combinedPath, SHA256: artifact.BlobSHA256,
		Size: artifact.Size, Pages: artifact.PageCount}
	combinedEntry := loadFileArchiveEntry{name: combinedPath, data: data, hash: artifact.BlobSHA256, size: artifact.Size}
	memberSet := make(map[string]bool, len(members))
	for _, member := range members {
		memberSet[member.OccurrenceID] = true
	}
	grouped := make(map[string][]store.BatesArtifactPage, len(members))
	for _, page := range artifact.Pages {
		if !memberSet[page.OccurrenceID] {
			return nil, nil, loadFileArchiveEntry{}, store.ErrPackageConflict
		}
		grouped[page.OccurrenceID] = append(grouped[page.OccurrenceID], page)
	}
	for _, member := range members {
		pages := grouped[member.OccurrenceID]
		if len(pages) == 0 {
			return nil, nil, loadFileArchiveEntry{}, store.ErrPackageConflict
		}
		labels := make([]LoadFileExportLabel, 0, len(pages)+2)
		for _, page := range pages {
			labels = append(labels, LoadFileExportLabel{Provenance: packageLabelProvenanceAssigned, LabelSet: allocation.NamespaceID,
				Label: page.Label, Endpoint: "page", PageNumber: page.SourcePage})
		}
		labels = append(labels,
			LoadFileExportLabel{Provenance: packageLabelProvenanceAssigned, LabelSet: allocation.NamespaceID, Label: pages[0].Label, Endpoint: "begin"},
			LoadFileExportLabel{Provenance: packageLabelProvenanceAssigned, LabelSet: allocation.NamespaceID, Label: pages[len(pages)-1].Label, Endpoint: "end"})
		outputPages := make([]int, len(pages))
		visible := make([]pdfstamp.PageLabel, len(pages))
		pageMap := make([]LoadFileExportBatesPage, len(pages))
		for index, page := range pages {
			outputPages[index] = page.OutputPage
			visible[index] = pdfstamp.PageLabel{SourcePage: index + 1, Label: page.Label}
			pageMap[index] = LoadFileExportBatesPage{Label: page.Label, SourcePage: page.SourcePage,
				ArtifactOutputPage: page.OutputPage, DerivativeOutputPage: index + 1}
		}
		var selected bytes.Buffer
		selection, selectErr := pdfstamp.SelectPagesSupervised(ctx, bytes.NewReader(data), outputPages, &selected)
		if selectErr != nil {
			return nil, nil, loadFileArchiveEntry{}, selectErr
		}
		if _, verifyErr := pdfstamp.VerifyStamped(ctx, selected.Bytes(), visible); verifyErr != nil {
			return nil, nil, loadFileArchiveEntry{}, verifyErr
		}
		entry := loadFileArchiveEntry{data: selected.Bytes(), hash: selection.SHA256, size: selection.Size}
		result[member.OccurrenceID] = batesLoadFileMember{entry: entry, labels: labels,
			pageMap: pageMap, pageCount: len(pages)}
	}
	return result, combined, combinedEntry, nil
}

func availableRepresentations(member store.CollectionSnapshotMember) map[string][]store.CollectionSnapshotRepresentation {
	result := make(map[string][]store.CollectionSnapshotRepresentation)
	for _, rep := range member.Representations {
		if rep.Status == packageStateAvailable {
			result[rep.Role] = append(result[rep.Role], rep)
		}
	}
	for role := range result {
		slices.SortFunc(result[role], func(left, right store.CollectionSnapshotRepresentation) int {
			if left.PageNumber != right.PageNumber {
				return left.PageNumber - right.PageNumber
			}
			return left.Ordinal - right.Ordinal
		})
	}
	return result
}

func firstRepresentation(values []store.CollectionSnapshotRepresentation) *store.CollectionSnapshotRepresentation {
	if len(values) == 0 {
		return nil
	}
	return &values[0]
}

func selectedPDFRepresentation(member store.CollectionSnapshotMember, values []store.CollectionSnapshotRepresentation) *store.CollectionSnapshotRepresentation {
	for index := range values {
		if member.SelectedSourcePages == nil || values[index].VerifiedPageCount == len(member.SelectedSourcePages) {
			return &values[index]
		}
	}
	if member.SelectedSourcePages == nil && member.DocumentKind == "pdf" {
		return &store.CollectionSnapshotRepresentation{BlobSHA256: member.BlobSHA256, Size: member.Size,
			VerifiedPageCount: member.SourcePageCount}
	}
	return nil
}

func selectedPageRepresentations(member store.CollectionSnapshotMember, values []store.CollectionSnapshotRepresentation) ([]store.CollectionSnapshotRepresentation, error) {
	if member.SelectedSourcePages == nil {
		return values, nil
	}
	selected := make([]store.CollectionSnapshotRepresentation, 0, len(member.SelectedSourcePages))
	for _, page := range member.SelectedSourcePages {
		index := slices.IndexFunc(values, func(value store.CollectionSnapshotRepresentation) bool { return value.PageNumber == page })
		if index < 0 {
			return nil, loadfile.ErrPackageIncomplete
		}
		selected = append(selected, values[index])
	}
	return selected, nil
}

func selectedTextBytes(ctx context.Context, blobs *blob.Store, member store.CollectionSnapshotMember,
	available map[string][]store.CollectionSnapshotRepresentation,
) ([]byte, string, error) {
	role := "supplied_text"
	values := available[role]
	if len(values) == 0 {
		role, values = "rendition_text", available["rendition_text"]
	}
	if len(values) == 0 {
		return nil, "", nil
	}
	if member.SelectedSourcePages != nil && len(member.SelectedSourcePages) < member.SourcePageCount {
		selected := make([]store.CollectionSnapshotRepresentation, 0, len(member.SelectedSourcePages))
		for _, page := range member.SelectedSourcePages {
			index := slices.IndexFunc(values, func(value store.CollectionSnapshotRepresentation) bool { return value.PageNumber == page })
			if index < 0 {
				return nil, "", nil
			}
			selected = append(selected, values[index])
		}
		values = selected
	} else if whole := slices.IndexFunc(values, func(value store.CollectionSnapshotRepresentation) bool { return value.PageNumber == 0 }); whole >= 0 {
		values = values[whole : whole+1]
	}
	var output bytes.Buffer
	for index, rep := range values {
		data, err := readExportBlob(ctx, blobs, rep.BlobSHA256, rep.Size, maxLoadFileTextBytes-int64(output.Len()))
		if err != nil {
			return nil, "", err
		}
		if !utf8.Valid(data) {
			return nil, "", loadfile.ErrUnrepresentable
		}
		if index > 0 {
			output.WriteString("\n\f\n")
		}
		output.Write(data)
	}
	return output.Bytes(), role, nil
}

func readExportBlob(ctx context.Context, blobs *blob.Store, hash string, size, remaining int64) ([]byte, error) {
	if size < 0 || size > remaining {
		return nil, loadfile.ErrLoadfileLimit
	}
	stream, actual, err := blobs.OpenStreamContext(ctx, hash)
	if err != nil {
		return nil, err
	}
	if actual != size {
		_ = stream.Close()
		return nil, store.ErrPackageConflict
	}
	data, readErr := io.ReadAll(io.LimitReader(stream, size+1))
	closeErr := stream.Close()
	if err := errors.Join(readErr, closeErr, ctx.Err()); err != nil {
		return nil, err
	}
	if int64(len(data)) != size || !stream.Verified() {
		return nil, store.ErrPackageConflict
	}
	return data, nil
}

func exportPageLabel(labels []LoadFileExportLabel, sourcePage int) string {
	for _, provenance := range []string{packageLabelProvenanceAssigned, packageDirectionReceived} {
		for _, label := range labels {
			if label.Provenance == provenance && label.Endpoint == "page" && label.PageNumber == sourcePage {
				return label.Label
			}
		}
	}
	return ""
}

func safeExportExtension(name, fallback string) string {
	extension := strings.ToLower(path.Ext(strings.ReplaceAll(name, `\`, "/")))
	if len(extension) < 2 || len(extension) > 16 {
		return fallback
	}
	for _, char := range extension[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return fallback
		}
	}
	return extension
}

func exportMediaExtension(mediaType, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0])) {
	case "text/plain":
		return ".txt"
	case "application/pdf":
		return ".pdf"
	case "image/tiff":
		return ".tif"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	default:
		return fallback
	}
}

func serializeLoadFile(records []loadfile.Record, profile loadfile.Profile) ([]byte, error) {
	var output bytes.Buffer
	var err error
	if profile.ID == "csv-rfc4180-v1" {
		err = loadfile.WriteCSV(&output, records, profile)
	} else {
		err = loadfile.WriteDAT(&output, records, profile)
	}
	return output.Bytes(), err
}

func serializePageMap(images []loadfile.ImageRef, profileID string) ([]byte, error) {
	profile, err := loadfile.ReadProfile(profileID)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if profileID == "lfp-ipro-v1" {
		err = loadfile.WriteLFP(&output, images, profile)
	} else {
		err = loadfile.WriteOPT(&output, images, profile)
	}
	return output.Bytes(), err
}

type countingWriter struct {
	writer io.Writer
	count  int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.count += int64(n)
	return n, err
}

func writeLoadFileArchiveEntry(ctx context.Context, archive *zip.Writer, blobs *blob.Store, entry loadFileArchiveEntry) error {
	header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
	header.SetMode(0o600)
	header.Modified = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	output, err := archive.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create load-file export entry: %w", err)
	}
	if entry.data != nil {
		_, err = output.Write(entry.data)
		return err
	}
	stream, size, err := blobs.OpenStreamContext(ctx, entry.hash)
	if err != nil {
		return err
	}
	if size != entry.size {
		_ = stream.Close()
		return store.ErrPackageConflict
	}
	written, copyErr := io.Copy(output, &contextReader{ctx: ctx, reader: stream})
	closeErr := stream.Close()
	if err := errors.Join(copyErr, closeErr, ctx.Err()); err != nil {
		return err
	}
	if written != entry.size || !stream.Verified() {
		return store.ErrPackageConflict
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

func sha256HexBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// VerifyLoadFileExport independently reopens the archive, validates every
// declared artifact and sidecar hash, and reparses its load file and page map.
func VerifyLoadFileExport(ctx context.Context, source io.ReaderAt, size int64) (LoadFileExportResult, error) {
	var result LoadFileExportResult
	if source == nil || size <= 0 {
		return result, loadfile.ErrMalformedInput
	}
	archive, err := zip.NewReader(source, size)
	if err != nil {
		return result, fmt.Errorf("open load-file export: %w", err)
	}
	entries := make(map[string]*zip.File, len(archive.File))
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || entries[entry.Name] != nil || strings.Contains(entry.Name, `\`) || path.Clean(entry.Name) != entry.Name {
			return result, loadfile.ErrUnsafeReference
		}
		entries[entry.Name] = entry
	}
	receiptBytes, err := readZIPEntry(entries[loadFileExportReceipt], 1<<20)
	if err != nil {
		return result, err
	}
	receipt, err := canonical.Decode[LoadFileExportReceipt](receiptBytes)
	if err != nil || receipt.Contract != loadFileExportContract || receipt.ArchiveSHA256 != "" || receipt.Size != 0 ||
		receipt.RecordCount < 1 || receipt.RecordCount > store.MaxSnapshotMembers {
		return result, loadfile.ErrMalformedInput
	}
	profileSHA, err := receipt.Profile.SHA256()
	if err != nil || profileSHA != receipt.ProfileSHA256 {
		return result, loadfile.ErrMalformedInput
	}
	mappingBytes, err := canonical.Marshal(receipt.Mapping)
	if err != nil {
		return result, err
	}
	mapping, mappingSHA, err := loadfile.DecodeMapping(mappingBytes, receipt.Profile.Columns)
	if err != nil || mappingSHA != receipt.MappingSHA256 {
		return result, loadfile.ErrMalformedInput
	}
	receipt.Mapping = mapping
	manifestBytes, err := readZIPEntry(entries[loadFileExportManifest], 512<<20)
	if err != nil || sha256HexBytes(manifestBytes) != receipt.ManifestSHA256 {
		return result, loadfile.ErrMalformedInput
	}
	manifest, err := loadfile.ReadManifestJSONL(bytes.NewReader(manifestBytes), receipt.ManifestSHA256)
	if err != nil || manifest.ProfileSHA256 != receipt.ProfileSHA256 || manifest.MappingSHA256 != receipt.MappingSHA256 {
		return result, loadfile.ErrMalformedInput
	}
	crosswalkBytes, err := readZIPEntry(entries[loadFileExportCrosswalk], 512<<20)
	if err != nil || sha256HexBytes(crosswalkBytes) != receipt.CrosswalkSHA256 {
		return result, loadfile.ErrMalformedInput
	}
	crosswalk, err := canonical.Decode[LoadFileExportCrosswalk](crosswalkBytes)
	if err != nil || crosswalk.Contract != loadFileCrosswalkContract || crosswalk.SnapshotID != receipt.SnapshotID ||
		crosswalk.SourcePackageID != receipt.SourcePackageID || crosswalk.BatesAllocationID != receipt.BatesAllocationID ||
		crosswalk.ProfileID != receipt.ProfileID || len(crosswalk.Members) != receipt.RecordCount {
		return result, loadfile.ErrMalformedInput
	}
	allowed := map[string]bool{
		loadFileExportReceipt: true, loadFileExportManifest: true, loadFileExportCrosswalk: true,
		receipt.LoadFile: true,
	}
	if receipt.PageMap != "" {
		allowed[receipt.PageMap] = true
	}
	if crosswalk.CombinedArtifact != nil {
		allowed[crosswalk.CombinedArtifact.RelPath] = true
	}
	for _, ref := range manifest.Files {
		allowed[ref.Volume+"/"+ref.RelPath] = true
	}
	if err := verifyLoadFileBatesArtifacts(ctx, entries, receipt, manifest, crosswalk); err != nil {
		return result, err
	}
	if len(entries) != len(allowed) {
		return result, loadfile.ErrMalformedInput
	}
	for name := range entries {
		if !allowed[name] {
			return result, loadfile.ErrMalformedInput
		}
	}
	for _, ref := range manifest.Files {
		entry := entries[ref.Volume+"/"+ref.RelPath]
		if entry == nil || ref.Size < 0 || entry.UncompressedSize64 != uint64(ref.Size) {
			return result, loadfile.ErrMalformedInput
		}
		stream, err := entry.Open()
		if err != nil {
			return result, fmt.Errorf("open exported artifact: %w", err)
		}
		digest := sha256.New()
		written, readErr := io.Copy(digest, &contextReader{ctx: ctx, reader: stream})
		closeErr := stream.Close()
		if err := errors.Join(readErr, closeErr, ctx.Err()); err != nil {
			return result, err
		}
		if written != ref.Size || hex.EncodeToString(digest.Sum(nil)) != ref.SHA256 {
			return result, fmt.Errorf("verify exported artifact %s/%s: %w", ref.Volume, ref.RelPath, store.ErrPackageConflict)
		}
	}
	loadFileEntry := entries[receipt.LoadFile]
	if loadFileEntry == nil {
		return result, loadfile.ErrMalformedInput
	}
	stream, err := loadFileEntry.Open()
	if err != nil {
		return result, fmt.Errorf("open exported load file: %w", err)
	}
	var records []loadfile.Record
	if receipt.Profile.ID == "csv-rfc4180-v1" {
		records, _, err = loadfile.ParseCSV(stream, receipt.Profile)
	} else {
		records, _, err = loadfile.ParseDAT(stream, receipt.Profile)
	}
	err = errors.Join(err, stream.Close())
	if err != nil {
		return result, err
	}
	if _, err := loadfile.ApplyMapping(records, receipt.Mapping, receipt.Profile, func(int64) error { return nil }); err != nil {
		return result, err
	}
	loadfile.NormalizeFileReferences(records, manifest.Volumes)
	if err := compareExportRecords(records, manifest.Records); err != nil || len(records) != receipt.RecordCount {
		return result, fmt.Errorf("verify exported load-file rows: %w", errors.Join(store.ErrPackageConflict, err))
	}
	if err := verifyLoadFileCustodianCrosswalk(manifest, crosswalk); err != nil {
		return result, err
	}
	if receipt.PageMap != "" {
		pageEntry := entries[receipt.PageMap]
		if pageEntry == nil {
			return result, loadfile.ErrMalformedInput
		}
		pageStream, err := pageEntry.Open()
		if err != nil {
			return result, fmt.Errorf("open exported page map: %w", err)
		}
		pageProfile, err := loadfile.ReadProfile(func() string {
			if strings.HasSuffix(receipt.PageMap, ".LFP") {
				return "lfp-ipro-v1"
			}
			return "opt-standard-v1"
		}())
		if err != nil {
			_ = pageStream.Close()
			return result, err
		}
		var images []loadfile.ImageRef
		if pageProfile.ID == "lfp-ipro-v1" {
			err = loadfile.ScanLFP(ctx, pageStream, pageProfile, func(image loadfile.ImageRef) error {
				images = append(images, image)
				return nil
			})
		} else {
			images, _, err = loadfile.ParseOPT(pageStream, pageProfile)
		}
		err = errors.Join(err, pageStream.Close())
		if err != nil || !sameExportImages(images, manifest.Images) || len(images) != receipt.PageCount {
			return result, loadfile.ErrMalformedInput
		}
	}
	section := io.NewSectionReader(source, 0, size)
	digest := sha256.New()
	written, err := io.Copy(digest, &contextReader{ctx: ctx, reader: section})
	if err != nil || written != size {
		return result, errors.Join(err, ctx.Err())
	}
	receipt.ArchiveSHA256 = hex.EncodeToString(digest.Sum(nil))
	receipt.Size = size
	return LoadFileExportResult{Receipt: receipt, Manifest: manifest, Crosswalk: crosswalk}, nil
}

func sameExportImages(actual, expected []loadfile.ImageRef) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		left, right := actual[index], expected[index]
		if left.ImageKey != right.ImageKey || left.Volume != right.Volume || left.RelPath != right.RelPath ||
			left.DocumentBreak != right.DocumentBreak || left.PageOrdinal != right.PageOrdinal {
			return false
		}
	}
	return true
}

func verifyLoadFileCustodianCrosswalk(manifest loadfile.Manifest, crosswalk LoadFileExportCrosswalk) error {
	if len(manifest.Records) != len(crosswalk.Members) {
		return loadfile.ErrMalformedInput
	}
	for index, member := range crosswalk.Members {
		custodianValue := ""
		for _, field := range manifest.Records[index].Fields {
			if field.Column == "CUSTODIAN" {
				custodianValue = field.Raw
				break
			}
		}
		claims := make(map[string]LoadFileExportCustodian, len(member.Custodians))
		for _, claim := range member.Custodians {
			if claim.AssignmentID == "" || claim.RawLabel == "" || claim.ScopeKind == "" || claim.Rank == "" ||
				claims[claim.AssignmentID].AssignmentID != "" {
				return loadfile.ErrMalformedInput
			}
			claims[claim.AssignmentID] = claim
		}
		primaryID := ""
		if member.PrimaryCustodian != nil {
			primaryID = member.PrimaryCustodian.AssignmentID
			claim, ok := claims[primaryID]
			if !ok || claim != *member.PrimaryCustodian || custodianValue != claim.RawLabel || claim.Rank != "primary" {
				return loadfile.ErrMalformedInput
			}
		} else if custodianValue != "" {
			return loadfile.ErrMalformedInput
		}
		omitted := make(map[string]bool, len(member.OmittedCustodianClaims))
		for _, assignmentID := range member.OmittedCustodianClaims {
			if assignmentID == primaryID || claims[assignmentID].AssignmentID == "" || omitted[assignmentID] {
				return loadfile.ErrMalformedInput
			}
			omitted[assignmentID] = true
		}
		expectedOmitted := len(claims)
		if primaryID != "" {
			expectedOmitted--
		}
		if len(omitted) != expectedOmitted {
			return loadfile.ErrMalformedInput
		}
	}
	return nil
}

func verifyLoadFileBatesArtifacts(
	ctx context.Context,
	entries map[string]*zip.File,
	receipt LoadFileExportReceipt,
	manifest loadfile.Manifest,
	crosswalk LoadFileExportCrosswalk,
) error {
	if receipt.BatesAllocationID == "" {
		if crosswalk.CombinedArtifact != nil {
			return loadfile.ErrMalformedInput
		}
		for _, member := range crosswalk.Members {
			if len(member.BatesPages) != 0 {
				return loadfile.ErrMalformedInput
			}
		}
		return nil
	}
	combined := crosswalk.CombinedArtifact
	if combined == nil || combined.RelPath != loadFileExportVolume+"/ARTIFACTS/"+receipt.BatesAllocationID+".pdf" ||
		combined.Size < 1 || combined.Pages != receipt.PageCount || combined.SHA256 == "" {
		return loadfile.ErrMalformedInput
	}
	combinedBytes, err := readZIPEntry(entries[combined.RelPath], 512<<20)
	if err != nil || int64(len(combinedBytes)) != combined.Size || sha256HexBytes(combinedBytes) != combined.SHA256 {
		return errors.Join(err, store.ErrPackageConflict)
	}
	combinedPages := make([]LoadFileExportBatesPage, 0, combined.Pages)
	for memberIndex, member := range crosswalk.Members {
		if len(member.BatesPages) == 0 {
			return loadfile.ErrMalformedInput
		}
		combinedPages = append(combinedPages, member.BatesPages...)
		roleIndex := slices.IndexFunc(member.Roles, func(role LoadFileExportRole) bool { return role.Role == "stamped_pdf" })
		if roleIndex < 0 {
			return loadfile.ErrMalformedInput
		}
		role := member.Roles[roleIndex]
		derivativeBytes, err := readZIPEntry(entries[loadFileExportVolume+"/"+role.RelPath], 512<<20)
		if err != nil || int64(len(derivativeBytes)) != role.Size || sha256HexBytes(derivativeBytes) != role.SHA256 {
			return errors.Join(err, store.ErrPackageConflict)
		}
		derivativePages := slices.Clone(member.BatesPages)
		slices.SortFunc(derivativePages, func(left, right LoadFileExportBatesPage) int {
			return left.DerivativeOutputPage - right.DerivativeOutputPage
		})
		labels := make([]pdfstamp.PageLabel, len(derivativePages))
		for index, page := range derivativePages {
			if page.DerivativeOutputPage != index+1 || page.ArtifactOutputPage < 1 || page.SourcePage < 1 || page.Label == "" {
				return loadfile.ErrMalformedInput
			}
			labels[index] = pdfstamp.PageLabel{SourcePage: page.SourcePage, Label: page.Label}
		}
		if memberIndex >= len(manifest.Records) ||
			exportRecordLabel(manifest.Records[memberIndex], "loadfile.label.assigned.begin") != derivativePages[0].Label ||
			exportRecordLabel(manifest.Records[memberIndex], "loadfile.label.assigned.end") != derivativePages[len(derivativePages)-1].Label {
			return loadfile.ErrMalformedInput
		}
		verified, err := pdfstamp.VerifyStamped(ctx, derivativeBytes, labels)
		if err != nil || verified.SHA256 != role.SHA256 || verified.PageCount != len(labels) {
			return errors.Join(err, store.ErrPackageConflict)
		}
	}
	if len(combinedPages) != combined.Pages {
		return loadfile.ErrMalformedInput
	}
	slices.SortFunc(combinedPages, func(left, right LoadFileExportBatesPage) int {
		return left.ArtifactOutputPage - right.ArtifactOutputPage
	})
	labels := make([]pdfstamp.PageLabel, len(combinedPages))
	for index, page := range combinedPages {
		if page.ArtifactOutputPage != index+1 || page.Label == "" {
			return loadfile.ErrMalformedInput
		}
		labels[index] = pdfstamp.PageLabel{SourcePage: page.SourcePage, Label: page.Label}
	}
	verified, err := pdfstamp.VerifyStamped(ctx, combinedBytes, labels)
	if err != nil || verified.SHA256 != combined.SHA256 || verified.PageCount != combined.Pages {
		return errors.Join(err, store.ErrPackageConflict)
	}
	return nil
}

func exportRecordLabel(record loadfile.Record, canonicalName string) string {
	for _, field := range record.Fields {
		if field.Canonical == canonicalName {
			return field.Raw
		}
	}
	return ""
}

func readZIPEntry(entry *zip.File, limit int64) ([]byte, error) {
	if entry == nil || limit < 0 || entry.UncompressedSize64 > uint64(limit) {
		return nil, loadfile.ErrMalformedInput
	}
	stream, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open load-file export ZIP entry: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(stream, limit+1))
	return data, errors.Join(readErr, stream.Close())
}

func compareExportRecords(left, right []loadfile.Record) error {
	if len(left) != len(right) {
		return fmt.Errorf("record count %d does not match %d", len(left), len(right))
	}
	for index := range left {
		if left[index].DocID != right[index].DocID || left[index].Family.ParentDocID != right[index].Family.ParentDocID ||
			left[index].Family.GroupID != right[index].Family.GroupID ||
			!slices.Equal(left[index].Family.AttachmentDocIDs, right[index].Family.AttachmentDocIDs) ||
			!slices.Equal(left[index].ColumnOrder, right[index].ColumnOrder) || len(left[index].Fields) != len(right[index].Fields) ||
			len(left[index].Files) != len(right[index].Files) {
			return fmt.Errorf("record %d identity, family, columns, or file count differs", index+1)
		}
		for field := range left[index].Fields {
			if left[index].Fields[field].Raw != right[index].Fields[field].Raw ||
				left[index].Fields[field].Canonical != right[index].Fields[field].Canonical {
				return fmt.Errorf("record %d field %d differs", index+1, field+1)
			}
		}
		for file := range left[index].Files {
			if left[index].Files[file].Role != right[index].Files[file].Role ||
				left[index].Files[file].Volume != right[index].Files[file].Volume ||
				left[index].Files[file].RelPath != right[index].Files[file].RelPath {
				return fmt.Errorf("record %d file %d differs", index+1, file+1)
			}
		}
	}
	return nil
}
