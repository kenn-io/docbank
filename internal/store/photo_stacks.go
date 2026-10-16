package store

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"crypto/sha256"
	"encoding/hex"

	"golang.org/x/text/unicode/norm"
)

const MaxPhotoStackFrames = 500

func photoStackTime(row PhotoBrowseRow) (time.Time, bool, bool) {
	f := row.Fields
	if row.Kind != "photo" || f.CaptureTime == nil || f.CaptureTimePrecision == nil || (*f.CaptureTimePrecision != "second" && *f.CaptureTimePrecision != "fraction") {
		return time.Time{}, false, false
	}
	value := *f.CaptureTime
	stamp, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return stamp, true, true
	}
	stamp, err = time.Parse("2006-01-02T15:04:05.999999999", value)
	return stamp, false, err == nil
}

func photoStackNeighbors(a, b PhotoBrowseRow, gap int) bool {
	if a.SourceFolderKey == "" || a.SourceFolderKey != b.SourceFolderKey {
		return false
	}
	first, firstZoned, ok := photoStackTime(a)
	if !ok {
		return false
	}
	second, secondZoned, ok := photoStackTime(b)
	if !ok || firstZoned != secondZoned {
		return false
	}
	delta := first.Sub(second)
	if delta < 0 {
		delta = -delta
	}
	return delta <= time.Duration(gap)*time.Second
}

// Provenance can come from a Windows backup. Import identities remain native;
// only absolute drive/UNC paths use portable separators for burst grouping.
func photoStackFolder(source string) string {
	portable := strings.ReplaceAll(source, `\`, "/")
	drive := len(portable) >= 3 && ((portable[0] >= 'A' && portable[0] <= 'Z') || (portable[0] >= 'a' && portable[0] <= 'z')) && portable[1:3] == ":/"
	unc := strings.HasPrefix(source, `\\`) && len(strings.Split(strings.TrimPrefix(portable, "//"), "/")) >= 3
	if drive || unc {
		return norm.NFC.String("windows:" + path.Dir(portable))
	}
	return norm.NFC.String(filepath.Dir(source))
}

func hydratePhotoStackFolders(ctx context.Context, q metadataQuerier, items []PhotoBrowseRow) error {
	byNode := map[int64][]int{}
	ids := make([]int64, 0, len(items))
	for i, row := range items {
		ids = append(ids, row.NodeID)
		byNode[row.NodeID] = append(byNode[row.NodeID], i)
	}
	folders := map[int64]string{}
	ambiguous := map[int64]bool{}
	err := queryInChunks(ctx, q, `SELECT p.node_id,p.original_path,i.source_kind,i.source_desc FROM provenance p JOIN ingests i ON i.id=p.ingest_id WHERE NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity) AND p.node_id IN (`, ids, func(rows *sql.Rows) error {
		var node int64
		var path, sourceKind, sourceName string
		if err := rows.Scan(&node, &path, &sourceKind, &sourceName); err != nil {
			return err
		}
		if path == "" {
			return nil
		}
		folder := photoStackFolder(path)
		if sourceKind == "push" || sourceKind == "watch" {
			folder = "source\x00" + sourceKind + "\x00" + sourceName + "\x00" + folder
		}
		if old, exists := folders[node]; exists && old != folder {
			ambiguous[node] = true
		}
		folders[node] = folder
		return nil
	})
	if err != nil {
		return err
	}
	assets := make([]string, 0, len(items))
	for _, row := range items {
		assets = append(assets, row.AssetID)
	}
	picked := map[string]bool{}
	if err := queryInChunks(ctx, q, `SELECT DISTINCT f.asset_id FROM photo_files f JOIN nodes n ON n.id=f.node_id WHERE f.role!='sidecar' AND n.trashed_at IS NULL AND f.flag='pick' AND f.asset_id IN (`, assets, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		picked[id] = true
		return nil
	}); err != nil {
		return err
	}
	for i := range items {
		items[i].Picked = picked[items[i].AssetID]
	}
	for node, indexes := range byNode {
		if !ambiguous[node] {
			for _, i := range indexes {
				if folder := folders[node]; folder != "" {
					digest := sha256.Sum256([]byte(folder))
					items[i].SourceFolderKey = hex.EncodeToString(digest[:])
				}
			}
		}
	}
	return nil
}

var ErrStalePhotoStack = errors.New("photo stack changed; refresh photos before deciding")

type PhotoStackDecision struct {
	Hidden      bool
	AssetIDs    []string
	Targets     []PhotoAuthoredEditTarget
	Decision    string
	CurrentID   string
	SelectedIDs []string
}

func (s *Store) DecidePhotoStack(ctx context.Context, request PhotoStackDecision) (PhotoAuthoredReceipt, error) {
	if len(request.AssetIDs) == 0 || len(request.AssetIDs) > MaxPhotoStackFrames || len(request.Targets) == 0 || len(request.Targets) > maxBatchTagTargets {
		return PhotoAuthoredReceipt{}, ErrInvalidPhotoAsset
	}
	if !slices.Contains([]string{"current", "all", "selected", "representative"}, request.Decision) || !slices.Contains(request.AssetIDs, request.CurrentID) || request.Decision == "selected" && len(request.SelectedIDs) == 0 {
		return PhotoAuthoredReceipt{}, ErrInvalidPhotoAsset
	}
	for _, id := range request.SelectedIDs {
		if !slices.Contains(request.AssetIDs, id) {
			return PhotoAuthoredReceipt{}, ErrInvalidPhotoAsset
		}
	}
	var result PhotoAuthoredReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		settings, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		// Resolve only the submitted members. The observed query or frozen run
		// defines membership; unseen neighboring imports remain untouched.
		rows, err := photoStackMembers(ctx, tx, request.AssetIDs, request.Hidden)
		if err != nil {
			return err
		}
		if len(rows) != len(request.AssetIDs) {
			return ErrStalePhotoStack
		}
		for i := 1; i < len(rows); i++ {
			if !photoStackNeighbors(rows[i-1], rows[i], settings.BurstGapSeconds) {
				return ErrStalePhotoStack
			}
		}
		// Prepare every live source file inside the same transaction. This also
		// prevents a client from omitting a sibling or one half of a RAW/JPEG pair.
		preparation := PhotoAuthoredPreparation{}
		preparer := photoAuthoredPreparer{tx: tx, hidden: request.Hidden, settings: settings, result: &preparation}
		if err := preparer.prepare(ctx, request.AssetIDs); err != nil {
			return err
		}
		if len(preparation.Targets) != len(request.Targets) {
			return ErrStalePhotoStack
		}
		expected := map[string]PhotoAuthoredEditTarget{}
		for _, target := range request.Targets {
			if _, duplicate := expected[target.FileID]; duplicate {
				return ErrInvalidPhotoAsset
			}
			expected[target.FileID] = target
		}
		for _, target := range preparation.Targets {
			wanted := expected[target.FileID]
			if wanted.ContentVersionID != "" {
				var current string
				if err := tx.QueryRowContext(ctx, `SELECT current_version_id FROM nodes WHERE id=?`, target.NodeID).Scan(&current); err != nil {
					return err
				}
				if current != wanted.ContentVersionID || target.DisplayFileID != target.FileID {
					return ErrStaleRevision
				}
				wanted.ContentVersionID = ""
			}
			if wanted != target.PhotoAuthoredEditTarget {
				return ErrStaleRevision
			}
		}
		flagged := map[string]bool{}
		for _, target := range preparation.Targets {
			if target.Values.Flag != "" {
				flagged[target.AssetID] = true
			}
		}
		targets := make([]PhotoAuthoredTarget, 0, len(preparation.Targets))
		for _, target := range preparation.Targets {
			flag, apply := "", false
			switch request.Decision {
			case "current":
				if target.AssetID == request.CurrentID {
					flag, apply = "pick", true
				} else if !flagged[target.AssetID] {
					flag, apply = "reject", true
				}
			case "all":
				flag, apply = "pick", true
			case "selected":
				if slices.Contains(request.SelectedIDs, target.AssetID) {
					flag, apply = "pick", true
				} else if !flagged[target.AssetID] {
					flag, apply = "reject", true
				}
			case "representative":
				if target.AssetID == request.CurrentID {
					flag, apply = "pick", true
				} else if target.Values.Flag == "pick" {
					apply = true
				}
			}
			if apply {
				targets = append(targets, PhotoAuthoredTarget{FileID: target.FileID, Revision: target.Revision, Patch: PhotoAuthoredPatch{Flag: new(flag)}})
			}
		}
		result, err = s.applyPhotoAuthoredValuesTx(ctx, tx, targets, photoAuthoredApply{persist: true})
		return err
	})
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	return result, nil
}

// Read a bounded observed stack in caller order, respecting Hidden-library scope.
func photoStackMembers(ctx context.Context, q metadataQuerier, ids []string, hidden bool) ([]PhotoBrowseRow, error) {
	byID := make(map[string]PhotoBrowseRow, len(ids))
	if err := queryInChunks(ctx, q, `SELECT a.asset_id,a.kind,n.id,p.capture_time,p.capture_time_precision,a.hidden_at FROM `+photoBrowseDisplayFrom+` WHERE n.trashed_at IS NULL AND a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var row PhotoBrowseRow
		var hiddenAt sql.NullString
		if err := rows.Scan(&row.AssetID, &row.Kind, &row.NodeID, &row.Fields.CaptureTime, &row.Fields.CaptureTimePrecision, &hiddenAt); err != nil {
			return err
		}
		if hidden == hiddenAt.Valid {
			byID[row.AssetID] = row
		}
		return nil
	}); err != nil {
		return nil, err
	}
	result := make([]PhotoBrowseRow, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, ErrInvalidPhotoAsset
		}
		seen[id] = true
		if row, ok := byID[id]; ok {
			result = append(result, row)
		}
	}
	if err := hydratePhotoStackFolders(ctx, q, result); err != nil {
		return nil, err
	}
	return result, nil
}
