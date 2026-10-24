package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// PhotoXMPPlan is the worker's merge decision for one original. Conflicts records
// a disagreement and Refusal a blocked sync; otherwise Values and Keywords are
// the catalog after Import and Packet is the sidecar after Write.
type PhotoXMPPlan struct {
	Work      PhotoXMPWork
	Choice    string
	Refusal   string
	State     string
	Conflicts PhotoAuthoredFields
	// ConflictPath is the outside source that caused the conflict, empty for Docbank's own versions.
	ConflictPath string
	Outside      []PhotoXMPSource
	Packet       PhotoXMPPacket
	Values       PhotoAuthored
	Keywords     []string
	// Suppliers are the sidecar versions that supplied an imported value, first one first.
	Suppliers []string
	// Fold is what the steps before a conflict left, kept in the conflict receipt.
	Fold   *photoXMPFold
	Import bool
	Write  bool
}

// PublishPhotoXMP commits a plan's catalog import, sidecar version, receipt
// and settled fence together, provided its inputs are still current.
func (s *Store) PublishPhotoXMP(ctx context.Context, plan PhotoXMPPlan, hash string, size int64, physical BlobPhysical) (imported *PhotoAuthoredReceipt, err error) {
	w := plan.Work
	var run IngestRun
	if plan.Write && w.Sidecar == nil {
		if run, err = s.BeginCallerSuppliedIngest(ctx, "photo-xmp", "Photo XMP sidecar"); err != nil {
			return nil, err
		}
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := checkPhotoXMPWorkTx(ctx, tx, w); err != nil {
			return err
		}
		if plan.Choice != "" {
			if err := s.photoAuthoredAccessTx(ctx, tx, w.Original.AssetID); err != nil {
				return err
			}
		}
		var err error
		imported, err = s.publishPhotoXMPTx(ctx, tx, run, plan, hash, size, physical)
		return err
	})
	// A person's choice reports the refusal and leaves the conflict open.
	if errors.Is(err, ErrPhotoEditReceiptLimit) && plan.Choice == "" {
		imported = nil
		err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
			if err := checkPhotoXMPWorkTx(ctx, tx, w); err != nil {
				return err
			}
			return settlePhotoXMPTx(ctx, tx, w, photoXMPReceipt{State: PhotoXMPStateInvalid, Reason: "The XMP sync history exceeds its size limit. Detach this sidecar and attach a different XMP file to start a new history."})
		})
	}
	if err != nil {
		return nil, err
	}
	return imported, nil
}

// checkPhotoXMPWorkTx compares the shared input token and current receipt
// identity, including a settlement by another worker with unchanged inputs.
func checkPhotoXMPWorkTx(ctx context.Context, tx *sql.Tx, w PhotoXMPWork) error {
	var token, receiptID string
	err := tx.QueryRowContext(ctx, `SELECT token,COALESCE(receipt_id,'') FROM photo_xmp_inputs WHERE file_id=?`, w.Original.ID).Scan(&token, &receiptID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleRevision
	}
	if err != nil {
		return err
	}
	if token != w.token || receiptID != w.receiptID {
		return ErrStaleRevision
	}
	return nil
}

func (s *Store) publishPhotoXMPTx(ctx context.Context, tx *sql.Tx, run IngestRun, plan PhotoXMPPlan, hash string, size int64, physical BlobPhysical) (*PhotoAuthoredReceipt, error) {
	var imported *PhotoAuthoredReceipt
	w, sc := plan.Work, plan.Work.Sidecar
	if plan.Refusal != "" || plan.State != "" {
		return nil, settlePhotoXMPTx(ctx, tx, w, photoXMPReceipt{State: cmp.Or(plan.State, PhotoXMPStateInvalid), Reason: boundPhotoXMPReason(plan.Refusal)})
	}
	if plan.Conflicts != 0 {
		var keywords []string
		if plan.Conflicts&PhotoXMPKeywordsBit != 0 {
			keywords = plan.Packet.Keywords
		}
		return nil, settlePhotoXMPTx(ctx, tx, w, photoXMPReceipt{State: PhotoXMPStateConflict, Conflicts: plan.Conflicts, ConflictPath: plan.ConflictPath, ConflictFileID: sc.FileID, Fold: plan.Fold, ConflictKeywords: keywords, ConflictValues: new(PhotoXMPOverlay(PhotoAuthored{}, plan.Packet.PhotoAuthored, plan.Conflicts))})
	}
	r := photoXMPSync{}
	if old := w.History; old != nil {
		r.ImportReceiptID, r.WrittenVersion = old.ImportReceiptID, old.WrittenVersion
	}
	if sc != nil {
		r.Sidecar = PhotoSidecarProvenance{NodeID: sc.Node.ID, VersionID: sc.Node.CurrentVersionID, FileID: sc.FileID}
		var err error
		if r.Outside, err = livePhotoXMPSourcesTx(ctx, tx, sc.Node.ID, plan.Outside); err != nil {
			return nil, err
		}
	}

	if plan.Import {
		receipt, err := s.importPhotoXMPTx(ctx, tx, w, plan)
		if err != nil {
			return nil, err
		}
		r.ImportReceiptID = receipt.ReceiptID
		imported = &receipt
	}
	if plan.Write {
		published, err := s.writePhotoXMPSidecarTx(ctx, tx, run, w, hash, size, physical)
		if err != nil {
			return nil, err
		}
		r.Sidecar = published
		r.WrittenVersion = published.VersionID
	}
	digest := hash
	if !plan.Write {
		digest = sc.Node.BlobHash
	}
	r.Settled = PhotoXMPSnapshot{Digest: digest, VersionID: r.Sidecar.VersionID, SourceOrientation: w.Orientation, Packet: plan.Packet.keepReadable(w.History), Catalog: plan.Values, Keywords: plan.Keywords}

	var ignored []string
	for _, field := range photoAuthoredFields {
		if plan.Packet.Ignored&field.bit != 0 && (!plan.Packet.LegacyReject || field.bit != PhotoConfirmedRating) {
			ignored = append(ignored, field.name)
		}
	}
	reason := ""
	if len(ignored) > 0 {
		reason = "Unsupported XMP values left unchanged: " + strings.Join(ignored, ", ") + ". Edit these fields in Metadata to replace them."
	}
	return imported, settlePhotoXMPTx(ctx, tx, w, photoXMPReceipt{State: PhotoXMPStateSynced, Reason: reason, Sync: &r})
}

// keepReadable gives each field this packet could not read the value of the last
// settled packet that could, so a later removal compares with a real ancestor.
func (p PhotoXMPPacket) keepReadable(old *photoXMPSync) PhotoXMPPacket {
	if old == nil {
		return p
	}
	return p.keepUnreadable(old.Settled.Packet)
}

// keepUnreadable retains comparison evidence for properties this packet cannot
// read. A rating of -1 supplies new rejection evidence while keeping the rating.
func (p PhotoXMPPacket) keepUnreadable(base PhotoXMPPacket) PhotoXMPPacket {
	keep := p.Ignored &^ base.Ignored
	p.PhotoAuthored = PhotoXMPOverlay(p.PhotoAuthored, base.PhotoAuthored, keep)
	p.Present = p.Present&^keep | base.Present&keep
	if p.Ignored&PhotoConfirmedRating != 0 && !p.LegacyReject {
		p.LegacyReject = base.LegacyReject
	}
	if keep&PhotoConfirmedRotation != 0 {
		p.Rotation, p.Orientation = base.Rotation, base.Orientation
	}
	p.Ignored &^= keep
	return p
}

// livePhotoXMPSourcesTx drops packets of sources the sidecar no longer has.
func livePhotoXMPSourcesTx(ctx context.Context, tx *sql.Tx, nodeID int64, sources []PhotoXMPSource) ([]PhotoXMPSource, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	live, err := photoXMPSourceVersionsTx(ctx, tx, nodeID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(sources), func(s PhotoXMPSource) bool {
		return !slices.ContainsFunc(live, func(l photoXMPSourceVersion) bool { return l.Path == s.Path })
	}), nil
}

// CheckPhotoXMPPairing validates the packet against its saved attachment. Publication
// checks the work's freshness, so the pairing read does not.
func (s *Store) CheckPhotoXMPPairing(ctx context.Context, w PhotoXMPWork, extension string) error {
	return s.photoReadTx(ctx, func(tx *sql.Tx) error {
		intended, err := photoSidecarIntendedTarget(ctx, tx, PhotoFile{Role: PhotoRoleSidecar, AssetID: w.Original.AssetID, SidecarOfID: &w.Original.ID}, w.Sidecar.Node, extension)
		if err != nil {
			return err
		}
		if intended != w.Original.ID {
			return errPhotoXMPUnpaired
		}
		return nil
	})
}

var errPhotoXMPUnpaired = fmt.Errorf("%w: the sidecar does not uniquely match this original; correct its attachment or filename", ErrInvalidPhotoAsset)

// CheckPhotoXMPClaim refuses a source copy whose extension claim names a different original. The
// copy is read against the original's retained source filenames in its own folder, or in any
// folder when the original was never imported beside it, so a rename in the vault changes nothing.
func (s *Store) CheckPhotoXMPClaim(ctx context.Context, w PhotoXMPWork, sourcePath, extension string) error {
	if extension == "" {
		return nil
	}
	names := []string{}
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		folder := strings.ToLower(path.Dir(normalizePhotoSourcePath(sourcePath)))
		var local []string
		err := queryRows(ctx, tx, `SELECT p.original_path FROM provenance p JOIN ingests i ON i.id=p.ingest_id
 WHERE p.node_id=? AND i.source_kind NOT LIKE 'embedded:%' AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)`, []any{w.Node.ID}, func(rows *sql.Rows) error {
			var source string
			if err := rows.Scan(&source); err != nil {
				return err
			}
			source = normalizePhotoSourcePath(source)
			names = append(names, source)
			if strings.ToLower(path.Dir(source)) == folder {
				local = append(local, source)
			}
			return nil
		})
		if len(local) > 0 {
			names = local
		}
		return err
	})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		names = []string{w.Node.Name}
	}
	if !slices.ContainsFunc(names, func(name string) bool { return photoSidecarExtensionAgrees(name, extension) }) {
		return errPhotoXMPUnpaired
	}
	return nil
}

// importPhotoXMPTx applies imported decisions and keywords as one undoable
// sidecar edit.
func (s *Store) importPhotoXMPTx(ctx context.Context, tx *sql.Tx, w PhotoXMPWork, imp PhotoXMPPlan) (PhotoAuthoredReceipt, error) {
	f, n := w.Original, w.Node
	edits, err := s.photoXMPKeywordEditsTx(ctx, tx, n.ID, imp.Keywords)
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	plan, err := planPhotoKeywordsTx(ctx, tx, edits, []BatchTagTarget{{NodeID: n.ID, Revision: n.Revision}})
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	loaded := map[string]photoAuthoredLoaded{f.ID: {f, n}}
	provenance := &PhotoSidecarProvenance{NodeID: w.Sidecar.Node.ID, VersionID: imp.Suppliers[0], FileID: w.Sidecar.FileID}
	receipt, err := s.applyPhotoAuthoredValuesTx(ctx, tx, []PhotoAuthoredTarget{{FileID: f.ID, Revision: f.Revision, restore: &imp.Values}}, photoAuthoredApply{sidecar: provenance, force: true, loaded: loaded})
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	err = s.applyPhotoKeywordsTx(ctx, tx, &receipt, plan, map[int64]Node{n.ID: loaded[f.ID].node})
	return receipt, err
}

// photoXMPKeywordEditsTx turns a keyword set into tag edits, creating tags
// that do not exist yet.
func (s *Store) photoXMPKeywordEditsTx(ctx context.Context, tx *sql.Tx, nodeID int64, names []string) ([]PhotoKeywordEdit, error) {
	current := map[string]string{}
	err := queryRows(ctx, tx, `SELECT t.id,t.name FROM node_tags nt JOIN tags t ON t.id=nt.tag_id WHERE nt.node_id=?`, []any{nodeID}, func(rows *sql.Rows) error {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		current[name] = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	edits := []PhotoKeywordEdit{}
	for _, name := range names {
		if _, exists := current[name]; exists {
			delete(current, name)
			continue
		}
		var id string
		err = tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE name=?`, name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			if id, err = newUUIDv4(); err != nil {
				return nil, err
			}
			err = s.createTagTx(ctx, tx, Tag{ID: id, Name: name, Revision: 1})
		}
		if err != nil {
			return nil, err
		}
		edits = append(edits, PhotoKeywordEdit{TagID: id, Assign: true})
	}
	for _, id := range current {
		edits = append(edits, PhotoKeywordEdit{TagID: id, Assign: false})
	}
	return edits, nil
}

// writePhotoXMPSidecarTx publishes a new version of the sidecar, or creates
// and attaches one beside the original.
func (s *Store) writePhotoXMPSidecarTx(ctx context.Context, tx *sql.Tx, run IngestRun, w PhotoXMPWork, hash string, size int64, physical BlobPhysical) (PhotoSidecarProvenance, error) {
	if sc := w.Sidecar; sc != nil {
		published, _, err := s.replaceContentTx(ctx, tx, sc.Node, sc.Node.Revision, hash, size, "application/rdf+xml", physical)
		return PhotoSidecarProvenance{NodeID: sc.Node.ID, VersionID: published.CurrentVersionID, FileID: sc.FileID}, err
	}
	parent := s.RootID()
	if w.Node.ParentID != nil {
		parent = *w.Node.ParentID
	}
	written, _, _, err := s.ingestFileTx(ctx, tx, run, parent, w.Node.Name+".xmp", hash, size, "application/rdf+xml", "docbank:xmp:"+w.Original.ID, "", ingestFileOptions{exact: true, deferPhotoEnrollment: true}, physical)
	if err != nil {
		return PhotoSidecarProvenance{}, err
	}
	before, err := photoAssetByIDQuery(ctx, tx, w.Original.AssetID)
	if err != nil {
		return PhotoSidecarProvenance{}, err
	}
	if err := s.insertPhotoFileTx(ctx, tx, PhotoFile{AssetID: w.Original.AssetID, NodeID: written.Node.ID, Role: PhotoRoleSidecar, SidecarOfID: &w.Original.ID, CreatedAt: nowRFC3339()}); err != nil {
		return PhotoSidecarProvenance{}, err
	}
	if _, err := commitPhotoAssetTx(ctx, tx, before, before, "attach"); err != nil {
		return PhotoSidecarProvenance{}, err
	}
	var fileID string
	if err := tx.QueryRowContext(ctx, `SELECT file_id FROM photo_files WHERE node_id=?`, written.Node.ID).Scan(&fileID); err != nil {
		return PhotoSidecarProvenance{}, err
	}
	return PhotoSidecarProvenance{NodeID: written.Node.ID, VersionID: written.Node.CurrentVersionID, FileID: fileID}, validatePhotoAssetGraph(ctx, tx, before.ID)
}
