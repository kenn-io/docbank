package store

import (
	"context"
	"database/sql"
	"slices"
)

type photoXMPSourceVersion struct {
	Path      string
	Digest    string
	VersionID string
	Size      int64
}

// photoXMPSourceVersionsTx returns the latest bound version for each live
// outside path, ordered by its latest observation, newest first. Docbank's own
// sidecar writes are not outside sources.
func photoXMPSourceVersionsTx(ctx context.Context, q metadataQuerier, nodeID int64) ([]photoXMPSourceVersion, error) {
	result := []photoXMPSourceVersion{}
	seen := map[string]bool{}
	err := queryRows(ctx, q, `SELECT p.original_path,COALESCE(v.blob_hash,''),COALESCE(v.version_id,''),COALESCE(v.size,0) FROM provenance p
 JOIN ingests i ON i.id=p.ingest_id LEFT JOIN provenance_version_bindings b ON b.provenance_identity=p.identity
 LEFT JOIN content_versions v ON v.version_id=b.content_version_id AND v.node_id=p.node_id
 WHERE p.node_id=? AND EXISTS(SELECT 1 FROM photo_files pf WHERE pf.node_id=p.node_id AND pf.role='sidecar')
 AND i.source_kind NOT LIKE 'embedded:%' AND i.source_kind<>'photo-checkout'
 AND NOT EXISTS(SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
 ORDER BY b.observed_at DESC,v.node_revision DESC,p.identity DESC`, []any{nodeID}, func(rows *sql.Rows) error {
		var s photoXMPSourceVersion
		if err := rows.Scan(&s.Path, &s.Digest, &s.VersionID, &s.Size); err != nil {
			return err
		}
		s.Path = normalizePhotoSourcePath(s.Path)
		if !seen[s.Path] {
			seen[s.Path] = true
			result = append(result, s)
		}
		return nil
	})
	return result, err
}

// photoXMPHeadsTx lists the sources whose latest version is not the one the
// receipt holds a packet for, oldest observation first. The current version's
// heads go last, because the sidecar's bytes are theirs.
func photoXMPHeadsTx(ctx context.Context, q metadataQuerier, w PhotoXMPWork) ([]PhotoXMPHead, error) {
	sources, err := photoXMPSourceVersionsTx(ctx, q, w.Sidecar.Node.ID)
	if err != nil {
		return nil, err
	}
	entries := w.outside()
	var heads, current []PhotoXMPHead
	for _, s := range slices.Backward(sources) {
		own := photoXMPSourceIndex(entries, s.Path)
		if s.Digest == "" || own >= 0 && entries[own].VersionID == s.VersionID {
			continue
		}
		head := PhotoXMPHead{Path: s.Path, Digest: s.Digest, VersionID: s.VersionID, Size: s.Size}
		// A source with no packet of its own may hold bytes another source already supplied.
		if own < 0 {
			own = slices.IndexFunc(entries, func(e PhotoXMPSource) bool { return e.Digest == s.Digest })
		}
		if own >= 0 {
			head.Base = &entries[own].PhotoXMPSnapshot
		}
		if s.VersionID == w.Sidecar.Node.CurrentVersionID {
			current = append(current, head)
		} else {
			heads = append(heads, head)
		}
	}
	return append(heads, current...), nil
}

func photoXMPSourceUnchangedTx(ctx context.Context, tx *sql.Tx, node Node, member PhotoImportMember) (bool, string, error) {
	sources, err := photoXMPSourceVersionsTx(ctx, tx, node.ID)
	if err != nil {
		return false, "", err
	}
	for _, source := range sources {
		if source.Path == normalizePhotoSourcePath(member.OriginalPath) {
			return source.Digest == member.BlobHash, source.VersionID, nil
		}
	}
	return false, "", nil
}
