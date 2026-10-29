package store

import (
	"context"
	"database/sql"
	"slices"

	"go.kenn.io/docbank/document"
)

// PhotoBestShot is a computed offer until the owner confirms a flag.
type PhotoBestShot = document.PhotoBestShotProposal

type PhotoBestShotResponse struct {
	AssetID  string `json:"asset_id" format:"uuid"`
	Flag     string `json:"flag" enum:"pick,reject"`
	Response string `json:"response" enum:"accepted,dismissed"`
}

// bestShotsTx reuses the prepared files for a bounded observed stack.
func (s *Store) bestShotsTx(ctx context.Context, tx *sql.Tx, ids []string, prep PhotoAuthoredPreparation, loaded map[string]photoAuthoredLoaded) ([]PhotoBestShot, error) {
	versions := map[string]string{}
	var versionIDs []string
	for _, t := range prep.Targets {
		if t.Values.Confirmed&PhotoConfirmedFlag != 0 && t.Values.Flag == "pick" {
			return nil, nil
		}
		if t.FileID == t.DisplayFileID {
			versions[t.AssetID] = t.CurrentContentVersionID
			versionIDs = append(versionIDs, t.CurrentContentVersionID)
		}
	}
	quality, err := photoQualityForVersions(ctx, tx, versionIDs)
	if err != nil {
		return nil, err
	}
	frames := make([]document.PhotoBestShotFrame, 0, len(ids))
	for _, id := range ids {
		frame := document.PhotoBestShotFrame{AssetID: id}
		confirmed := false
		for _, t := range prep.Targets {
			if t.AssetID == id {
				confirmed = confirmed || t.Values.Confirmed&PhotoConfirmedFlag != 0
				frame.Raw = frame.Raw || loaded[t.FileID].file.Role == "raw"
			}
		}
		if confirmed {
			continue
		}
		if q, ok := quality.signals[versions[id]]; ok && !quality.unavailable[versions[id]] && !quality.staleFaces[versions[id]] {
			frame.Signals = document.PhotoBestShotSignals(q)
		}
		frames = append(frames, frame)
	}
	return document.PlanPhotoBestShots(frames), nil
}

// PhotoStackBestShots enriches a complete read, publishing nothing on failure.
func (s *Store) PhotoStackBestShots(ctx context.Context, stacks []PhotoStack, hidden bool) error {
	offers := make([][]PhotoBestShot, len(stacks))
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		settings, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		for i, stack := range stacks {
			prep := PhotoAuthoredPreparation{}
			loaded := map[string]photoAuthoredLoaded{}
			p := photoAuthoredPreparer{tx: tx, hidden: hidden, settings: settings, result: &prep, limit: MaxPhotoStackTargets, loaded: loaded}
			if err := p.prepare(ctx, stack.AssetIDs); err != nil {
				return err
			}
			offers[i], err = s.bestShotsTx(ctx, tx, stack.AssetIDs, prep, loaded)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		for i := range stacks {
			stacks[i].Proposals = offers[i]
		}
	}
	return err
}

// respondPhotoBestShotsTx shares membership and complete-file revision checks.
func (s *Store) respondPhotoBestShotsTx(ctx context.Context, tx *sql.Tx, request PhotoStackDecision, prep PhotoAuthoredPreparation, loaded map[string]photoAuthoredLoaded) (PhotoAuthoredReceipt, error) {
	result := PhotoAuthoredReceipt{Before: []PhotoAuthoredSnapshot{}, After: []PhotoAuthoredSnapshot{}}
	offers, err := s.bestShotsTx(ctx, tx, request.AssetIDs, prep, loaded)
	if err != nil {
		return result, err
	}
	if len(offers) != len(request.Responses) {
		return result, ErrStalePhotoStack
	}
	seen := map[string]bool{}
	targets := []PhotoAuthoredTarget{}
	for _, response := range request.Responses {
		if request.Decision != "all" || response.Response != request.Responses[0].Response || seen[response.AssetID] || !slices.Contains([]string{"accepted", "dismissed"}, response.Response) {
			return result, ErrInvalidPhotoAsset
		}
		seen[response.AssetID] = true
		if !slices.ContainsFunc(offers, func(p PhotoBestShot) bool { return p.AssetID == response.AssetID && p.Flag == response.Flag }) {
			return result, ErrStalePhotoStack
		}
		for _, t := range prep.Targets {
			if t.AssetID == response.AssetID {
				flag := t.Values.Flag
				if response.Response == "accepted" {
					flag = response.Flag
				}
				targets = append(targets, PhotoAuthoredTarget{FileID: t.FileID, Revision: t.Revision, Patch: PhotoAuthoredPatch{Flag: new(flag)}})
			}
		}
	}
	return s.applyPhotoStackFlagsTx(ctx, tx, targets, loaded, false)
}
