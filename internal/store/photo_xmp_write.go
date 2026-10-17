package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// PhotoXMPPlan is the worker's merge decision for one original. Rows records
// a conflict and Refusal a blocked sync; otherwise Values and Keywords are
// the catalog after Import and Packet is the sidecar after Write.
type PhotoXMPPlan struct {
	Work         PhotoXMPWork
	Choice       string
	Extension    string
	Refusal      string
	RefusalState string
	Rows         []PhotoXMPRow
	Sidecar      PhotoXMPPacket
	Packet       PhotoXMPPacket
	Values       PhotoAuthored
	Keywords     []string
	Import       bool
	Write        bool
}

// PublishPhotoXMP commits a plan's catalog import, sidecar version, receipt
// and settled fence together, provided its inputs are still current. When the
// first sync of an imported sidecar finds that its claim names another
// member, it moves the sidecar there instead and returns that member's file ID.
func (s *Store) PublishPhotoXMP(ctx context.Context, plan PhotoXMPPlan, hash string, size int64, physical BlobPhysical) (retargeted string, err error) {
	w := plan.Work
	var run IngestRun
	if plan.Write && w.Sidecar == nil {
		if run, err = s.BeginCallerSuppliedIngest(ctx, "photo-xmp", "Photo XMP sidecar"); err != nil {
			return "", err
		}
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		current, err := loadPhotoXMPWorkTx(ctx, tx, w.Original.ID)
		if err != nil {
			return err
		}
		if plan.Choice != "" {
			if err := s.photoAuthoredAccessTx(ctx, tx, w.Original.AssetID); err != nil {
				return err
			}
			if w.receipt == nil {
				return ErrStaleRevision
			}
			if err := current.conflict(w.receipt.ConflictID); err != nil {
				return err
			}
		}
		if !reflect.DeepEqual(current, w) {
			return ErrStaleRevision
		}
		retargeted, err = s.publishPhotoXMPTx(ctx, tx, run, plan, hash, size, physical)
		return err
	})
	return retargeted, err
}

func (s *Store) publishPhotoXMPTx(ctx context.Context, tx *sql.Tx, run IngestRun, plan PhotoXMPPlan, hash string, size int64, physical BlobPhysical) (string, error) {
	w, sc := plan.Work, plan.Work.Sidecar
	if plan.Refusal == "" && plan.Rows == nil && sc != nil {
		intended, err := w.pairing(ctx, tx, plan.Extension)
		if err != nil {
			return "", err
		}
		if intended != w.Original.ID {
			if moved, err := s.retargetPhotoXMPSidecarTx(ctx, tx, w, intended); err != nil || moved {
				return intended, err
			}
			plan.Refusal = "The sidecar does not uniquely match this photo. Check its attachment or filename."
			if intended != "" {
				plan.Refusal = fmt.Sprintf("The sidecar names member %s. Run docbank photos assets detach %s %s, then docbank photos assets attach %s id:%d --role sidecar --sidecar-of-file-id %s.", intended, w.Original.AssetID, sc.FileID, w.Original.AssetID, sc.Node.ID, intended)
			}
		}
	}
	if plan.Refusal != "" {
		return "", settlePhotoXMPTx(ctx, tx, w, photoXMPReceipt{State: cmp.Or(plan.RefusalState, PhotoXMPStateInvalid), Reason: plan.Refusal})
	}
	if plan.Rows != nil {
		return "", settlePhotoXMPTx(ctx, tx, w, photoXMPReceipt{State: PhotoXMPStateConflict, Reason: "Catalog and sidecar decisions differ.", Rows: plan.Rows})
	}
	r := photoXMPSync{Extension: strings.ToLower(plan.Extension), OriginalVersionID: w.Node.CurrentVersionID, Orientation: w.Orientation}
	if sc != nil {
		r.Sidecar = PhotoSidecarProvenance{NodeID: sc.Node.ID, VersionID: sc.Node.CurrentVersionID, FileID: sc.FileID}
		if old := w.synced(); old != nil {
			r.Source, r.SourceSHA256, r.ImportReceiptID = old.Source, old.SourceSHA256, old.ImportReceiptID
		}
		accepted := sc.Observed
		if accepted == "" && w.synced() == nil {
			// The first sync of a sidecar Docbank did not write accepts its bytes as outside,
			// so a later rescan of the same bytes cannot undo Docbank's next version.
			accepted = sc.Node.BlobHash
		}
		if accepted == sc.Node.BlobHash && r.SourceSHA256 != sc.Node.BlobHash {
			r.Source = &PhotoXMPSnapshot{Packet: plan.Sidecar, Catalog: plan.Values, Keywords: plan.Keywords}
			r.SourceSHA256 = sc.Node.BlobHash
		}
	}
	if plan.Import {
		receipt, err := s.importPhotoXMPTx(ctx, tx, w, plan.Values, plan.Keywords)
		if err != nil {
			return "", err
		}
		r.ImportReceiptID = receipt.ReceiptID
	}
	if plan.Write {
		published, err := s.writePhotoXMPSidecarTx(ctx, tx, run, w, hash, size, physical)
		if err != nil {
			return "", err
		}
		r.Sidecar = published
	}
	r.Baseline = PhotoXMPSnapshot{Packet: plan.Packet, Catalog: plan.Values, Keywords: plan.Keywords}
	return "", settlePhotoXMPTx(ctx, tx, w, photoXMPReceipt{State: PhotoXMPStateSynced, Sync: &r})
}

// retargetPhotoXMPSidecarTx moves a sidecar that import attached to the
// member its claim names, before the sidecar's first sync. The move records an
// import photo receipt, so it keeps import's authority over the attachment.
// Neither original settles; both sync again.
func (s *Store) retargetPhotoXMPSidecarTx(ctx context.Context, tx *sql.Tx, w PhotoXMPWork, intended string) (bool, error) {
	sc := w.Sidecar
	if intended == "" {
		return false, nil
	}
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM photo_change_receipts INDEXED BY photo_change_receipts_xmp_sidecar
 WHERE operation='xmp_sync' AND json_extract(after_json,'$.sync.sidecar.node_id')=?1 AND json_extract(after_json,'$.sync.sidecar.file_id')=?2)
 AND COALESCE((SELECT operation='import' FROM photo_change_receipts r WHERE asset_id=?3
 AND EXISTS(SELECT 1 FROM json_each(r.after_json,'$.member_changes') c WHERE json_extract(c.value,'$.file_id')=?2)
 ORDER BY after_revision DESC LIMIT 1),0)
 AND EXISTS(SELECT 1 FROM photo_files t JOIN nodes tn ON tn.id=t.node_id WHERE t.file_id=?4 AND t.asset_id=?3 AND t.role IN ('raw','image') AND tn.trashed_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM photo_files o JOIN nodes onode ON onode.id=o.node_id WHERE o.sidecar_of_file_id=?4 AND onode.trashed_at IS NULL)`,
		sc.Node.ID, sc.FileID, w.Original.AssetID, intended).Scan(&eligible)
	if err != nil || !eligible {
		return false, err
	}
	target, err := photoFileByIDQuery(ctx, tx, intended)
	if err != nil {
		return false, err
	}
	for _, node := range []int64{sc.Node.ID, target.NodeID} {
		if refused, err := auditRefusesAttachmentChangeTx(ctx, tx, node); err != nil || refused {
			return false, err
		}
	}
	asset, err := photoAssetByIDQuery(ctx, tx, w.Original.AssetID)
	if err != nil {
		return false, err
	}
	_, err = s.mutatePhotoAssetTx(ctx, tx, asset.ID, asset.Revision, "import", func(tx *sql.Tx, _ *PhotoAsset) (bool, error) {
		_, err := tx.ExecContext(ctx, `UPDATE photo_files SET sidecar_of_file_id=? WHERE file_id=?`, intended, sc.FileID)
		return true, err
	})
	return err == nil, err
}

// importPhotoXMPTx applies imported decisions and keywords as one undoable
// sidecar edit.
func (s *Store) importPhotoXMPTx(ctx context.Context, tx *sql.Tx, w PhotoXMPWork, values PhotoAuthored, keywords []string) (PhotoAuthoredReceipt, error) {
	f, n := w.Original, w.Node
	edits, err := s.photoXMPKeywordEditsTx(ctx, tx, n.ID, keywords)
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	plan, err := planPhotoKeywordsTx(ctx, tx, edits, []BatchTagTarget{{NodeID: n.ID, Revision: n.Revision}})
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	loaded := map[string]photoAuthoredLoaded{f.ID: {f, n}}
	provenance := &PhotoSidecarProvenance{NodeID: w.Sidecar.Node.ID, VersionID: w.Sidecar.Node.CurrentVersionID, FileID: w.Sidecar.FileID}
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
	return PhotoSidecarProvenance{NodeID: written.Node.ID, VersionID: written.Node.CurrentVersionID, FileID: fileID}, nil
}
