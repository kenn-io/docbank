package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

const (
	packageStateAvailable          = "available"
	packageDirectionReceived       = "received"
	packageLabelProvenanceAssigned = "assigned"
)

// PackageImportConfig binds the background worker to one daemon-owned vault.
type PackageImportConfig struct {
	Catalog       *store.Store
	Blobs         *blob.Store
	Mutate        func(context.Context, func() error) error
	Owner         string
	LeaseDuration time.Duration
	IdleDelay     time.Duration
	OpenContainer func(context.Context, string, string) (io.ReaderAt, int64, error)
}

type PackageImportWorker struct{ cfg PackageImportConfig }

var errPackageSuppliedText = errors.New("package supplied-text publication failed")

type packageImportWork struct {
	SourceKind        string `json:"source_kind"`
	SourceLocator     string `json:"source_locator"`
	Into              string `json:"into"`
	Total             int    `json:"total"`
	AcceptPartial     bool   `json:"accept_partial"`
	IndexSuppliedText bool   `json:"index_supplied_text"`
}

type packageImportReceiptData struct {
	Member *store.CollectionSnapshotMember `json:"member,omitzero"`
	Gaps   []string                        `json:"gaps"`
}

type packageImageFile struct {
	Image loadfile.ImageRef
	File  loadfile.FileRef
}

type packageFrozenField struct {
	Field     loadfile.Field `json:"field"`
	Sensitive bool           `json:"sensitive"`
}

func NewPackageImportWorker(cfg PackageImportConfig) (*PackageImportWorker, error) {
	if cfg.Catalog == nil || cfg.Blobs == nil || cfg.Owner == "" || len(cfg.Owner) > 256 {
		return nil, errors.New("package import worker requires a catalog, blobs, and owner")
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 5 * time.Minute
	}
	if cfg.IdleDelay == 0 {
		cfg.IdleDelay = time.Second
	}
	if cfg.LeaseDuration < time.Second || cfg.LeaseDuration > time.Hour || cfg.IdleDelay < time.Millisecond {
		return nil, errors.New("invalid package import worker timing")
	}
	if cfg.Mutate == nil {
		cfg.Mutate = func(_ context.Context, fn func() error) error { return fn() }
	}
	return &PackageImportWorker{cfg: cfg}, nil
}

func (w *PackageImportWorker) Run(ctx context.Context) error {
	delay := w.cfg.IdleDelay
	for {
		more, err := w.ProcessOnce(ctx)
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			delay = w.cfg.IdleDelay
		}
		if err != nil || !more {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		if err != nil {
			// A released job is claimable again immediately; a persistent source or
			// storage fault must not re-extract the package once a second forever.
			delay = min(delay*2, w.cfg.LeaseDuration)
		}
	}
}

// ProcessOnce claims at most one admitted job. Failed local work releases its
// lease so a subsequent claim can resume from the last committed record.
func (w *PackageImportWorker) ProcessOnce(ctx context.Context) (more bool, err error) {
	job, err := w.cfg.Catalog.ClaimPackageImportJob(ctx, w.cfg.Owner, w.cfg.LeaseDuration)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, releaseErr := w.cfg.Catalog.ReleasePackageImportJob(releaseCtx, job.ID, job.Epoch, job.Token)
			if releaseErr != nil && !errors.Is(releaseErr, store.ErrPackageConflict) {
				err = errors.Join(err, releaseErr)
			}
		}
	}()
	claimCtx, cancel := context.WithCancel(ctx)
	renewed := make(chan error, 1)
	go func() {
		renewed <- w.keepPackageImportLease(claimCtx, job, cancel)
	}()
	err = w.processClaim(claimCtx, job)
	cancel()
	if renewErr := <-renewed; renewErr != nil && !errors.Is(renewErr, context.Canceled) {
		err = errors.Join(err, renewErr)
	}
	if err != nil && ctx.Err() == nil && terminalPackageImportError(err) {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		finishErr := w.cfg.Mutate(finishCtx, func() error {
			_, failErr := w.cfg.Catalog.FinishPackageImportJob(finishCtx, job.ID, job.Epoch, job.Token, "failed", "")
			return failErr
		})
		finishCancel()
		if finishErr != nil && !errors.Is(finishErr, store.ErrPackageConflict) {
			err = errors.Join(err, finishErr)
		}
	}
	return true, err
}

func terminalPackageImportError(err error) bool {
	return errors.Is(err, store.ErrPackageConflict) || errors.Is(err, loadfile.ErrLoadfileLimit) ||
		errors.Is(err, loadfile.ErrMalformedInput) || errors.Is(err, loadfile.ErrUnsafeReference) ||
		errors.Is(err, store.ErrMailboxConflict) || errors.Is(err, store.ErrNotFound) ||
		errors.Is(err, store.ErrNotDir) || errors.Is(err, errPackageSuppliedText)
}

func (w *PackageImportWorker) keepPackageImportLease(ctx context.Context,
	job store.PackageImportJob, cancel context.CancelFunc,
) error {
	interval := max(w.cfg.LeaseDuration/3, 100*time.Millisecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := w.cfg.Catalog.RenewPackageImportJob(ctx, job.ID, job.Epoch, job.Token,
				w.cfg.LeaseDuration); err != nil {
				cancel()
				return err
			}
		}
	}
}

func (w *PackageImportWorker) processClaim(ctx context.Context, job store.PackageImportJob) (retErr error) {
	pkg, err := w.cfg.Catalog.Package(ctx, job.PackageID)
	if err != nil {
		return err
	}
	if pkg.Direction != packageDirectionReceived || pkg.State != "importing" || pkg.IngestID == "" {
		return store.ErrPackageConflict
	}
	var work packageImportWork
	if err := json.Unmarshal(job.JobJSON, &work, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("decode admitted package work: %w", err)
	}
	if work.SourceLocator == "" || (work.SourceKind != "root" && work.SourceKind != "container") {
		return store.ErrPackageConflict
	}
	manifest, err := w.loadManifest(ctx, pkg)
	if err != nil {
		return err
	}
	if work.Total < 1 || work.Total > 100_000 || work.Total != len(manifest.Records) {
		return store.ErrPackageConflict
	}
	var profile loadfile.Profile
	if err := json.Unmarshal([]byte(pkg.ProfileJSON), &profile); err != nil {
		return fmt.Errorf("decode confirmed package profile: %w", err)
	}
	into := work.Into
	if into == "" {
		into = "/"
	}
	destination, err := w.cfg.Catalog.NodeByPath(ctx, into)
	if err != nil {
		return err
	}
	if !destination.IsDir() {
		return store.ErrNotDir
	}
	root, cleanup, err := w.sourceRoot(ctx, job.Owner, work)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	resolver, err := loadfile.NewResolver(ctx, root, manifest.Mapping.VolumeRoots)
	if err != nil {
		return err
	}
	defer func() { _ = resolver.Close() }()
	if err := verifyPackageVolumes(pkg, manifest, resolver, !work.AcceptPartial); err != nil {
		return err
	}
	if !work.AcceptPartial && !packageManifestComplete(manifest) {
		return fmt.Errorf("package declares missing files and the import did not accept gaps: %w", store.ErrPackageConflict)
	}
	run, err := w.cfg.Catalog.PackageIngestRun(ctx, pkg.PackageID)
	if err != nil {
		return err
	}
	volumes := make(map[string]loadfile.Volume, len(manifest.Volumes))
	for _, volume := range manifest.Volumes {
		volumes[volume.Name] = volume
	}
	for _, ref := range manifest.Files {
		if work.AcceptPartial && ref.Role != "raw_load_file" {
			continue
		}
		volume, ok := volumes[ref.Volume]
		if !ok {
			return store.ErrPackageConflict
		}
		if err := verifyPackageInputFile(ctx, resolver, volume, ref); err != nil {
			return err
		}
	}
	pagesByDocument, err := packageImageFiles(manifest)
	if err != nil {
		return err
	}
	keys := make(map[string]string, len(manifest.Records))
	for _, record := range manifest.Records {
		key, keyErr := store.PackageRecordKey(record.LoadFile, record.RowOrdinal, record.DocID)
		if keyErr != nil || keys[record.DocID] != "" {
			return store.ErrPackageConflict
		}
		keys[record.DocID] = key
	}
	familyIDs, err := packageFamilyIDs(pkg.PackageID, manifest.Records, keys, nil)
	if err != nil {
		return err
	}
	var packageDir store.Node
	if err := w.cfg.Mutate(ctx, func() error {
		var dirErr error
		packageDir, dirErr = w.cfg.Catalog.EnsureDir(ctx, destination.ID, pkg.PackageID)
		return dirErr
	}); err != nil {
		return err
	}
	committed, gaps := 0, 0
	committedDocuments := make(map[string]bool, len(manifest.Records))
	for index, record := range manifest.Records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := w.cfg.Catalog.RenewPackageImportJob(ctx, job.ID, job.Epoch, job.Token, w.cfg.LeaseDuration); err != nil {
			return err
		}
		key := keys[record.DocID]
		occurrence := store.PackageOccurrenceID(pkg.PackageID, key)
		receipt, err := w.cfg.Catalog.PackageImportHead(ctx, pkg.PackageID, key)
		if errors.Is(err, store.ErrNotFound) {
			receipt, err = w.importRecord(ctx, job, pkg, run, resolver, volumes, packageDir,
				record, manifest.Mapping, pagesByDocument[record.DocID], index, key, occurrence,
				keys, familyIDs[record.DocID], work.AcceptPartial, work.IndexSuppliedText, profile.Encoding)
		}
		if err != nil {
			return err
		}
		var data packageImportReceiptData
		if err := json.Unmarshal(receipt.ReceiptJSON, &data); err != nil {
			return fmt.Errorf("decode committed package receipt: %w", err)
		}
		gaps += len(data.Gaps)
		if receipt.State == "committed" {
			if data.Member == nil || data.Member.OccurrenceID != occurrence ||
				data.Member.ContentVersionID != receipt.ContentVersionID {
				return store.ErrPackageConflict
			}
			if err := w.ensureCustodian(ctx, pkg.PackageID, key, occurrence, record, manifest.Mapping); err != nil {
				return err
			}
			_, assigned := packageRecordLabels(pkg.PackageID, occurrence, receipt.ContentVersionID, record)
			pageProvenance, pageLabelSet := packagePageLabelAuthority(record, pagesByDocument[record.DocID])
			if pageProvenance == packageLabelProvenanceAssigned {
				assigned = append(assigned, packagePageLabels(pkg.PackageID, occurrence, receipt.ContentVersionID,
					pageProvenance, pageLabelSet, pagesByDocument[record.DocID])...)
			}
			if len(assigned) > 0 {
				if err := w.cfg.Mutate(ctx, func() error {
					return w.cfg.Catalog.AssignPackageLabels(ctx, pkg.PackageID, occurrence,
						receipt.ContentVersionID, assigned)
				}); err != nil {
					return err
				}
			}
			committed++
			committedDocuments[record.DocID] = true
		} else if len(data.Gaps) == 0 {
			gaps++
		}
	}
	if committed == 0 || gaps > 0 && !work.AcceptPartial {
		return store.ErrPackageConflict
	}
	familyIDs, err = packageFamilyIDs(pkg.PackageID, manifest.Records, keys, committedDocuments)
	if err != nil {
		return err
	}
	spool, err := os.CreateTemp("", "docbank-package-members-*.jsonl")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(spool.Name()); _ = spool.Close() }()
	recordsByDocument := make(map[string]loadfile.Record, len(manifest.Records))
	for _, record := range manifest.Records {
		recordsByDocument[record.DocID] = record
	}
	ordinal := 0
	familyOrder := make(map[string]int)
	for _, record := range manifest.Records {
		if !committedDocuments[record.DocID] {
			continue
		}
		receipt, err := w.cfg.Catalog.PackageImportHead(ctx, pkg.PackageID, keys[record.DocID])
		if err != nil {
			return err
		}
		var data packageImportReceiptData
		if err := json.Unmarshal(receipt.ReceiptJSON, &data); err != nil || data.Member == nil {
			return store.ErrPackageConflict
		}
		member := *data.Member
		if err := normalizeCommittedPackageFamily(&member, record, recordsByDocument,
			committedDocuments, keys, pkg.PackageID); err != nil {
			return err
		}
		member.FamilyID = familyIDs[record.DocID]
		member.SourceCollectionIDs = []string{pkg.IngestID}
		ordinal++
		member.Ordinal = ordinal
		familyOrder[member.FamilyID]++
		member.FamilyOrder = familyOrder[member.FamilyID]
		encoded, err := canonical.Marshal(&member)
		if err != nil {
			return err
		}
		if _, err := spool.Write(append(encoded, '\n')); err != nil {
			return err
		}
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	snapshotID := packageImportSnapshotID(pkg.PackageID)
	if err := w.cfg.Mutate(ctx, func() error {
		_, sealErr := w.cfg.Catalog.SealCollectionSnapshotStream(ctx,
			store.SnapshotSealHeader{SnapshotID: snapshotID, SourceCollectionIDs: []string{pkg.IngestID}}, spool)
		return sealErr
	}); err != nil {
		return err
	}
	state := "complete"
	if gaps > 0 {
		state = "partial"
	}
	return w.cfg.Mutate(ctx, func() error {
		_, finishErr := w.cfg.Catalog.FinishPackageImportJob(ctx, job.ID, job.Epoch, job.Token, state, snapshotID)
		return finishErr
	})
}

func (w *PackageImportWorker) loadManifest(ctx context.Context, pkg store.Package) (loadfile.Manifest, error) {
	stream, size, err := w.cfg.Blobs.OpenStreamContext(ctx, pkg.ManifestBlobSHA256)
	if err != nil {
		return loadfile.Manifest{}, err
	}
	if size <= 0 || size > 512<<20 {
		return loadfile.Manifest{}, errors.Join(loadfile.ErrLoadfileLimit, stream.Close())
	}
	manifest, readErr := loadfile.ReadManifestJSONL(stream, pkg.ManifestSHA256)
	if err := errors.Join(readErr, stream.Close()); err != nil {
		return loadfile.Manifest{}, err
	}
	if manifest.ProfileSHA256 != pkg.ProfileSHA256 || manifest.MappingSHA256 != pkg.MappingSHA256 {
		return loadfile.Manifest{}, store.ErrPackageConflict
	}
	return manifest, nil
}

func (w *PackageImportWorker) sourceRoot(ctx context.Context, owner string, work packageImportWork) (string, func() error, error) {
	if work.SourceKind == "root" {
		return work.SourceLocator, func() error { return nil }, nil
	}
	if w.cfg.OpenContainer == nil {
		return "", nil, store.ErrMailboxConflict
	}
	reader, size, err := w.cfg.OpenContainer(ctx, owner, work.SourceLocator)
	if err != nil {
		return "", nil, err
	}
	root, err := loadfile.ExtractZIP(ctx, reader, size)
	if err != nil {
		return "", nil, err
	}
	return root, func() error { return loadfile.RemoveExtractedZIP(root) }, nil
}

// verifyPackageVolumes compares the admitted volume bindings with the live
// source. A strict import also requires the source root inventory to match the
// preflight digest; a partial import tolerates files that vanished after
// preflight and records them as gaps.
func verifyPackageVolumes(pkg store.Package, manifest loadfile.Manifest, resolver *loadfile.Resolver, strict bool) error {
	if len(pkg.Volumes) != len(manifest.Volumes) {
		return store.ErrPackageConflict
	}
	rootDigest, err := resolver.RootDigest()
	if err != nil {
		return err
	}
	for index, volume := range manifest.Volumes {
		bound := pkg.Volumes[index]
		mapped := volume.DeclaredRoot
		if override := manifest.Mapping.VolumeRoots[volume.Name]; override != "" {
			mapped = override
		}
		if bound.Ordinal != volume.Ordinal || bound.VolumeName != volume.Name ||
			bound.DeclaredRoot != volume.DeclaredRoot || bound.MappedRoot != mapped {
			return store.ErrPackageConflict
		}
		if !strict {
			continue
		}
		binding, err := store.PackageVolumeRootBinding(rootDigest, mapped)
		if err != nil {
			return err
		}
		if binding != bound.ResolvedRootSHA256 {
			return fmt.Errorf("package source root %s changed after preflight: %w", volume.Name, store.ErrPackageConflict)
		}
	}
	return nil
}

func packageManifestComplete(manifest loadfile.Manifest) bool {
	for _, ref := range manifest.Files {
		if ref.Status != "available" {
			return false
		}
	}
	for _, record := range manifest.Records {
		for _, ref := range record.Files {
			if ref.Status != "available" {
				return false
			}
		}
	}
	return true
}

func packageImageFiles(manifest loadfile.Manifest) (map[string][]packageImageFile, error) {
	type fileKey struct{ volume, relPath string }
	verified := make(map[fileKey]loadfile.FileRef)
	for _, ref := range manifest.Files {
		if ref.Role == "page_image" {
			key := fileKey{ref.Volume, ref.RelPath}
			if previous, exists := verified[key]; exists && previous != ref {
				return nil, store.ErrPackageConflict
			}
			verified[key] = ref
		}
	}
	documentByKey := make(map[string]string, len(manifest.Records)*2)
	producedByDocument := make(map[string]map[fileKey]loadfile.FileRef, len(manifest.Records))
	for _, record := range manifest.Records {
		produced := make(map[fileKey]loadfile.FileRef)
		for _, ref := range record.Files {
			if ref.Role == "produced_pdf" {
				produced[fileKey{ref.Volume, ref.RelPath}] = ref
			}
		}
		producedByDocument[record.DocID] = produced
		for _, key := range append([]string{record.DocID}, packageBeginLabels(record)...) {
			if key == "" || documentByKey[key] != "" && documentByKey[key] != record.DocID {
				return nil, store.ErrPackageConflict
			}
			documentByKey[key] = record.DocID
		}
	}
	pages := make(map[string][]packageImageFile)
	document := ""
	for _, image := range manifest.Images {
		if image.DocumentBreak {
			document = documentByKey[image.ImageKey]
		}
		if document == "" || image.PageOrdinal < 1 {
			return nil, store.ErrPackageConflict
		}
		key := fileKey{image.Volume, image.RelPath}
		ref, ok := producedByDocument[document][key]
		if !ok {
			ref, ok = verified[key]
		}
		if !ok {
			return nil, store.ErrPackageConflict
		}
		pages[document] = append(pages[document], packageImageFile{Image: image, File: ref})
	}
	return pages, nil
}

func packageBeginLabels(record loadfile.Record) []string {
	var labels []string
	for _, field := range record.Fields {
		if (field.Canonical == "loadfile.label.begin" || field.Canonical == "loadfile.label.assigned.begin") && field.Raw != "" {
			labels = append(labels, field.Raw)
		}
	}
	return labels
}

// packageFamilyIDs joins parent trees named by the same group without adding
// parent edges. A nil committed set includes every declared record.
func packageFamilyIDs(packageID string, records []loadfile.Record, keys map[string]string, committed map[string]bool) (map[string]string, error) {
	parents := make(map[string]string, len(records))
	for _, record := range records {
		parents[record.DocID] = record.Family.ParentDocID
	}
	roots := make(map[string]string, len(records))
	representatives := make(map[string]string)
	for _, record := range records {
		if committed != nil && !committed[record.DocID] {
			continue
		}
		root := record.DocID
		seen := make(map[string]bool)
		for ancestor := record.DocID; parents[ancestor] != ""; {
			if seen[ancestor] || keys[parents[ancestor]] == "" {
				return nil, store.ErrPackageConflict
			}
			seen[ancestor] = true
			ancestor = parents[ancestor]
			if committed == nil || committed[ancestor] {
				root = ancestor
			}
		}
		roots[record.DocID] = root
		representatives[root] = root
	}
	resolve := func(root string) string {
		for representatives[root] != root {
			representatives[root] = representatives[representatives[root]]
			root = representatives[root]
		}
		return root
	}
	groups := make(map[string]string)
	for _, record := range records {
		if roots[record.DocID] == "" || record.Family.GroupID == "" {
			continue
		}
		root := resolve(roots[record.DocID])
		if previous := groups[record.Family.GroupID]; previous != "" {
			representatives[root] = resolve(previous)
		} else {
			groups[record.Family.GroupID] = root
		}
	}
	result := make(map[string]string, len(roots))
	for document, root := range roots {
		result[document] = store.PackageOccurrenceID(packageID, keys[resolve(root)])
	}
	return result, nil
}

func normalizeCommittedPackageFamily(member *store.CollectionSnapshotMember, record loadfile.Record,
	records map[string]loadfile.Record, committed map[string]bool, keys map[string]string, packageID string,
) error {
	member.ParentOccurrenceID = ""
	seen := map[string]bool{record.DocID: true}
	for ancestorID := record.Family.ParentDocID; ancestorID != ""; {
		if seen[ancestorID] || keys[ancestorID] == "" {
			return store.ErrPackageConflict
		}
		seen[ancestorID] = true
		if committed[ancestorID] {
			occurrence := store.PackageOccurrenceID(packageID, keys[ancestorID])
			if member.ParentOccurrenceID == "" {
				member.ParentOccurrenceID = occurrence
			}
		}
		ancestor, ok := records[ancestorID]
		if !ok {
			return store.ErrPackageConflict
		}
		ancestorID = ancestor.Family.ParentDocID
	}
	return nil
}

func verifyPackageInputFile(ctx context.Context, resolver *loadfile.Resolver,
	volume loadfile.Volume, ref loadfile.FileRef,
) error {
	if ref.Status != packageStateAvailable || !canonical.IsSHA256Hex(ref.SHA256) || ref.Size < 0 {
		return store.ErrPackageConflict
	}
	source, err := resolver.Open(volume, ref.RelPath)
	if err != nil {
		return fmt.Errorf("open admitted package file: %w", store.ErrPackageConflict)
	}
	digest := sha256.New()
	count, readErr := io.Copy(digest, mediaContextReader{ctx: ctx, r: source})
	closeErr := source.Close()
	if err := errors.Join(readErr, closeErr, ctx.Err()); err != nil {
		return err
	}
	if count != ref.Size || hex.EncodeToString(digest.Sum(nil)) != ref.SHA256 {
		return store.ErrPackageConflict
	}
	return nil
}

func packageImportSnapshotID(packageID string) string {
	sum := sha256.Sum256([]byte("package-import-snapshot/v1/" + packageID))
	var id uuid.UUID
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String()
}

func (w *PackageImportWorker) indexPackageSuppliedText(ctx context.Context, packageID,
	occurrenceID, encoding string, member *store.CollectionSnapshotMember,
) error {
	for index := range member.Representations {
		rep := &member.Representations[index]
		if rep.Role != "supplied_text" || rep.Status != packageStateAvailable {
			continue
		}
		if rep.ContentVersionID == "" {
			return store.ErrPackageConflict
		}
		sourceVersion, err := w.cfg.Catalog.ContentVersionByID(ctx, member.ContentVersionID)
		if err != nil {
			return err
		}
		textVersion, err := w.cfg.Catalog.ContentVersionByID(ctx, rep.ContentVersionID)
		if err != nil {
			return err
		}
		var generation string
		if err := w.cfg.Mutate(ctx, func() error {
			var publishErr error
			generation, publishErr = PublishPackageSuppliedText(ctx, w.cfg.Catalog, w.cfg.Blobs,
				packageID, occurrenceID, sourceVersion, textVersion, encoding)
			return publishErr
		}); err != nil {
			if errors.Is(err, store.ErrLexicalGenerationStale) {
				return err
			}
			return fmt.Errorf("%w: %w", errPackageSuppliedText, err)
		}
		if generation == "" {
			return store.ErrPackageConflict
		}
		rep.TextAuthority = "supplied"
		rep.LexicalGenerationID = generation
	}
	return nil
}

func (w *PackageImportWorker) importRecord(ctx context.Context, job store.PackageImportJob, pkg store.Package,
	run store.IngestRun, resolver *loadfile.Resolver, volumes map[string]loadfile.Volume,
	destination store.Node, record loadfile.Record, mapping loadfile.Mapping, pages []packageImageFile, ordinal int, key, occurrence string,
	keys map[string]string, familyID string, acceptPartial, indexSuppliedText bool, encoding string,
) (store.PackageImportReceipt, error) {
	raw, err := canonical.Marshal(record)
	if err != nil {
		return store.PackageImportReceipt{}, err
	}
	if len(raw) > 1<<20 {
		return store.PackageImportReceipt{}, loadfile.ErrLoadfileLimit
	}
	frozenFields := frozenPackageFields(record, mapping)
	frozenJSON, err := canonical.Marshal(frozenFields)
	if err != nil {
		return store.PackageImportReceipt{}, err
	}
	recordRow := store.PackageRecordRow{PackageID: pkg.PackageID, RowID: key,
		LoadFile: record.LoadFile, RowOrdinal: record.RowOrdinal, OccurrenceID: occurrence,
		RawJSON: raw, RawSHA256: renditionBytesSHA256(raw), Sensitive: packageRowSensitive(frozenFields)}
	member := store.CollectionSnapshotMember{Ordinal: ordinal + 1, OccurrenceID: occurrence,
		FamilyID: familyID, DisplayName: record.DocID,
		FrozenFieldsJSON: string(frozenJSON), DocumentKind: "file"}
	if record.Family.ParentDocID != "" {
		parentKey := keys[record.Family.ParentDocID]
		if parentKey == "" {
			return store.PackageImportReceipt{}, store.ErrPackageConflict
		}
		member.ParentOccurrenceID = store.PackageOccurrenceID(pkg.PackageID, parentKey)
	}
	var gaps []string
	sourceIsSuppliedText := false
	for index, ref := range record.Files {
		if ref.Role == "" {
			return store.PackageImportReceipt{}, store.ErrPackageConflict
		}
		representation := store.CollectionSnapshotRepresentation{OccurrenceID: occurrence,
			Role: ref.Role, Ordinal: index, Status: "omitted", TextAuthority: "none",
			VerifiedPageCount: ref.VerifiedPageCount}
		if ref.Status == "missing" {
			representation.Status = "missing"
			gaps = append(gaps, ref.Volume+"/"+ref.RelPath)
		} else if ref.Status == packageStateAvailable {
			volume, ok := volumes[ref.Volume]
			if !ok {
				return store.PackageImportReceipt{}, store.ErrPackageConflict
			}
			staged, absent, stageErr := w.stageFile(ctx, job, run, resolver, volume, destination,
				occurrence, index, ref)
			if stageErr != nil {
				return store.PackageImportReceipt{}, stageErr
			}
			if absent {
				representation.Status = "missing"
				gaps = append(gaps, ref.Volume+"/"+ref.RelPath)
			} else {
				representation.Status = packageStateAvailable
				representation.ContentVersionID = staged.Version.ID
				representation.BlobSHA256 = staged.Version.BlobHash
				representation.Size = staged.Version.Size
				representation.MediaType = staged.Version.MimeType
				if member.ContentVersionID == "" || ref.Role == "produced_pdf" && ref.VerifiedPageCount > 0 ||
					ref.Role == "native" && member.SourcePageCount == 0 {
					member.NodeID = staged.Node.ID
					member.ContentVersionID = staged.Version.ID
					member.BlobSHA256 = staged.Version.BlobHash
					member.Size = staged.Version.Size
					member.DocumentKind = packageImportDocumentKind(ref)
					sourceIsSuppliedText = ref.Role == "supplied_text"
					if ref.VerifiedPageCount > 0 && member.DocumentKind == "pdf" {
						member.SourcePageCount = ref.VerifiedPageCount
						member.SelectedPDFSHA256 = staged.Version.BlobHash
					}
				}
			}
		} else if ref.Status != "" {
			representation.Status = "unavailable"
			gaps = append(gaps, ref.Volume+"/"+ref.RelPath)
		}
		member.Representations = append(member.Representations, representation)
	}
	for index, page := range pages {
		ref := page.File
		if ref.Role == "produced_pdf" {
			continue
		}
		representation := store.CollectionSnapshotRepresentation{OccurrenceID: occurrence,
			Role: "page_image", Ordinal: len(record.Files) + index, PageNumber: page.Image.PageOrdinal,
			Status: "omitted", TextAuthority: "none"}
		if ref.Status == "missing" {
			representation.Status = "missing"
			gaps = append(gaps, ref.Volume+"/"+ref.RelPath)
		} else if ref.Status == packageStateAvailable {
			volume, ok := volumes[ref.Volume]
			if !ok {
				return store.PackageImportReceipt{}, store.ErrPackageConflict
			}
			staged, absent, stageErr := w.stageFile(ctx, job, run, resolver, volume, destination,
				occurrence, len(record.Files)+index, ref)
			if stageErr != nil {
				return store.PackageImportReceipt{}, stageErr
			}
			if absent {
				representation.Status = "missing"
				gaps = append(gaps, ref.Volume+"/"+ref.RelPath)
			} else {
				representation.Status = packageStateAvailable
				representation.ContentVersionID = staged.Version.ID
				representation.BlobSHA256 = staged.Version.BlobHash
				representation.Size = staged.Version.Size
				representation.MediaType = staged.Version.MimeType
				// A page image is the source when the only mapped file is
				// supplied text. Keep that text as a separate authority.
				if member.ContentVersionID == "" || sourceIsSuppliedText {
					member.NodeID = staged.Node.ID
					member.ContentVersionID = staged.Version.ID
					member.BlobSHA256 = staged.Version.BlobHash
					member.Size = staged.Version.Size
					member.DocumentKind = "image"
					sourceIsSuppliedText = false
				}
			}
		} else if ref.Status != "" {
			representation.Status = "unavailable"
			gaps = append(gaps, ref.Volume+"/"+ref.RelPath)
		}
		member.Representations = append(member.Representations, representation)
	}
	for _, role := range []string{"native", "produced_pdf", "supplied_text", "page_image"} {
		found := false
		for _, rep := range member.Representations {
			if rep.Role == role {
				found = true
				break
			}
		}
		if !found {
			member.Representations = append(member.Representations,
				store.CollectionSnapshotRepresentation{OccurrenceID: occurrence, Role: role,
					Status: "omitted", TextAuthority: "none"})
		}
	}
	if len(gaps) > 0 && !acceptPartial {
		return store.PackageImportReceipt{}, fmt.Errorf("package record %s has missing declared files: %w", record.DocID, store.ErrPackageConflict)
	}
	state := "committed"
	var contentVersionID string
	var frozen *store.CollectionSnapshotMember
	if member.ContentVersionID != "" {
		contentVersionID = member.ContentVersionID
		if indexSuppliedText {
			if err := w.indexPackageSuppliedText(ctx, pkg.PackageID, occurrence, encoding, &member); err != nil {
				return store.PackageImportReceipt{}, err
			}
		}
		frozen = &member
	} else {
		state = "rejected"
	}
	receiptJSON, err := canonical.Marshal(packageImportReceiptData{Member: frozen, Gaps: gaps})
	if err != nil {
		return store.PackageImportReceipt{}, err
	}
	receipt := store.PackageImportReceipt{ReceiptID: uuid.NewString(), PackageID: pkg.PackageID,
		RecordKey: key, OccurrenceID: occurrence, ContentVersionID: contentVersionID,
		State: state, ReceiptJSON: receiptJSON}
	if state == "rejected" {
		var rejected store.PackageImportReceipt
		err := w.cfg.Mutate(ctx, func() error {
			var gapErr error
			rejected, gapErr = w.cfg.Catalog.RecordPackageGapWithLease(ctx, job.ID, job.Epoch, job.Token, recordRow, receipt)
			return gapErr
		})
		return rejected, err
	}
	labels, _ := packageRecordLabels(pkg.PackageID, occurrence, contentVersionID, record)
	pageProvenance, pageLabelSet := packagePageLabelAuthority(record, pages)
	if pageProvenance == packageDirectionReceived {
		labels = append(labels, packagePageLabels(pkg.PackageID, occurrence, contentVersionID,
			pageProvenance, pageLabelSet, pages)...)
	}
	var committed store.PackageImportReceipt
	err = w.cfg.Mutate(ctx, func() error {
		var commitErr error
		committed, commitErr = w.cfg.Catalog.CommitPackageRecordWithLease(ctx, job.ID, job.Epoch,
			job.Token, recordRow, labels, receipt)
		return commitErr
	})
	if err != nil {
		return store.PackageImportReceipt{}, err
	}
	return committed, nil
}

func packagePageLabels(packageID, occurrence, contentVersionID, provenance, labelSet string,
	pages []packageImageFile,
) []store.PackageLabelRow {
	labels := make([]store.PackageLabelRow, 0, len(pages))
	for _, page := range pages {
		if page.Image.ImageKey == "" {
			continue
		}
		labels = append(labels, store.PackageLabelRow{PackageID: packageID, Provenance: provenance,
			LabelSet: labelSet, Label: page.Image.ImageKey, LabelSortKey: store.LabelSortKey(page.Image.ImageKey),
			OccurrenceID: occurrence, ContentVersionID: contentVersionID, PageNumber: page.Image.PageOrdinal,
			PageState: "unknown", Endpoint: "page"})
	}
	return labels
}

func packagePageLabelAuthority(record loadfile.Record, pages []packageImageFile) (string, string) {
	assignedSet, assignedBegin, assignedEnd := "assigned", "", ""
	for _, field := range record.Fields {
		switch field.Canonical {
		case "loadfile.label.assigned.set":
			if field.Raw != "" {
				assignedSet = field.Raw
			}
		case "loadfile.label.assigned.begin":
			assignedBegin = field.Raw
		case "loadfile.label.assigned.end":
			assignedEnd = field.Raw
		}
	}
	if len(pages) > 0 && assignedBegin != "" && assignedEnd != "" &&
		pages[0].Image.ImageKey == assignedBegin && pages[len(pages)-1].Image.ImageKey == assignedEnd {
		return packageLabelProvenanceAssigned, assignedSet
	}
	return packageDirectionReceived, packageDirectionReceived
}

func (w *PackageImportWorker) stageFile(ctx context.Context, job store.PackageImportJob, run store.IngestRun,
	resolver *loadfile.Resolver, volume loadfile.Volume, destination store.Node,
	occurrence string, ordinal int, ref loadfile.FileRef,
) (store.ContentWriteReceipt, bool, error) {
	source, err := resolver.Open(volume, ref.RelPath)
	if errors.Is(err, loadfile.ErrUnsafeReference) {
		return store.ContentWriteReceipt{}, true, nil
	}
	if err != nil {
		return store.ContentWriteReceipt{}, false, err
	}
	defer func() {
		if source != nil {
			_ = source.Close()
		}
	}()
	if ref.SHA256 == "" || ref.Size < 0 {
		return store.ContentWriteReceipt{}, false, store.ErrPackageConflict
	}
	name := fmt.Sprintf("%s-%02d-%s%s", occurrence, ordinal, ref.Role, path.Ext(ref.RelPath))
	original := ref.Volume + "/" + ref.RelPath
	mediaType := mime.TypeByExtension(strings.ToLower(path.Ext(ref.RelPath)))
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	var result store.ContentWriteReceipt
	err = w.cfg.Mutate(ctx, func() error {
		return w.cfg.Blobs.WithMutation(ctx, func() error {
			written, writeErr := w.cfg.Blobs.WriteDetailedContext(ctx, source)
			if writeErr != nil {
				return writeErr
			}
			if written.Hash != ref.SHA256 || written.Size != ref.Size {
				return fmt.Errorf("package source %s changed after preflight: %w", original, store.ErrPackageConflict)
			}
			encoding, encodeErr := written.EncodingName()
			if encodeErr != nil {
				return encodeErr
			}
			physical := store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize,
				PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created}
			result, writeErr = w.cfg.Catalog.IngestPackageFileWithReceipt(ctx, job, run, destination.ID,
				name, written.Hash, written.Size, mediaType, original, physical)
			if errors.Is(writeErr, store.ErrExists) {
				result, writeErr = w.recoverStagedFile(ctx, run.ID(), destination, name, original, ref)
			}
			return writeErr
		})
	})
	closeErr := source.Close()
	source = nil
	return result, false, errors.Join(err, closeErr)
}

func (w *PackageImportWorker) recoverStagedFile(ctx context.Context, ingestID string,
	destination store.Node, name, original string, ref loadfile.FileRef,
) (store.ContentWriteReceipt, error) {
	parentPath, err := w.cfg.Catalog.Path(ctx, destination.ID)
	if err != nil {
		return store.ContentWriteReceipt{}, err
	}
	node, err := w.cfg.Catalog.NodeByPath(ctx, path.Join(parentPath, name))
	if err != nil {
		return store.ContentWriteReceipt{}, err
	}
	if node.BlobHash != ref.SHA256 || node.Size != ref.Size || node.CurrentVersionID == "" {
		return store.ContentWriteReceipt{}, store.ErrPackageConflict
	}
	provenance, err := w.cfg.Catalog.NodeProvenance(ctx, node.ID, 1000, 0)
	if err != nil {
		return store.ContentWriteReceipt{}, err
	}
	bound := false
	for _, fact := range provenance.Items {
		if fact.IngestID == ingestID && fact.OriginalPath == original && fact.Active {
			bound = true
			break
		}
	}
	if !bound {
		return store.ContentWriteReceipt{}, store.ErrPackageConflict
	}
	version, err := w.cfg.Catalog.ContentVersionByID(ctx, node.CurrentVersionID)
	if err != nil {
		return store.ContentWriteReceipt{}, err
	}
	return store.ContentWriteReceipt{Node: node, Version: version}, nil
}

func packageImportDocumentKind(ref loadfile.FileRef) string {
	switch strings.ToLower(path.Ext(ref.RelPath)) {
	case ".pdf":
		return "pdf"
	case ".txt":
		return "text"
	default:
		return "file"
	}
}

func frozenPackageFields(record loadfile.Record, mapping loadfile.Mapping) []packageFrozenField {
	explicit := make(map[int]bool, len(mapping.Columns))
	for _, column := range mapping.Columns {
		if column.SourceOrdinal != nil && column.Sensitive {
			explicit[*column.SourceOrdinal] = true
		}
	}
	result := make([]packageFrozenField, len(record.Fields))
	for index, field := range record.Fields {
		result[index] = packageFrozenField{Field: field,
			Sensitive: loadfile.PackageFieldSensitive(field.Canonical, explicit[field.Ordinal])}
	}
	return result
}

func packageRowSensitive(fields []packageFrozenField) bool {
	for _, field := range fields {
		if field.Sensitive {
			return true
		}
	}
	return false
}

func packageRecordLabels(packageID, occurrence, versionID string, record loadfile.Record) ([]store.PackageLabelRow, []store.PackageLabelRow) {
	receivedSet, assignedSet := packageDirectionReceived, packageLabelProvenanceAssigned
	for _, field := range record.Fields {
		switch field.Canonical {
		case "loadfile.label.set":
			if field.Raw != "" {
				receivedSet = field.Raw
			}
		case "loadfile.label.assigned.set":
			if field.Raw != "" {
				assignedSet = field.Raw
			}
		}
	}
	var received, assigned []store.PackageLabelRow
	for _, field := range record.Fields {
		provenance, labelSet, endpoint := packageDirectionReceived, receivedSet, ""
		switch field.Canonical {
		case "loadfile.label.begin":
			endpoint = "begin"
		case "loadfile.label.end":
			endpoint = "end"
		case "loadfile.label.assigned.begin":
			provenance, labelSet, endpoint = packageLabelProvenanceAssigned, assignedSet, "begin"
		case "loadfile.label.assigned.end":
			provenance, labelSet, endpoint = packageLabelProvenanceAssigned, assignedSet, "end"
		}
		if endpoint == "" || field.Raw == "" {
			continue
		}
		label := store.PackageLabelRow{PackageID: packageID, Provenance: provenance,
			LabelSet: labelSet, Label: field.Raw, LabelSortKey: store.LabelSortKey(field.Raw),
			OccurrenceID: occurrence, ContentVersionID: versionID, PageState: "unknown", Endpoint: endpoint}
		if provenance == packageLabelProvenanceAssigned {
			assigned = append(assigned, label)
		} else {
			received = append(received, label)
		}
	}
	return received, assigned
}

func (w *PackageImportWorker) ensureCustodian(ctx context.Context, packageID, recordKey, occurrence string,
	record loadfile.Record, mapping loadfile.Mapping,
) error {
	var label string
	if mapping.CustodianColumn != "" {
		for _, field := range record.Fields {
			if field.Column == mapping.CustodianColumn {
				label = field.Raw
				break
			}
		}
	}
	if label == "" {
		label = mapping.DefaultCustodian
	}
	if label == "" {
		return nil
	}
	scope := store.CustodianScope{Kind: "package", PackageID: packageID,
		PackageRecordID: recordKey, HasPackageRecordID: true}
	assignments, _, err := w.cfg.Catalog.Custodians(ctx, scope, false, 250, 0)
	if err != nil {
		return err
	}
	sourceRef := packageID + "/" + occurrence
	for _, assignment := range assignments {
		if assignment.Rank == "primary" {
			if assignment.Basis == "package_column" && assignment.SourceRef == sourceRef && assignment.RawLabel == label {
				return nil
			}
			if assignment.Basis == "operator_assigned" {
				return nil
			}
			return store.ErrCustodianConflict
		}
	}
	return w.cfg.Mutate(ctx, func() error {
		_, setErr := w.cfg.Catalog.SetCustodian(ctx, store.CustodianRequest{Scope: scope,
			RawLabel: label, Rank: "primary", Basis: "package_column", SourceRef: sourceRef,
			IfMatchRevision: 1})
		return setErr
	})
}
