package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"math"
	"slices"
)

type PhotoKeywordEdit struct {
	TagID  string `json:"tag_id" format:"uuid"`
	Assign bool   `json:"assign"`
}

type PhotoKeywordDelta struct {
	NodeID int64  `json:"node_id"`
	TagID  string `json:"tag_id"`
	Before bool   `json:"before"`
	After  bool   `json:"after"`
}

type PhotoKeywordNode struct {
	NodeID        int64 `json:"node_id"`
	AfterRevision int64 `json:"after_revision"`
}

type photoKeywordState struct {
	tag     Tag
	targets []batchTagTargetState
}

const maxPhotoKeywordEdits = 100

var ErrPhotoEditFileLimit = errors.New("edit at most 1,000 files at once; RAW/JPEG pairs count as two files. Select fewer photos")
var ErrPhotoEditReceiptLimit = errors.New("photo metadata receipt exceeds the 1 MiB limit")

type PhotoAuthoredEditTarget struct {
	AssetID          string `json:"asset_id" format:"uuid"`
	AssetRevision    int64  `json:"asset_revision" minimum:"1"`
	FileID           string `json:"file_id" format:"uuid"`
	Revision         int64  `json:"revision" minimum:"1"`
	NodeRevision     int64  `json:"node_revision" minimum:"1"`
	ContentVersionID string `json:"content_version_id,omitempty" format:"uuid"`
}

type photoAuthoredLoaded struct {
	file PhotoFile
	node Node
}

type PhotoPreparedTarget struct {
	PhotoAuthoredEditTarget

	DisplayFileID string        `json:"display_file_id" format:"uuid"`
	FileName      string        `json:"file_name"`
	NodeID        int64         `json:"node_id"`
	Values        PhotoAuthored `json:"values"`
	Tags          []string      `json:"tags"`
}

type PhotoAuthoredPreparation struct {
	Assets  []PhotoAsset          `json:"assets"`
	Targets []PhotoPreparedTarget `json:"targets"`
}

type PhotoAuthoredBatch struct {
	Targets       []PhotoAuthoredEditTarget
	Patch         PhotoAuthoredPatch
	RotationDelta *int
	Keywords      []PhotoKeywordEdit
}

func (s *Store) PreparePhotoAuthored(ctx context.Context, selection PhotoSetSelection) (PhotoAuthoredPreparation, error) {
	result := PhotoAuthoredPreparation{Assets: []PhotoAsset{}, Targets: []PhotoPreparedTarget{}}
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		ids, err := s.photoAuthoredSelectionIDs(ctx, tx, selection)
		if err != nil {
			return err
		}
		settings, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		preparer := photoAuthoredPreparer{tx: tx, hidden: selection.Hidden, single: len(ids) == 1, settings: settings, result: &result}
		for chunk := range slices.Chunk(ids, photoSelectionChunkSize) {
			if err := preparer.prepare(ctx, chunk); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

// photoAuthoredSelectionIDs resolves a metadata-edit selection and maps album
// selection errors to their photo-edit equivalents.
func (s *Store) photoAuthoredSelectionIDs(ctx context.Context, tx *sql.Tx, selection PhotoSetSelection) ([]string, error) {
	selection.Limit = maxBatchTagTargets
	ids, err := s.photoSetSelectionIDs(ctx, tx, selection)
	switch {
	case errors.Is(err, errPhotoSetTooManyAssets):
		return nil, ErrPhotoEditFileLimit
	case errors.Is(err, errPhotoSetSelector):
		return nil, fmt.Errorf("provide either asset_ids or query: %w", ErrInvalidPhotoAsset)
	case selection.Query == nil && !selection.Hidden && errors.Is(err, ErrHiddenLocked):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	if len(ids) == 0 && !selection.SkipUnavailable {
		return nil, ErrInvalidPhotoAsset
	}
	return ids, nil
}

// photoAuthoredPreparer accumulates edit targets for one PreparePhotoAuthored call.
type photoAuthoredPreparer struct {
	tx       *sql.Tx
	hidden   bool
	single   bool
	settings PhotoSettings
	result   *PhotoAuthoredPreparation
}

func (p photoAuthoredPreparer) prepare(ctx context.Context, ids []string) error {
	in, args := placeholders(len(ids)), anyArgs(ids)
	assets, err := p.loadAssets(ctx, in, args)
	if err != nil {
		return err
	}
	var count int
	if err := p.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_files f JOIN nodes n ON n.id=f.node_id WHERE f.role!='sidecar' AND n.trashed_at IS NULL AND f.asset_id IN (`+in+`)`, args...).Scan(&count); err != nil {
		return err
	}
	if len(p.result.Targets)+count > maxBatchTagTargets {
		return ErrPhotoEditFileLimit
	}
	err = queryRows(ctx, p.tx, `SELECT `+photoFileColumns+` FROM photo_files WHERE asset_id IN (`+in+`) ORDER BY file_id`, args, func(rows *sql.Rows) error {
		file, err := scanPhotoFile(rows)
		if err != nil {
			return err
		}
		asset := assets[file.AssetID]
		asset.Files = append(asset.Files, file)
		assets[file.AssetID] = asset
		return nil
	})
	if err != nil {
		return err
	}
	targets, err := p.loadTargets(ctx, in, args)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := p.appendAsset(id, assets[id], targets); err != nil {
			return err
		}
	}
	return nil
}

func (p photoAuthoredPreparer) loadAssets(ctx context.Context, in string, args []any) (map[string]PhotoAsset, error) {
	assets := make(map[string]PhotoAsset, len(args))
	err := queryRows(ctx, p.tx, `SELECT `+photoAssetColumns+` FROM photo_assets WHERE asset_id IN (`+in+`)`, args, func(rows *sql.Rows) error {
		asset, err := scanPhotoAsset(rows)
		if err != nil {
			return err
		}
		if (asset.HiddenAt != nil) != p.hidden {
			return ErrNotFound
		}
		asset.Files = []PhotoFile{}
		assets[asset.ID] = asset
		return nil
	})
	return assets, err
}

// loadTargets returns the live edit targets of the selected assets, keyed by file ID.
func (p photoAuthoredPreparer) loadTargets(ctx context.Context, in string, args []any) (map[string]PhotoPreparedTarget, error) {
	targets := map[string]PhotoPreparedTarget{}
	err := queryRows(ctx, p.tx, `SELECT f.file_id, n.name, n.revision, nt.tag_id FROM photo_files f JOIN nodes n ON n.id=f.node_id LEFT JOIN node_tags nt ON nt.node_id=n.id WHERE n.trashed_at IS NULL AND f.asset_id IN (`+in+`) ORDER BY f.file_id, nt.tag_id`, args, func(rows *sql.Rows) error {
		var id string
		var fileName string
		var revision int64
		var tag sql.NullString
		if err := rows.Scan(&id, &fileName, &revision, &tag); err != nil {
			return err
		}
		target, exists := targets[id]
		if !exists {
			target = PhotoPreparedTarget{FileName: fileName, NodeRevision: revision, Tags: []string{}}
		}
		if tag.Valid {
			target.Tags = append(target.Tags, tag.String)
		}
		targets[id] = target
		return nil
	})
	return targets, err
}

func (p photoAuthoredPreparer) appendAsset(id string, asset PhotoAsset, targets map[string]PhotoPreparedTarget) error {
	asset, err := completePhotoAsset(asset, p.settings)
	if err != nil {
		return err
	}
	if asset.DisplayFileID == nil {
		return fmt.Errorf("photo %s display member is unavailable; refresh your selection: %w", id, ErrStaleRevision)
	}
	if _, live := targets[*asset.DisplayFileID]; !live {
		return fmt.Errorf("photo %s display member is unavailable; refresh your selection: %w", id, ErrStaleRevision)
	}
	asset.Files = slices.DeleteFunc(asset.Files, func(file PhotoFile) bool { _, live := targets[file.ID]; return !live })
	asset.Agreement = photoAgreement(asset.Files)
	if p.single {
		p.result.Assets = append(p.result.Assets, asset)
	}
	for _, file := range asset.Files {
		if file.Role == PhotoRoleSidecar {
			continue
		}
		target, exists := targets[file.ID]
		if !exists {
			return ErrNotFound
		}
		target.DisplayFileID = *asset.DisplayFileID
		target.AssetID, target.AssetRevision, target.FileID, target.Revision, target.NodeID, target.Values = id, asset.Revision, file.ID, file.Revision, file.NodeID, file.Authored()
		p.result.Targets = append(p.result.Targets, target)
	}
	return nil
}

func (s *Store) EditPhotoAuthoredBatch(ctx context.Context, request PhotoAuthoredBatch) (PhotoAuthoredReceipt, error) {
	var result PhotoAuthoredReceipt
	if request.Patch == (PhotoAuthoredPatch{}) && request.RotationDelta == nil && len(request.Keywords) == 0 ||
		request.RotationDelta != nil && (*request.RotationDelta != 90 || request.Patch.Rotation != nil) ||
		len(request.Keywords) > maxPhotoKeywordEdits || len(request.Targets) == 0 {
		return result, ErrInvalidPhotoAsset
	}
	if len(request.Targets) > maxBatchTagTargets {
		return result, ErrPhotoEditFileLimit
	}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		targets, loaded, err := s.photoAuthoredBatchTargetsTx(ctx, tx, request)
		if err != nil {
			return err
		}
		result, err = s.applyPhotoAuthoredValuesTx(ctx, tx, targets, photoAuthoredApply{force: true, loaded: loaded})
		if err != nil {
			return err
		}
		nodes := map[int64]Node{}
		tagTargets := make([]BatchTagTarget, 0, len(loaded))
		for _, snapshot := range loaded {
			nodes[snapshot.node.ID] = snapshot.node
			tagTargets = append(tagTargets, BatchTagTarget{NodeID: snapshot.node.ID, Revision: snapshot.node.Revision})
		}
		slices.SortFunc(tagTargets, func(a, b BatchTagTarget) int { return cmp.Compare(a.NodeID, b.NodeID) })
		plan, err := planPhotoKeywordsTx(ctx, tx, request.Keywords, tagTargets)
		if err != nil {
			return err
		}
		return s.applyPhotoKeywordsTx(ctx, tx, &result, plan, nodes)
	})
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	return result, nil
}

// photoAuthoredBatchRows holds the rows an edit batch reads before checking its targets.
type photoAuthoredBatchRows struct {
	assets map[string]photoAuthoredAssetAccess
	files  map[string]PhotoFile
	nodes  map[int64]Node
}

type photoAuthoredAssetAccess struct {
	revision      int64
	hidden        bool
	displayFileID sql.NullString
}

// loadPhotoAuthoredBatchRowsTx reads every target's asset, file, and node in
// chunked queries. Missing rows are absent from the maps.
func loadPhotoAuthoredBatchRowsTx(ctx context.Context, tx *sql.Tx, targets []PhotoAuthoredEditTarget) (photoAuthoredBatchRows, error) {
	loaded := photoAuthoredBatchRows{assets: map[string]photoAuthoredAssetAccess{}, files: map[string]PhotoFile{}, nodes: map[int64]Node{}}
	assetIDs := make([]string, 0, len(targets))
	fileIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		assetIDs = append(assetIDs, target.AssetID)
		fileIDs = append(fileIDs, target.FileID)
	}
	err := queryInChunks(ctx, tx, `SELECT asset_id, revision, hidden_at IS NOT NULL, display_file_id FROM photo_assets WHERE asset_id IN (`, uniqueSorted(assetIDs), func(rows *sql.Rows) error {
		var id string
		var access photoAuthoredAssetAccess
		if err := rows.Scan(&id, &access.revision, &access.hidden, &access.displayFileID); err != nil {
			return err
		}
		loaded.assets[id] = access
		return nil
	})
	if err != nil {
		return loaded, err
	}
	nodeIDs := make([]int64, 0, len(targets))
	err = queryInChunks(ctx, tx, `SELECT `+photoFileLookupColumns+` FROM photo_files WHERE file_id IN (`, uniqueSorted(fileIDs), func(rows *sql.Rows) error {
		file, err := scanPhotoFile(rows)
		if err != nil {
			return err
		}
		loaded.files[file.ID] = file
		nodeIDs = append(nodeIDs, file.NodeID)
		return nil
	})
	if err != nil {
		return loaded, err
	}
	err = queryInChunks(ctx, tx, `SELECT `+nodeCols+` FROM `+nodeFrom+` WHERE n.id IN (`, uniqueSorted(nodeIDs), func(rows *sql.Rows) error {
		node, err := scanNode(rows)
		if err != nil {
			return err
		}
		loaded.nodes[node.ID] = node
		return nil
	})
	return loaded, err
}

// photoAuthoredBatchTargetsTx checks each requested target, in request order,
// against its current asset, file, and node revisions.
func (s *Store) photoAuthoredBatchTargetsTx(ctx context.Context, tx *sql.Tx, request PhotoAuthoredBatch) ([]PhotoAuthoredTarget, map[string]photoAuthoredLoaded, error) {
	rows, err := loadPhotoAuthoredBatchRowsTx(ctx, tx, request.Targets)
	if err != nil {
		return nil, nil, err
	}
	targets := make([]PhotoAuthoredTarget, 0, len(request.Targets))
	assets := map[string]photoAuthoredAssetAccess{}
	loaded := map[string]photoAuthoredLoaded{}
	unlocked := false
	for _, expected := range request.Targets {
		asset, checked := assets[expected.AssetID]
		if !checked {
			if validateUUIDv4(expected.AssetID) != nil {
				return nil, nil, ErrInvalidPhotoAsset
			}
			var exists bool
			if asset, exists = rows.assets[expected.AssetID]; !exists {
				return nil, nil, ErrNotFound
			}
			if asset.hidden && !unlocked {
				if _, err := s.hiddenSession(ctx, tx); err != nil {
					return nil, nil, err
				}
				unlocked = true
			}
			assets[expected.AssetID] = asset
		}
		if expected.NodeRevision < 1 || expected.AssetRevision < 1 {
			return nil, nil, ErrInvalidPhotoAsset
		}
		file, exists := rows.files[expected.FileID]
		if !exists {
			return nil, nil, ErrNotFound
		}
		node, exists := rows.nodes[file.NodeID]
		if !exists {
			return nil, nil, fmt.Errorf("node %d: %w", file.NodeID, ErrNotFound)
		}
		loaded[file.ID] = photoAuthoredLoaded{file, node}
		if file.AssetID != expected.AssetID || asset.revision != expected.AssetRevision || len(request.Keywords) > 0 && node.Revision != expected.NodeRevision {
			return nil, nil, ErrStaleRevision
		}
		if expected.ContentVersionID != "" && (asset.displayFileID.String != file.ID || node.CurrentVersionID != expected.ContentVersionID) {
			return nil, nil, ErrStaleRevision
		}
		patch := request.Patch
		if request.RotationDelta != nil {
			rotation := (file.Rotation + *request.RotationDelta) % 360
			patch.Rotation = &rotation
		}
		targets = append(targets, PhotoAuthoredTarget{FileID: file.ID, Revision: expected.Revision, Patch: patch})
	}
	return targets, loaded, nil
}

// photoKeywordPlan lists keyword changes sorted by tag and node, with each tag's
// changed targets in the same order.
type photoKeywordPlan struct {
	changes []PhotoKeywordDelta
	states  map[string]photoKeywordState
}

func planPhotoKeywordsTx(ctx context.Context, tx *sql.Tx, keywords []PhotoKeywordEdit, tagTargets []BatchTagTarget) (photoKeywordPlan, error) {
	plan := photoKeywordPlan{states: map[string]photoKeywordState{}}
	for _, keyword := range keywords {
		if validateUUIDv4(keyword.TagID) != nil {
			return plan, ErrInvalidPhotoAsset
		}
		if _, duplicate := plan.states[keyword.TagID]; duplicate {
			return plan, ErrInvalidPhotoAsset
		}
		tag, err := tagByIDTx(tx, keyword.TagID)
		if err != nil {
			return plan, err
		}
		states, err := loadBatchTagTargetsTx(ctx, tx, keyword.TagID, tagTargets)
		if err != nil {
			return plan, err
		}
		changed := []batchTagTargetState{}
		for _, state := range states {
			if state.assigned != keyword.Assign {
				plan.changes = append(plan.changes, PhotoKeywordDelta{state.node.ID, keyword.TagID, state.assigned, keyword.Assign})
				changed = append(changed, state)
			}
		}
		plan.states[keyword.TagID] = photoKeywordState{tag, changed}
	}
	return plan, nil
}

// photoKeywordGroups yields consecutive runs of changes that share a tag.
func photoKeywordGroups(changes []PhotoKeywordDelta) iter.Seq[[]PhotoKeywordDelta] {
	return func(yield func([]PhotoKeywordDelta) bool) {
		for start := 0; start < len(changes); {
			end := start + 1
			for end < len(changes) && changes[end].TagID == changes[start].TagID {
				end++
			}
			if !yield(changes[start:end]) {
				return
			}
			start = end
		}
	}
}

func (s *Store) applyPhotoKeywordsTx(ctx context.Context, tx *sql.Tx, receipt *PhotoAuthoredReceipt, plan photoKeywordPlan, nodes map[int64]Node) error {
	changes := plan.changes
	keywordNodes := map[int64]bool{}
	for _, change := range changes {
		keywordNodes[change.NodeID] = true
	}
	before, after := receipt.Before, receipt.After
	receipt.Before, receipt.After = []PhotoAuthoredSnapshot{}, []PhotoAuthoredSnapshot{}
	for i, snapshot := range after {
		if snapshot.Revision != before[i].Revision || keywordNodes[snapshot.NodeID] {
			receipt.Before = append(receipt.Before, before[i])
			receipt.After = append(receipt.After, snapshot)
		}
	}
	if len(changes) == 0 {
		if !photoAuthoredChanged(*receipt) {
			*receipt = PhotoAuthoredReceipt{Before: []PhotoAuthoredSnapshot{}, After: []PhotoAuthoredSnapshot{}}
			return nil
		}
		if err := checkPhotoAuthoredReceiptSize(*receipt); err != nil {
			return err
		}
		return persistPhotoAuthoredReceiptTx(ctx, tx, *receipt)
	}
	slices.SortFunc(changes, func(a, b PhotoKeywordDelta) int {
		if order := cmp.Compare(a.TagID, b.TagID); order != 0 {
			return order
		}
		return cmp.Compare(a.NodeID, b.NodeID)
	})
	fences := map[int64]PhotoKeywordNode{}
	for _, change := range changes {
		node, ok := nodes[change.NodeID]
		if !ok {
			return ErrInvalidPhotoAsset
		}
		fence, ok := fences[node.ID]
		if !ok {
			fence = PhotoKeywordNode{NodeID: node.ID, AfterRevision: node.Revision}
		}
		if fence.AfterRevision == math.MaxInt64 {
			return ErrInvalidPhotoAsset
		}
		fence.AfterRevision++
		fences[node.ID] = fence
	}
	receipt.Keywords = changes
	for _, fence := range fences {
		receipt.KeywordNodes = append(receipt.KeywordNodes, fence)
	}
	slices.SortFunc(receipt.KeywordNodes, func(a, b PhotoKeywordNode) int { return cmp.Compare(a.NodeID, b.NodeID) })
	if err := checkPhotoAuthoredReceiptSize(*receipt); err != nil {
		return err
	}
	for group := range photoKeywordGroups(changes) {
		prepared := plan.states[group[0].TagID]
		tag, states := prepared.tag, prepared.targets
		if len(states) != len(group) {
			return ErrInvalidPhotoAsset
		}
		for i, change := range group {
			if states[i].node.ID != change.NodeID || states[i].assigned != change.Before {
				return ErrStaleRevision
			}
			states[i].node = nodes[change.NodeID]
		}
		results, err := s.applyBatchTagsTx(ctx, tx, BatchTagRequest{TagID: tag.ID, Assign: group[0].After}, tag, states, nowRFC3339())
		if err != nil {
			return err
		}
		for _, result := range results {
			node := nodes[result.NodeID]
			node.Revision = result.Revision
			nodes[result.NodeID] = node
		}
	}
	return persistPhotoAuthoredReceiptTx(ctx, tx, *receipt)
}

// anyArgs converts query arguments for a variadic database call.
func anyArgs[T any](values []T) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

// uniqueSorted returns the distinct values in ascending order.
func uniqueSorted[T cmp.Ordered](values []T) []T {
	return slices.Compact(slices.Sorted(slices.Values(values)))
}

// queryRows runs query and calls scan once per result row.
func queryRows(ctx context.Context, q metadataQuerier, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// queryInChunks runs prefix followed by an IN list of at most
// photoSelectionChunkSize IDs and a closing parenthesis, once per chunk.
func queryInChunks[T any](ctx context.Context, q metadataQuerier, prefix string, ids []T, scan func(*sql.Rows) error) error {
	for chunk := range slices.Chunk(ids, photoSelectionChunkSize) {
		if err := queryRows(ctx, q, prefix+placeholders(len(chunk))+`)`, anyArgs(chunk), scan); err != nil {
			return err
		}
	}
	return nil
}

func validatePhotoKeywords(r PhotoAuthoredReceipt) error {
	if len(r.Keywords) > maxBatchTagTargets*100 || len(r.KeywordNodes) > len(r.After) || r.Sidecar != nil && len(r.Keywords) != 0 {
		return ErrInvalidPhotoAsset
	}
	nodes := map[int64]int{}
	for _, file := range r.After {
		nodes[file.NodeID] = 0
	}
	for i, change := range r.Keywords {
		if _, exists := nodes[change.NodeID]; !exists {
			return ErrInvalidPhotoAsset
		}
		if validateUUIDv4(change.TagID) != nil || change.Before == change.After {
			return ErrInvalidPhotoAsset
		}
		if i > 0 {
			prior := r.Keywords[i-1]
			if prior.TagID > change.TagID || prior.TagID == change.TagID && (prior.NodeID >= change.NodeID || prior.After != change.After) {
				return ErrInvalidPhotoAsset
			}
		}
		nodes[change.NodeID]++
	}
	for i, fence := range r.KeywordNodes {
		count := nodes[fence.NodeID]
		if count < 1 || fence.AfterRevision < 1 || i > 0 && r.KeywordNodes[i-1].NodeID >= fence.NodeID {
			return ErrInvalidPhotoAsset
		}
		delete(nodes, fence.NodeID)
	}
	for _, count := range nodes {
		if count != 0 {
			return ErrInvalidPhotoAsset
		}
	}
	return nil
}

func photoAuthoredChanged(receipt PhotoAuthoredReceipt) bool {
	for i, after := range receipt.After {
		if after.Revision != receipt.Before[i].Revision {
			return true
		}
	}
	return false
}
