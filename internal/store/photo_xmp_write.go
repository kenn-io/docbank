package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// PhotoXMPPlan is the worker's merge decision for one original. Rows records
// a conflict and Refusal an invalid packet; otherwise Values and Keywords are
// the catalog after Import and Packet is the sidecar after Write.
type PhotoXMPPlan struct {
	Work      PhotoXMPWork
	Choice    string
	Extension string
	Refusal   string
	Rows      []PhotoXMPRow
	Sidecar   PhotoXMPPacket
	Packet    PhotoXMPPacket
	Values    PhotoAuthored
	Keywords  []string
	Import    bool
	Write     bool
}

// PublishPhotoXMP commits a plan's catalog import, sidecar version, receipt
// and status together, provided its inputs are still current.
func (s *Store) PublishPhotoXMP(ctx context.Context, plan PhotoXMPPlan, hash string, size int64, physical BlobPhysical) (result PhotoXMPState, err error) {
	w := plan.Work
	var run IngestRun
	if plan.Write && w.Sidecar == nil {
		if run, err = s.BeginCallerSuppliedIngest(ctx, "photo-xmp", "Photo XMP sidecar"); err != nil {
			return result, err
		}
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		current, err := loadPhotoXMPWorkTx(ctx, tx, w.FileID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current.inputs(), w.inputs()) {
			return ErrStaleRevision
		}
		if plan.Choice != "" {
			if err := s.photoAuthoredAccessTx(ctx, tx, w.AssetID); err != nil {
				return err
			}
			if current.state != PhotoXMPStateConflict || current.detail.ConflictID != w.detail.ConflictID {
				return ErrStaleRevision
			}
		}
		if err := s.publishPhotoXMPTx(ctx, tx, run, plan, hash, size, physical); err != nil {
			return err
		}
		f, err := photoFileByIDQuery(ctx, tx, w.FileID)
		if err != nil {
			return err
		}
		result, err = photoXMPStateTx(ctx, tx, f)
		return err
	})
	return result, err
}

func (s *Store) publishPhotoXMPTx(ctx context.Context, tx *sql.Tx, run IngestRun, plan PhotoXMPPlan, hash string, size int64, physical BlobPhysical) error {
	w, sc := plan.Work, plan.Work.Sidecar
	if plan.Refusal == "" && plan.Rows == nil && sc != nil {
		intended, err := photoXMPPairingTx(ctx, tx, sc.FileID, plan.Extension)
		if err != nil {
			return err
		}
		if intended != w.FileID {
			plan.Refusal = "The sidecar does not uniquely match this photo. Check its attachment or filename."
			if intended != "" {
				plan.Refusal = fmt.Sprintf("The sidecar names member %s. Run docbank photos assets detach %s %s, then docbank photos assets attach %s id:%d --role sidecar --sidecar-of-file-id %s.", intended, w.AssetID, sc.FileID, w.AssetID, sc.NodeID, intended)
			}
		}
	}
	if plan.Refusal != "" {
		return setPhotoXMPStateTx(ctx, tx, w.FileID, PhotoXMPStateInvalid, photoXMPDetail{Reason: plan.Refusal})
	}
	if plan.Rows != nil {
		id, err := newUUIDv4()
		if err != nil {
			return err
		}
		return setPhotoXMPStateTx(ctx, tx, w.FileID, PhotoXMPStateConflict, photoXMPDetail{Reason: "Catalog and sidecar decisions differ.", Rows: plan.Rows, ConflictID: id})
	}
	r := photoXMPReceipt{Version: 1, FileID: w.FileID, NodeID: w.NodeID, Extension: strings.ToLower(plan.Extension), OriginalVersionID: w.OriginalVersionID, Orientation: w.Orientation}
	var source *PhotoXMPSnapshot
	if sc != nil {
		r.Sidecar = PhotoSidecarProvenance{NodeID: sc.NodeID, VersionID: sc.VersionID, FileID: sc.FileID}
		if old := w.receipt; old != nil && old.Sidecar.FileID == sc.FileID {
			r.Source, r.SourceSHA256 = old.Source, old.SourceSHA256
		}
		accepted := sc.SourceSHA256
		if accepted == "" && r.Source == nil && (w.receipt == nil || w.receipt.Sidecar.FileID != sc.FileID) {
			// The first sync of a sidecar Docbank did not write accepts its bytes as outside,
			// so a later rescan of the same bytes cannot undo Docbank's next version.
			accepted = sc.SHA256
			if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET source_sha256=? WHERE file_id=?`, accepted, sc.FileID); err != nil {
				return err
			}
		}
		if accepted == sc.SHA256 && r.SourceSHA256 != sc.SHA256 {
			source = &PhotoXMPSnapshot{Packet: plan.Sidecar}
			r.Source, r.SourceSHA256 = source, sc.SHA256
		}
	}
	if plan.Import {
		receipt, err := s.importPhotoXMPTx(ctx, tx, w, plan.Values, plan.Keywords)
		if err != nil {
			return err
		}
		r.ImportReceiptID = receipt.ReceiptID
	}
	if plan.Write {
		published, err := s.writePhotoXMPSidecarTx(ctx, tx, run, w, hash, size, physical)
		if err != nil {
			return err
		}
		r.Sidecar = published
	}
	f, err := photoFileByIDQuery(ctx, tx, w.FileID)
	if err != nil {
		return err
	}
	keywords, err := photoKeywordNames(ctx, tx, w.NodeID)
	if err != nil {
		return err
	}
	r.Baseline = PhotoXMPSnapshot{Packet: plan.Packet, Catalog: f.Authored(), Keywords: keywords}
	if source != nil {
		source.Catalog, source.Keywords = r.Baseline.Catalog, keywords
	}
	// Only the latest successful sync is retained.
	if _, err := tx.ExecContext(ctx, `DELETE FROM photo_change_receipts WHERE receipt_id=? AND operation='xmp_sync'`, w.receiptID); err != nil {
		return err
	}
	id, err := newUUIDv4()
	if err != nil {
		return err
	}
	if err := writePhotoReceiptTx(ctx, tx, photoReceipt{ReceiptID: id, Operation: "xmp_sync", AfterRevision: 1, After: r}); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE photo_files SET xmp_state='synced',xmp_detail='',xmp_receipt_id=? WHERE file_id=?`, id, w.FileID)
	return err
}

// importPhotoXMPTx applies imported decisions and keywords as one undoable
// sidecar edit.
func (s *Store) importPhotoXMPTx(ctx context.Context, tx *sql.Tx, w PhotoXMPWork, values PhotoAuthored, keywords []string) (PhotoAuthoredReceipt, error) {
	f, err := photoFileByIDQuery(ctx, tx, w.FileID)
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	n, err := nodeByIDQuery(ctx, tx, f.NodeID)
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	edits, err := s.photoXMPKeywordEditsTx(ctx, tx, n.ID, keywords)
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	plan, err := planPhotoKeywordsTx(ctx, tx, edits, []BatchTagTarget{{NodeID: n.ID, Revision: n.Revision}})
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	loaded := map[string]photoAuthoredLoaded{f.ID: {f, n}}
	provenance := &PhotoSidecarProvenance{NodeID: w.Sidecar.NodeID, VersionID: w.Sidecar.VersionID, FileID: w.Sidecar.FileID}
	receipt, err := s.applyPhotoAuthoredValuesTx(ctx, tx, []PhotoAuthoredTarget{{FileID: f.ID, Revision: f.Revision, restore: &values}}, photoAuthoredApply{sidecar: provenance, force: true, loaded: loaded})
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
	if len(edits) > MaxPhotoXMPKeywords {
		return nil, ErrPhotoEditReceiptLimit
	}
	return edits, nil
}

// writePhotoXMPSidecarTx publishes a new version of the sidecar, or creates
// and attaches one beside the original.
func (s *Store) writePhotoXMPSidecarTx(ctx context.Context, tx *sql.Tx, run IngestRun, w PhotoXMPWork, hash string, size int64, physical BlobPhysical) (PhotoSidecarProvenance, error) {
	if sc := w.Sidecar; sc != nil {
		n, err := nodeByIDQuery(ctx, tx, sc.NodeID)
		if err != nil {
			return PhotoSidecarProvenance{}, err
		}
		published, _, err := s.replaceContentTx(ctx, tx, n, n.Revision, hash, size, "application/rdf+xml", physical)
		return PhotoSidecarProvenance{NodeID: n.ID, VersionID: published.CurrentVersionID, FileID: sc.FileID}, err
	}
	parent := s.RootID()
	if w.parentID != nil {
		parent = *w.parentID
	}
	written, _, _, err := s.ingestFileTx(ctx, tx, run, parent, w.name+".xmp", hash, size, "application/rdf+xml", "docbank:xmp:"+w.FileID, "", ingestFileOptions{exact: true, deferPhotoEnrollment: true}, physical)
	if err != nil {
		return PhotoSidecarProvenance{}, err
	}
	before, err := photoAssetByIDQuery(ctx, tx, w.AssetID)
	if err != nil {
		return PhotoSidecarProvenance{}, err
	}
	if err := s.insertPhotoFileTx(ctx, tx, PhotoFile{AssetID: w.AssetID, NodeID: written.Node.ID, Role: PhotoRoleSidecar, SidecarOfID: &w.FileID, CreatedAt: nowRFC3339()}); err != nil {
		return PhotoSidecarProvenance{}, err
	}
	if _, err := commitPhotoAssetTx(ctx, tx, before, before, "attach"); err != nil {
		return PhotoSidecarProvenance{}, err
	}
	var fileID string
	if err := tx.QueryRowContext(ctx, `SELECT file_id FROM photo_files WHERE node_id=?`, written.Node.ID).Scan(&fileID); err != nil {
		return PhotoSidecarProvenance{}, err
	}
	return PhotoSidecarProvenance{NodeID: written.Node.ID, VersionID: written.Node.CurrentVersionID, FileID: fileID}, nil
}
