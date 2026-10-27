package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/vectorindex"
)

// MaxPhotoStackFrames bounds a stack to one decision request. A longer
// connected run stays as individual photos rather than a misleading partial stack.
const MaxPhotoStackFrames = 500

// MaxPhotoStackTargets bounds every live file revision in one stack decision.
const MaxPhotoStackTargets = 2_000

// MaxPhotoStackGroupIDs bounds one explicit-ID grouping request to a Cull run.
const MaxPhotoStackGroupIDs = 250_000

const (
	photoStackImageWindowSeconds    = 60
	photoStackImageSimilarityCutoff = 0.95
)

const photoStackKeyLayout = "2006-01-02T15:04:05.000000000"

var ErrStalePhotoStack = errors.New("photo stack changed; refresh photos before deciding")

// PhotoStack is one burst of at least two frames in ascending capture order.
type PhotoStack struct {
	AssetIDs         []string
	RepresentativeID string
}

// photoStackFrame is one live asset's grouping input. Folder is empty when the
// photo cannot join a stack.
type photoStackFrame struct {
	ImageVectors     map[string][]float32
	AssetID          string
	Folder           string
	CameraMake       string
	CameraModel      string
	BodySerialNumber string
	SortKey          string
	Rank             int64
	Picked           bool
	Rejected         bool
	Files            int
	Stale            bool
	// Block is the clock hour photoStackBlocks restarts a long run at.
	Block string
}

// applyMetadataFingerprint groups a frame read by an outdated extractor as one
// with a missing serial: it keeps its folder, clock, make, and model, and
// matches only other missing-serial frames. Every grouping path applies it.
func (frame *photoStackFrame) applyMetadataFingerprint(actual, expected string) {
	frame.Stale = expected != "" && actual != expected
	if frame.Stale {
		frame.BodySerialNumber = ""
	}
}

// photoStackAnyStale reports whether any frame was read by an outdated
// extractor. Its serial is unknown, so a decision that changes flags refuses
// the stack until backfill reads it.
func photoStackAnyStale(frames map[string]photoStackFrame) bool {
	return slices.ContainsFunc(slices.Collect(maps.Values(frames)), func(frame photoStackFrame) bool { return frame.Stale })
}

// photoStackPartition separates camera streams before chaining capture times.
type photoStackPartition struct{ folder, make, model, serial string }

func (frame *photoStackFrame) partition() photoStackPartition {
	return photoStackPartition{frame.Folder, frame.CameraMake, frame.CameraModel, frame.BodySerialNumber}
}

func comparePhotoStackFrames(a, b photoStackFrame) int {
	return cmp.Or(strings.Compare(a.SortKey, b.SortKey), strings.Compare(a.AssetID, b.AssetID))
}

// photoStackReach is the farthest sort key within the gap of key. Every
// grouping path uses it, so browsing, Cull runs, and decisions agree.
func photoStackReach(key string, gap int, forward bool) (string, bool) {
	stamp, err := time.Parse(photoStackKeyLayout, key)
	if err != nil {
		return "", false
	}
	step := time.Duration(gap) * time.Second
	if !forward {
		step = -step
	}
	return stamp.Add(step).Format(photoStackKeyLayout), true
}

// photoStackChained reports whether next follows previous in one stack.
func photoStackChained(head, previous, next photoStackFrame, gap int) bool {
	if previous.Folder == "" || previous.partition() != next.partition() || previous.Block != next.Block {
		return false
	}
	reach, ok := photoStackReach(previous.SortKey, gap, true)
	if ok && next.SortKey <= reach {
		return true
	}
	reach, ok = photoStackReach(head.SortKey, photoStackImageWindowSeconds, true)
	if !ok || next.SortKey > reach {
		return false
	}
	for space, headVector := range head.ImageVectors {
		if vector := next.ImageVectors[space]; len(vector) > 0 && vectorindex.Cosine(headVector, vector)+1e-7 >= photoStackImageSimilarityCutoff {
			return true
		}
	}
	return false
}

// groupPhotoStackFrames returns the stacks among frames, each in ascending
// capture order. Frames outside every stack are implicit singletons.
func groupPhotoStackFrames(frames []photoStackFrame, gap int) [][]photoStackFrame {
	frames = slices.DeleteFunc(slices.Clone(frames), func(frame photoStackFrame) bool { return frame.Folder == "" })
	slices.SortFunc(frames, func(a, b photoStackFrame) int {
		return cmp.Or(strings.Compare(a.Folder, b.Folder), strings.Compare(a.CameraMake, b.CameraMake), strings.Compare(a.CameraModel, b.CameraModel), strings.Compare(a.BodySerialNumber, b.BodySerialNumber), comparePhotoStackFrames(a, b))
	})
	var groups [][]photoStackFrame
	for group := range photoStackSegments(frames, gap) {
		if eligiblePhotoStack(group) {
			groups = append(groups, group)
		}
	}

	return groups
}

// photoStackSegments splits capture-ordered frames at the first-frame boundary.
// Callers sort each camera partition before applying the decision size limit.
func photoStackSegments(frames []photoStackFrame, gap int) iter.Seq[[]photoStackFrame] {
	return func(yield func([]photoStackFrame) bool) {
		for first, blocked := 0, photoStackBlocks(frames); first < len(frames); {
			next := first + 1
			for next < len(frames) && photoStackChained(blocked[first], blocked[next-1], blocked[next], gap) {
				next++
			}
			if !yield(frames[first:next]) {
				return
			}
			first = next
		}
	}
}

// photoStackBlocks marks where stacking restarts. A run of more than
// MaxPhotoStackFrames frames, each within the image window of the previous one,
// restarts at every clock hour, so a stack never depends on the run's earlier
// hours. Such an hour holding more than MaxPhotoStackFrames frames links only
// by burst gap, so resolving any stack reads at most that many vectors.
func photoStackBlocks(frames []photoStackFrame) []photoStackFrame {
	blocked := slices.Clone(frames)
	for first := 0; first < len(blocked); {
		next := first + 1
		for ; next < len(blocked) && blocked[next].partition() == blocked[first].partition(); next++ {
			if reach, ok := photoStackReach(blocked[next-1].SortKey, photoStackImageWindowSeconds, true); !ok || blocked[next].SortKey > reach {
				break
			}
		}
		counts := map[string]int{}
		for i := first; i < next; i++ {
			blocked[i].Block = ""
			if next-first > MaxPhotoStackFrames {
				blocked[i].Block = photoStackHour(blocked[i].SortKey)
			}
			counts[blocked[i].Block]++
		}
		for i := first; i < next; i++ {
			if counts[blocked[i].Block] > MaxPhotoStackFrames {
				blocked[i].ImageVectors = nil
			}
		}
		first = next
	}
	return blocked
}

// photoStackHour is the clock-hour prefix of a capture sort key.
func photoStackHour(key string) string {
	return key[:min(len(key), len("2006-01-02T15"))]
}

// eligiblePhotoStack keeps every exposed stack within one authored edit.
func eligiblePhotoStack(frames []photoStackFrame) bool {
	files := 0
	for _, frame := range frames {
		files += frame.Files
	}
	return len(frames) >= 2 && len(frames) <= MaxPhotoStackFrames && files <= MaxPhotoStackTargets
}

func photoRepresentativeEligible(frame photoStackFrame, allRejected bool) bool {
	return allRejected || !frame.Rejected || frame.Picked
}

func photoStackAllRejected(frames []photoStackFrame) bool {
	return !slices.ContainsFunc(frames, func(frame photoStackFrame) bool { return photoRepresentativeEligible(frame, false) })
}

// newPhotoStack chooses the latest eligible explicit choice, else the first
// picked frame, else the first non-rejected frame, else the first frame.
// Every frame is eligible when all frames are rejected.
func newPhotoStack(group []photoStackFrame) PhotoStack {
	stack := PhotoStack{AssetIDs: make([]string, len(group))}
	var chosen, picked, available *photoStackFrame
	allRejected := photoStackAllRejected(group)
	for i := range group {
		frame := &group[i]
		stack.AssetIDs[i] = frame.AssetID
		rejected := frame.Rejected && !frame.Picked
		if !rejected && available == nil {
			available = frame
		}
		if frame.Rank > 0 && photoRepresentativeEligible(*frame, allRejected) {
			if chosen == nil || frame.Rank > chosen.Rank {
				chosen = frame
			}
		}
		if frame.Picked && picked == nil {
			picked = frame
		}
	}
	switch {
	case chosen != nil:
		stack.RepresentativeID = chosen.AssetID
	case picked != nil:
		stack.RepresentativeID = picked.AssetID
	case available != nil:
		stack.RepresentativeID = available.AssetID
	default:
		stack.RepresentativeID = group[0].AssetID
	}
	return stack
}

// groupRankedPhotoPage groups only the returned relevance matches. Each burst
// occupies its highest-ranked member's position, keeping rank order within it.
func (s *Store) groupRankedPhotoPage(ctx context.Context, q metadataQuerier, page *PhotoBrowsePage, hidden bool, gap int, fingerprint string) error {
	ids := make([]string, len(page.Items))
	rows := make(map[string]PhotoBrowseRow, len(page.Items))
	for i, row := range page.Items {
		ids[i], rows[row.AssetID] = row.AssetID, row
	}
	frames, err := s.photoStackFrames(ctx, q, ids, hidden, fingerprint)
	if err != nil {
		return err
	}
	err = queryInChunks(ctx, q, `SELECT a.asset_id,`+photoStackChoiceColumns+` FROM photo_assets a WHERE a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var id string
		var choice photoStackFrame
		if err := rows.Scan(&id, &choice.Rank, &choice.Picked, &choice.Rejected); err != nil {
			return err
		}
		frame := frames[id]
		frame.Rank, frame.Picked, frame.Rejected = choice.Rank, choice.Picked, choice.Rejected
		frames[id] = frame
		return nil
	})
	if err != nil {
		return err
	}
	page.GroupingIncomplete = photoStackAnyStale(frames)
	byID := make(map[string]PhotoStack)
	for _, group := range groupPhotoStackFrames(slices.Collect(maps.Values(frames)), gap) {
		stack := newPhotoStack(group)
		for _, id := range stack.AssetIDs {
			byID[id] = stack
		}
	}
	page.Items = make([]PhotoBrowseRow, 0, len(ids))
	emitted := make(map[string]bool)
	for _, id := range ids {
		stack, ok := byID[id]
		if !ok {
			page.Items = append(page.Items, rows[id])
			continue
		}
		if emitted[stack.RepresentativeID] {
			continue
		}
		emitted[stack.RepresentativeID] = true
		page.Stacks = append(page.Stacks, stack)
		for _, member := range ids {
			if byID[member].RepresentativeID == stack.RepresentativeID {
				page.Items = append(page.Items, rows[member])
			}
		}
	}
	return nil
}

// photoStackChoiceColumns are the representative inputs: the explicit rank and
// whether any live non-sidecar file is picked or rejected.
const photoStackChoiceColumns = `COALESCE(a.representative_rank,0),
 EXISTS(SELECT 1 FROM photo_files pick JOIN nodes pn ON pn.id=pick.node_id WHERE pick.asset_id=a.asset_id AND pick.role!='sidecar' AND pn.trashed_at IS NULL AND pick.flag='pick'),
 EXISTS(SELECT 1 FROM photo_files reject JOIN nodes rn ON rn.id=reject.node_id WHERE reject.asset_id=a.asset_id AND reject.role!='sidecar' AND rn.trashed_at IS NULL AND reject.flag='reject')`

// The display file's timeline row decides grouping. Only photos join stacks.
const photoStackFileCount = `(SELECT COUNT(*) FROM photo_files live JOIN nodes ln ON ln.id=live.node_id WHERE live.asset_id=a.asset_id AND live.role!='sidecar' AND ln.trashed_at IS NULL)`

const photoStackCameraColumns = `COALESCE(p.camera_make_folded,''),COALESCE(p.camera_model_folded,''),COALESCE(p.body_serial_number,'')`

const photoStackFrameColumns = `a.asset_id,CASE WHEN a.kind='photo' THEN COALESCE(pl.folder_key,'') ELSE '' END,COALESCE(pl.sort_key,''),` + photoStackCameraColumns + `,` + photoStackFileCount

// photoStackFrames reads the live, visible members of ids. Missing IDs are unavailable.
func (s *Store) photoStackFrames(ctx context.Context, q metadataQuerier, ids []string, hidden bool, fingerprint string) (map[string]photoStackFrame, error) {
	frames, err := readPhotoStackFrames(ctx, q, ids, hidden, fingerprint)
	if err == nil {
		err = s.hydratePhotoStackVectors(ctx, q, frames)
	}
	return frames, err
}

func readPhotoStackFrames(ctx context.Context, q metadataQuerier, ids []string, hidden bool, fingerprint string) (map[string]photoStackFrame, error) {
	frames := make(map[string]photoStackFrame, len(ids))
	err := queryInChunks(ctx, q, `SELECT `+photoStackFrameColumns+`,COALESCE(mg.extractor_fingerprint,'') FROM `+photoBrowseDisplayFrom+` WHERE `+photoBrowseLiveDisplay+` AND `+photoVisibilityPredicate(hidden)+` AND a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var frame photoStackFrame
		var actual string
		if err := rows.Scan(&frame.AssetID, &frame.Folder, &frame.SortKey, &frame.CameraMake, &frame.CameraModel, &frame.BodySerialNumber, &frame.Files, &actual); err != nil {
			return err
		}
		frame.applyMetadataFingerprint(actual, fingerprint)
		frames[frame.AssetID] = frame
		return nil
	})
	return frames, err
}

// PhotoStackGroups groups an explicit ID set, such as a frozen Cull run, under
// the current gap. Groups keep request order inside and are ordered by their
// first member; IDs that are no longer live and visible are unavailable.
func (s *Store) PhotoStackGroups(ctx context.Context, ids []string, hidden bool, fingerprint string) (groups [][]string, unavailable []string, gap int, incomplete bool, err error) {
	position := make(map[string]int, len(ids))
	for i, id := range ids {
		if _, duplicate := position[id]; duplicate {
			return nil, nil, 0, false, fmt.Errorf("%w: asset_ids must contain unique IDs", ErrInvalidPhotoAsset)
		}
		if validateUUIDv4(id) != nil {
			return nil, nil, 0, false, fmt.Errorf("%w: asset_ids must contain UUIDv4 IDs", ErrInvalidPhotoAsset)
		}
		position[id] = i
	}
	err = s.photoReadTx(ctx, func(tx *sql.Tx) error {
		if hidden {
			if _, err := s.hiddenSession(ctx, tx); err != nil {
				return err
			}
		}
		settings, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		gap = settings.BurstGapSeconds
		frames, err := s.photoStackFrames(ctx, tx, ids, hidden, fingerprint)
		if err != nil {
			return err
		}
		unavailable = []string{}
		for _, id := range ids {
			if _, ok := frames[id]; !ok {
				unavailable = append(unavailable, id)
			}
		}
		incomplete = photoStackAnyStale(frames)
		groups = photoStackRequestGroups(slices.Collect(maps.Values(frames)), settings.BurstGapSeconds, position)
		return nil
	})
	return groups, unavailable, gap, incomplete, err
}

// photoStackRequestGroups orders each group's members, and the groups, by request position.
func photoStackRequestGroups(frames []photoStackFrame, gap int, position map[string]int) [][]string {
	groups := [][]string{}
	for _, group := range groupPhotoStackFrames(frames, gap) {
		ids := make([]string, len(group))
		for i, frame := range group {
			ids[i] = frame.AssetID
		}
		slices.SortFunc(ids, func(a, b string) int { return cmp.Compare(position[a], position[b]) })
		groups = append(groups, ids)
	}
	slices.SortFunc(groups, func(a, b []string) int { return cmp.Compare(position[a[0]], position[b[0]]) })
	return groups
}

type PhotoStackDecision struct {
	BurstGapSeconds int
	Hidden          bool
	AssetIDs        []string
	Targets         []PhotoAuthoredEditTarget
	Decision        string
	CurrentID       string
	SelectedIDs     []string
	// SourceMetadataFingerprint is the extractor identity browsing groups
	// under, so a decision revalidates the stack that was shown.
	SourceMetadataFingerprint string
}

// checkPhotoStackTargetTx tolerates only sidecar changes to asset membership.
// Original file, node, and displayed content versions stay exact.
func checkPhotoStackTargetTx(ctx context.Context, tx *sql.Tx, wanted PhotoAuthoredEditTarget, target PhotoPreparedTarget) error {
	seen := wanted.AssetRevision
	wanted.AssetRevision = target.AssetRevision
	if wanted.PhotoAuthoredTargetRevision != target.PhotoAuthoredTargetRevision || seen < 1 || seen > target.AssetRevision {
		return ErrStaleRevision
	}
	if seen != target.AssetRevision {
		if err := photoAssetSidecarOnlySinceTx(ctx, tx, target.AssetID, seen, target.AssetRevision); err != nil {
			return err
		}
	}
	if wanted.ContentVersionID != "" && (target.CurrentContentVersionID != wanted.ContentVersionID || target.DisplayFileID != target.FileID) {
		return ErrStaleRevision
	}
	return nil
}

// DecidePhotoStack applies one decision to an observed stack in one
// transaction. The submitted members must still form one stack under the
// stored folder and capture keys, and every file must keep its observed revision.
func (s *Store) DecidePhotoStack(ctx context.Context, request PhotoStackDecision) (PhotoAuthoredReceipt, error) {
	if !slices.Contains(request.AssetIDs, request.CurrentID) {
		return PhotoAuthoredReceipt{}, fmt.Errorf("%w: current_id must belong to asset_ids", ErrInvalidPhotoAsset)
	}
	if request.Decision == "selected" && len(request.SelectedIDs) == 0 {
		return PhotoAuthoredReceipt{}, fmt.Errorf("%w: selected_ids must contain at least one ID for a selected decision", ErrInvalidPhotoAsset)
	}
	for _, id := range request.SelectedIDs {
		if !slices.Contains(request.AssetIDs, id) {
			return PhotoAuthoredReceipt{}, fmt.Errorf("%w: selected_ids must belong to asset_ids", ErrInvalidPhotoAsset)
		}
	}
	vectors, err := s.newPhotoStackVectorReader(ctx, s.db)
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	return s.decidePhotoStack(ctx, request, vectors)
}

func (s *Store) decidePhotoStack(ctx context.Context, request PhotoStackDecision, vectors *photoStackVectorReader) (PhotoAuthoredReceipt, error) {
	var result PhotoAuthoredReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		heads, err := photoStackVectorHeads(ctx, tx)
		if err != nil {
			return err
		}
		if !maps.Equal(heads, vectors.heads) {
			return ErrStalePhotoStack
		}
		if request.Hidden {
			if _, err := s.hiddenSession(ctx, tx); err != nil {
				return err
			}
		}
		settings, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if settings.BurstGapSeconds != request.BurstGapSeconds {
			return ErrStalePhotoStack
		}
		// Only the observed query or frozen run defines membership, so unseen
		// neighboring imports stay untouched.
		found, err := readPhotoStackFrames(ctx, tx, request.AssetIDs, request.Hidden, request.SourceMetadataFingerprint)
		if err == nil {
			err = vectors.hydrate(ctx, tx, found)
		}
		if err != nil {
			return err
		}
		if len(found) != len(request.AssetIDs) || (request.Decision != "representative" && photoStackAnyStale(found)) {
			return ErrStalePhotoStack
		}
		frames := slices.SortedFunc(maps.Values(found), comparePhotoStackFrames)
		groups := groupPhotoStackFrames(frames, settings.BurstGapSeconds)
		if len(groups) != 1 || len(groups[0]) != len(frames) {
			return ErrStalePhotoStack
		}

		// Prepare every live source file inside the transaction, so a client
		// cannot omit a sibling or one half of a RAW/JPEG pair.
		preparation := PhotoAuthoredPreparation{}
		loaded := map[string]photoAuthoredLoaded{}
		preparer := photoAuthoredPreparer{tx: tx, hidden: request.Hidden, settings: settings, result: &preparation, limit: MaxPhotoStackTargets, loaded: loaded}
		if err := preparer.prepare(ctx, request.AssetIDs); err != nil {
			return err
		}
		if len(preparation.Targets) != len(request.Targets) {
			return ErrStalePhotoStack
		}
		expected := map[string]PhotoAuthoredEditTarget{}
		for _, target := range request.Targets {
			if _, duplicate := expected[target.FileID]; duplicate {
				return fmt.Errorf("%w: targets must contain unique file_id values", ErrInvalidPhotoAsset)
			}
			expected[target.FileID] = target
		}
		for _, target := range preparation.Targets {
			wanted, ok := expected[target.FileID]
			if !ok {
				return ErrStalePhotoStack
			}
			if err := checkPhotoStackTargetTx(ctx, tx, wanted, target); err != nil {
				return err
			}
		}
		kept := map[string]bool{}
		for _, target := range preparation.Targets {
			if target.Values.Confirmed&PhotoConfirmedFlag != 0 && (request.Decision != "current" || target.Values.FlagOwner == "") {
				kept[target.AssetID] = true
			}
			frame := found[target.AssetID]
			frame.Picked = frame.Picked || target.Values.Flag == "pick"
			frame.Rejected = frame.Rejected || target.Values.Flag == "reject"
			found[target.AssetID] = frame
		}
		if request.Decision == "representative" {
			for i := range frames {
				frames[i] = found[frames[i].AssetID]
			}
			if !photoRepresentativeEligible(found[request.CurrentID], photoStackAllRejected(frames)) {
				return fmt.Errorf("%w: a rejected frame cannot represent a stack with non-rejected frames", ErrInvalidPhotoAsset)
			}
			result, err = s.choosePhotoRepresentativeTx(ctx, tx, found[request.CurrentID])
			return err
		}
		chosen := []string{request.CurrentID}
		switch request.Decision {
		case "all":
			chosen = request.AssetIDs
		case "selected":
			chosen = request.SelectedIDs
		}
		targets := make([]PhotoAuthoredTarget, 0, len(preparation.Targets))
		for _, target := range preparation.Targets {
			// Chosen frames become picks; rejects skip frames with an independent or unowned flag.
			flag := "pick"
			if !slices.Contains(chosen, target.AssetID) {
				if kept[target.AssetID] {
					if request.Decision == "current" || target.Values.FlagOwner == "" {
						continue
					}
					flag = target.Values.Flag
				} else {
					flag = "reject"
				}
			}
			targets = append(targets, PhotoAuthoredTarget{FileID: target.FileID, Revision: target.Revision, Patch: PhotoAuthoredPatch{Flag: new(flag)}})
		}
		result, err = s.applyPhotoStackFlagsTx(ctx, tx, targets, loaded, request.Decision == "current")
		return err
	})
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	return result, nil
}

// applyPhotoStackFlagsTx keeps the stack decision and its undo receipt together.
func (s *Store) applyPhotoStackFlagsTx(ctx context.Context, tx *sql.Tx, targets []PhotoAuthoredTarget, loaded map[string]photoAuthoredLoaded, returnFlags bool) (PhotoAuthoredReceipt, error) {
	result, err := s.applyPhotoAuthoredValuesTx(ctx, tx, targets, photoAuthoredApply{loaded: loaded, returnFlags: returnFlags})
	if err != nil || result.ReceiptID == "" {
		return result, err
	}
	if err := checkPhotoAuthoredReceiptSize(result); err != nil {
		return result, err
	}
	return result, persistPhotoAuthoredReceiptTx(ctx, tx, result)
}

// choosePhotoRepresentativeTx stores an explicit choice with the next rank.
// Flags stay unchanged, so every frame keeps its picks and rejects.
func (s *Store) choosePhotoRepresentativeTx(ctx context.Context, tx *sql.Tx, frame photoStackFrame) (PhotoAuthoredReceipt, error) {
	prior, err := photoBurstSettingsTx(ctx, tx)
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO photo_library_settings(singleton,updated_at) VALUES(1,'') ON CONFLICT(singleton) DO NOTHING`); err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	var rank int64
	if err := tx.QueryRowContext(ctx, `UPDATE photo_library_settings SET representative_sequence=COALESCE(representative_sequence,0)+1 WHERE singleton=1 RETURNING representative_sequence`).Scan(&rank); err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	change, err := setPhotoRepresentativeTx(ctx, tx, frame.AssetID, -1, &rank)
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	if err := s.auditPhotoBurstTx(ctx, tx, prior, []PhotoRepresentativeChange{change}); err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	return persistPhotoRepresentativeReceiptTx(ctx, tx, PhotoAuthoredReceipt{Representatives: []PhotoRepresentativeChange{change}})
}

// setPhotoRepresentativeTx changes one asset's rank and revision. A
// non-negative revision is the precondition an undo checks.
func setPhotoRepresentativeTx(ctx context.Context, tx *sql.Tx, assetID string, revision int64, rank *int64) (PhotoRepresentativeChange, error) {
	change, err := photoRepresentativeStateTx(ctx, tx, assetID, rank)
	if err != nil {
		return change, err
	}
	if revision >= 0 && change.BeforeRevision != revision {
		return change, ErrStaleRevision
	}
	change.AfterRevision = change.BeforeRevision + 1
	_, err = tx.ExecContext(ctx, `UPDATE photo_assets SET representative_rank=?,revision=?,updated_at=? WHERE asset_id=?`, rank, change.AfterRevision, nowRFC3339(), assetID)
	return change, err
}

// photoRepresentativeStateTx loads the prior rank and asset revision.
func photoRepresentativeStateTx(ctx context.Context, tx *sql.Tx, assetID string, rank *int64) (PhotoRepresentativeChange, error) {
	change := PhotoRepresentativeChange{AssetID: assetID, After: rank}
	var before sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT revision,representative_rank FROM photo_assets WHERE asset_id=?`, assetID).Scan(&change.BeforeRevision, &before); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return change, ErrNotFound
		}
		return change, err
	}
	if before.Valid {
		change.Before = new(before.Int64)
	}
	return change, nil
}

// photoStackPager emits whole query-scoped stacks from the live capture-time
// keyset. A chain whose gap reach stays strictly inside the rows a page has
// read is grouped from those rows; a chain that touches an edge reads its
// run from the folder index. Each component is resolved once per page.
type photoStackPager struct {
	q           metadataQuerier
	scope       photoBrowseScope
	gap         int
	descending  bool
	probes      [2]*sql.Stmt
	args        []any
	components  map[string]*photoStackComponent
	frames      map[string]photoStackFrame
	vectors     *photoStackVectorReader
	reach       int
	fingerprint string
}

type photoStackComponent struct {
	frames     []photoStackFrame
	ineligible bool
}

// A probe reads every in-scope frame within this many image windows, so one
// query usually finishes a run in one direction.
const photoStackProbeGaps = 8

const photoStackProbeFrom = `photo_timeline pl INDEXED BY photo_timeline_folder
 CROSS JOIN photo_files f ON f.file_id=pl.file_id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 CROSS JOIN nodes n ON n.id=f.node_id
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
 LEFT JOIN source_metadata_generations mg ON mg.generation_id=h.generation_id
 LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id`

func newPhotoStackPager(ctx context.Context, q metadataQuerier, scope photoBrowseScope, gap int, descending bool, fingerprint string, vectors *photoStackVectorReader) (*photoStackPager, error) {
	preparer, ok := q.(interface {
		PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	})
	if !ok {
		return nil, errors.New("photo stack paging requires a statement-preparing connection")
	}
	pager := &photoStackPager{q: q, scope: scope, gap: gap, fingerprint: fingerprint, descending: descending, components: map[string]*photoStackComponent{}, frames: map[string]photoStackFrame{}, vectors: vectors, reach: gap}
	if len(vectors.indexes) > 0 {
		pager.reach = max(gap, photoStackImageWindowSeconds)
	}
	// Each probe reads the in-scope frames after one frame, up to a window bound.
	for i, bounds := range [2]string{
		`pl.sort_key<=? AND pl.sort_key>=? AND (pl.sort_key<? OR a.asset_id<?) ORDER BY pl.sort_key DESC,a.asset_id DESC LIMIT ?`,
		`pl.sort_key>=? AND pl.sort_key<=? AND (pl.sort_key>? OR a.asset_id>?) ORDER BY pl.sort_key ASC,a.asset_id ASC LIMIT ?`,
	} {
		statement, args, err := scope.bind(`SELECT a.asset_id,pl.sort_key,` + photoStackFileCount + `,COALESCE(mg.extractor_fingerprint,'')` + ` FROM ` + photoStackProbeFrom + scope.queue + ` WHERE ` + scope.where() + ` AND a.kind='photo' AND pl.folder_key=? AND COALESCE(p.camera_make_folded,'')=? AND COALESCE(p.camera_model_folded,'')=? AND CASE WHEN ?='' OR COALESCE(mg.extractor_fingerprint,'')=? THEN COALESCE(p.body_serial_number,'') ELSE '' END=? AND ` + bounds)
		if err == nil {
			pager.args = args
			pager.probes[i], err = preparer.PrepareContext(ctx, statement) //nolint:sqlclosecheck // Close releases both probes when the page ends.
		}
		if err != nil {
			return nil, errors.Join(err, pager.Close())
		}
	}
	return pager, nil
}

func (p *photoStackPager) Close() error {
	var err error
	for _, probe := range p.probes {
		if probe != nil {
			err = errors.Join(err, probe.Close())
		}
	}
	return err
}

// probe reads the in-scope frames after current in one direction, up to window.
func (p *photoStackPager) probe(ctx context.Context, current photoStackFrame, window string, forward bool, limit int) (frames []photoStackFrame, err error) {
	direction := 0
	if forward {
		direction = 1
	}
	args := append(slices.Clone(p.args), current.Folder, current.CameraMake, current.CameraModel, p.fingerprint, p.fingerprint, current.BodySerialNumber, current.SortKey, window, current.SortKey, current.AssetID, limit)
	rows, err := p.probes[direction].QueryContext(ctx, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		next := photoStackFrame{Folder: current.Folder, CameraMake: current.CameraMake, CameraModel: current.CameraModel, BodySerialNumber: current.BodySerialNumber}
		var actual string
		if err := rows.Scan(&next.AssetID, &next.SortKey, &next.Files, &actual); err != nil {
			return nil, err
		}
		next.applyMetadataFingerprint(actual, p.fingerprint)
		frames = append(frames, next)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return frames, nil
}

// hydrate reuses page inputs and hydrates each walk's unread candidates together.
func (p *photoStackPager) hydrate(ctx context.Context, inputs map[string]photoStackFrame) error {
	missing := make(map[string]photoStackFrame)
	for id, frame := range inputs {
		if cached, ok := p.frames[id]; ok {
			inputs[id] = cached
		} else {
			missing[id] = frame
		}
	}
	if err := p.vectors.hydrate(ctx, p.q, missing); err != nil {
		return err
	}
	for id, frame := range missing {
		inputs[id], p.frames[id] = frame, frame
	}
	return nil
}

// hydrateFrames hydrates frames in place, reusing page inputs.
func (p *photoStackPager) hydrateFrames(ctx context.Context, frames []photoStackFrame) error {
	inputs := make(map[string]photoStackFrame, len(frames))
	for _, frame := range frames {
		inputs[frame.AssetID] = frame
	}
	err := p.hydrate(ctx, inputs)
	for i := range frames {
		frames[i] = inputs[frames[i].AssetID]
	}
	return err
}

// walk appends up to MaxPhotoStackFrames in-scope frames of frame's camera
// stream in one direction while each lies within the image window of the last.
func (p *photoStackPager) walk(ctx context.Context, frame photoStackFrame, forward bool, component *photoStackComponent) error {
	for current := frame; len(component.frames) <= MaxPhotoStackFrames; {
		window, ok := photoStackReach(current.SortKey, photoStackImageWindowSeconds*photoStackProbeGaps, forward)
		if !ok {
			return nil
		}
		read, err := p.probe(ctx, current, window, forward, MaxPhotoStackFrames+1-len(component.frames))
		if err != nil || len(read) == 0 {
			return err
		}
		for _, next := range read {
			reach, ok := photoStackReach(current.SortKey, photoStackImageWindowSeconds, forward)
			if !ok || forward && next.SortKey > reach || !forward && next.SortKey < reach {
				return nil
			}
			component.frames = append(component.frames, next)
			current = next
		}
	}
	return nil
}

// component resolves frame's stack from its block: its run, or its hour of a long run.
func (p *photoStackPager) component(ctx context.Context, frame photoStackFrame) (*photoStackComponent, error) {
	if found := p.components[frame.AssetID]; found != nil {
		return found, nil
	}
	walks := [2]*photoStackComponent{{frames: []photoStackFrame{frame}}, {frames: []photoStackFrame{frame}}}
	for i, walk := range walks {
		if err := p.walk(ctx, frame, i == 1, walk); err != nil {
			return nil, err
		}
	}
	window := slices.SortedFunc(slices.Values(slices.Concat(walks[0].frames, walks[1].frames[1:])), comparePhotoStackFrames)
	blocked := photoStackBlocks(window)
	key := blocked[slices.IndexFunc(blocked, func(candidate photoStackFrame) bool { return candidate.AssetID == frame.AssetID })].Block
	block := slices.DeleteFunc(blocked, func(candidate photoStackFrame) bool { return candidate.Block != key })
	if len(block) <= MaxPhotoStackFrames {
		if err := p.hydrateFrames(ctx, block); err != nil {
			return nil, err
		}
	}
	for segment := range photoStackSegments(block, p.gap) {
		// A probe limit can cut an edge chain; only an oversized one is certain.
		if cut := len(walks[0].frames) > MaxPhotoStackFrames && segment[0].AssetID == window[0].AssetID || len(walks[1].frames) > MaxPhotoStackFrames && segment[len(segment)-1].AssetID == window[len(window)-1].AssetID; !cut || len(segment) > MaxPhotoStackFrames {
			p.remember(&photoStackComponent{frames: segment})
		}
	}
	return p.components[frame.AssetID], nil
}

func (p *photoStackPager) remember(component *photoStackComponent) {
	component.ineligible = len(component.frames) > 1 && !eligiblePhotoStack(component.frames)
	slices.SortFunc(component.frames, comparePhotoStackFrames)
	for _, member := range component.frames {
		p.components[member.AssetID] = component
	}
}

// prime groups a raw batch in Go. The batch holds every in-scope row whose key
// lies strictly between its edges: after, the position it was read after, and
// its last known key unless the keyset ended. A chain whose gap reach stays
// inside both edges is complete; one that touches an edge is left for probes.
func (p *photoStackPager) prime(ctx context.Context, batch []PhotoBrowseRow, after *PhotoBrowsePosition, exhausted bool) error {
	ids := make([]string, len(batch))
	for i, row := range batch {
		ids[i] = row.AssetID
	}
	inputs := make(map[string]photoStackFrame, len(batch))
	for _, row := range batch {
		inputs[row.AssetID] = p.frame(row)
	}
	if err := queryInChunks(ctx, p.q, `SELECT a.asset_id,`+photoStackFileCount+` FROM photo_assets a WHERE a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return err
		}
		frame := inputs[id]
		frame.Files = count
		inputs[id] = frame
		return nil
	}); err != nil {
		return err
	}

	if err := p.hydrate(ctx, inputs); err != nil {
		return err
	}
	// A partial batch cannot establish the first frame of an image stack.
	// Resolve it through the folder probes in capture order instead.
	if len(p.vectors.indexes) > 0 {
		return nil
	}
	if after != nil && after.Missing {
		return nil
	}
	partitions := map[photoStackPartition][]photoStackFrame{}
	open, last := !exhausted, ""
	for _, row := range batch {
		if row.position.Missing {
			open = false
			continue
		}
		last = row.position.Key
		if row.folderKey != "" {
			frame := p.frame(row)
			partitions[frame.partition()] = append(partitions[frame.partition()], frame)
		}
	}
	start, end := "", ""
	if after != nil {
		start = after.Key
	}
	if open {
		end = last
	}
	// Scanning ascending reads toward later keys, so the end edge is forward.
	edges := map[bool]string{true: end, false: start}
	if p.descending {
		edges = map[bool]string{true: start, false: end}
	}
	inside := func(frame photoStackFrame, forward bool) bool {
		edge := edges[forward]
		if edge == "" {
			return true
		}
		reach, ok := photoStackReach(frame.SortKey, p.reach, forward)
		return ok && (forward && reach < edge || !forward && reach > edge)
	}
	for _, frames := range partitions {
		slices.SortFunc(frames, comparePhotoStackFrames)
		for chain := range photoStackSegments(frames, p.gap) {
			if known := slices.IndexFunc(chain, func(frame photoStackFrame) bool { return p.components[frame.AssetID] != nil }); known >= 0 {
				// Probing stops at the cap. Connected rows beyond that
				// point share the ineligible classification.
				if component := p.components[chain[known].AssetID]; component.ineligible && photoStackHour(chain[0].SortKey) == photoStackHour(chain[len(chain)-1].SortKey) {
					for _, member := range chain {
						p.components[member.AssetID] = component
					}
				}
				continue
			}
			if inside(chain[0], false) && inside(chain[len(chain)-1], true) && photoStackHour(chain[0].SortKey) == photoStackHour(chain[len(chain)-1].SortKey) {
				p.remember(&photoStackComponent{frames: slices.Clone(chain)})
			}
		}
	}
	return nil
}

func (p *photoStackPager) frame(row PhotoBrowseRow) photoStackFrame {
	if frame, ok := p.frames[row.AssetID]; ok {
		return frame
	}
	frame := photoStackFrame{AssetID: row.AssetID, Folder: row.folderKey, CameraMake: row.cameraMake, CameraModel: row.cameraModel, BodySerialNumber: row.bodySerialNumber, SortKey: row.position.Key}
	frame.applyMetadataFingerprint(row.metadataFingerprint, p.fingerprint)
	return frame
}

// compare follows the browse order: capture direction, then UUID ascending.
func (p *photoStackPager) compare(a, b photoStackFrame) int {
	key := strings.Compare(a.SortKey, b.SortKey)
	if p.descending {
		key = -key
	}
	return cmp.Or(key, strings.Compare(a.AssetID, b.AssetID))
}

// classify returns the frames a raw row emits: the row alone, its whole stack
// at the first member in scan order, or nothing for any other member.
func (p *photoStackPager) classify(ctx context.Context, row PhotoBrowseRow) ([]photoStackFrame, error) {
	frame := p.frame(row)
	if frame.Folder == "" || row.position.Missing {
		return []photoStackFrame{frame}, nil
	}
	component, err := p.component(ctx, frame)
	if err != nil {
		return nil, err
	}
	if component.ineligible || len(component.frames) == 1 {
		return []photoStackFrame{frame}, nil
	}
	first := slices.MinFunc(component.frames, p.compare)
	if first.AssetID != row.AssetID {
		return nil, nil
	}
	return component.frames, nil
}

// fill assembles a page from its emitted groups. It reads every stack's
// representative inputs at once, and the rows of members the scan did not reach.
func (p *photoStackPager) fill(ctx context.Context, page *PhotoBrowsePage, groups [][]photoStackFrame, scanned map[string]PhotoBrowseRow, identity string) error {
	var ids, unread []string
	for _, group := range groups {
		if len(group) == 1 {
			continue
		}
		for _, frame := range group {
			ids = append(ids, frame.AssetID)
			if _, ok := scanned[frame.AssetID]; !ok {
				unread = append(unread, frame.AssetID)
			}
		}
	}
	// The scan already checked membership, so only the choice inputs remain.
	inputs := make(map[string]photoStackFrame, len(ids))
	err := queryInChunks(ctx, p.q, `SELECT a.asset_id,`+photoStackChoiceColumns+` FROM photo_assets a WHERE a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var frame photoStackFrame
		if err := rows.Scan(&frame.AssetID, &frame.Rank, &frame.Picked, &frame.Rejected); err != nil {
			return err
		}
		inputs[frame.AssetID] = frame
		return nil
	})
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(unread, photoSelectionChunkSize) {
		statement, args, err := p.scope.bind(`SELECT `+photoBrowseSelect+`,pl.sort_key,0 FROM `+photoBrowseDisplayFrom+p.scope.queue+` WHERE `+p.scope.where()+` AND a.asset_id IN (`+placeholders(len(chunk))+`)`, anyArgs(chunk)...)
		if err != nil {
			return err
		}
		rows, err := p.q.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		loaded, err := scanPhotoBrowseRows(rows, PhotoBrowsePosition{QueryIdentity: identity, Total: page.Total})
		if err != nil {
			return err
		}
		for _, row := range loaded {
			scanned[row.AssetID] = row
		}
	}
	page.Items, page.Stacks = make([]PhotoBrowseRow, 0, len(ids)+len(groups)), []PhotoStack{}
	for _, group := range groups {
		page.GroupingIncomplete = page.GroupingIncomplete || slices.ContainsFunc(group, func(frame photoStackFrame) bool { return frame.Stale })
		if len(group) == 1 {
			page.Items = append(page.Items, scanned[group[0].AssetID])
			continue
		}
		stack := make([]photoStackFrame, len(group))
		for i, frame := range group {
			input, ok := inputs[frame.AssetID]
			_, scannedOK := scanned[frame.AssetID]
			if !ok || !scannedOK {
				return errors.New("photo stack members changed inside one read snapshot")
			}
			stack[i] = photoStackFrame{AssetID: frame.AssetID, Folder: frame.Folder, SortKey: frame.SortKey, Rank: input.Rank, Picked: input.Picked, Rejected: input.Rejected}
		}
		ordered := slices.Clone(group)
		slices.SortFunc(ordered, p.compare)
		for _, frame := range ordered {
			page.Items = append(page.Items, scanned[frame.AssetID])
		}
		page.Stacks = append(page.Stacks, newPhotoStack(stack))
	}
	return nil
}

// frontier fingerprints eligible stacks that could gain members across an
// issued cursor. Future-only stacks stay outside this live paging boundary.
func (p *photoStackPager) frontier(ctx context.Context, boundary *PhotoBrowsePosition) (fingerprint string, err error) {
	hash := sha256.New()
	if !boundary.Missing {
		low, _ := photoStackReach(boundary.Key, p.reach, false)
		high, _ := photoStackReach(boundary.Key, p.reach, true)
		from, _, _ := photoBrowseOrder("capture_time")
		statement, args, err := p.scope.bind(`SELECT a.asset_id,pl.folder_key,pl.sort_key,`+photoStackCameraColumns+`,`+photoStackFileCount+`,COALESCE(mg.extractor_fingerprint,'') FROM `+from+p.scope.queue+` WHERE `+p.scope.where()+` AND a.kind='photo' AND pl.folder_key IS NOT NULL AND pl.sort_key>=? AND pl.sort_key<=?`, low, high)
		if err != nil {
			return "", err
		}
		rows, err := p.q.QueryContext(ctx, statement, args...)
		if err != nil {
			return "", err
		}
		defer func() { err = errors.Join(err, rows.Close()) }()
		var seeds, members []photoStackFrame
		for rows.Next() {
			var frame photoStackFrame
			var actual string
			if err := rows.Scan(&frame.AssetID, &frame.Folder, &frame.SortKey, &frame.CameraMake, &frame.CameraModel, &frame.BodySerialNumber, &frame.Files, &actual); err != nil {
				return "", errors.Join(err, rows.Close())
			}
			frame.applyMetadataFingerprint(actual, p.fingerprint)
			seeds = append(seeds, frame)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return "", err
		}
		if err := p.hydrateFrames(ctx, seeds); err != nil {
			return "", err
		}
		seen := map[*photoStackComponent]bool{}
		position := photoStackFrame{AssetID: boundary.AssetID, SortKey: boundary.Key}
		for _, seed := range seeds {
			component, err := p.component(ctx, seed)
			if err != nil {
				return "", err
			}
			if seen[component] || !eligiblePhotoStack(component.frames) {
				continue
			}
			seen[component] = true
			first := slices.MinFunc(component.frames, p.compare)
			// Wider image probes also find completed stacks far behind the cursor.
			// Only stacks crossing it or within the burst gap affect its frontier.
			last := slices.MaxFunc(component.frames, p.compare)
			edge, _ := photoStackReach(boundary.Key, p.gap, p.descending)
			near := !p.descending && last.SortKey >= edge || p.descending && last.SortKey <= edge
			if p.compare(first, position) <= 0 && near {
				members = append(members, component.frames...)
			}
		}
		slices.SortFunc(members, comparePhotoStackFrames)
		for _, frame := range members {
			_, _ = fmt.Fprintf(hash, "%q %q %q %q %q %q %d\n", frame.Folder, frame.CameraMake, frame.CameraModel, frame.BodySerialNumber, frame.AssetID, frame.SortKey, frame.Files)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// listPhotoStackPage fills a capture-time page with whole stacks. It keeps
// reading the live keyset until the page has its requested frames, so the last
// stack may extend it by up to 499. A stack is emitted at its first member in
// scan order, and its other members are skipped wherever the scan meets them.
// The cursor is the row before the next emitted row, so the next page neither
// repeats nor re-reads the members this page skipped, and interleaved photos
// from other folders follow in their own turn.
func (s *Store) listPhotoStackPage(
	ctx context.Context, q metadataQuerier, scope photoBrowseScope, gap int, descending bool, size int,
	boundary *PhotoBrowsePosition, identity string,
	readRaw func(*PhotoBrowsePosition, int) ([]PhotoBrowseRow, error), page *PhotoBrowsePage, fingerprint string,
) (err error) {
	vectors, err := s.newPhotoStackVectorReader(ctx, q)
	if err != nil {
		return err
	}
	pager, err := newPhotoStackPager(ctx, q, scope, gap, descending, fingerprint, vectors)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, pager.Close()) }()
	if boundary != nil {
		frontier, err := pager.frontier(ctx, boundary)
		if err != nil {
			return err
		}
		if frontier != boundary.StackFrontier {
			return ErrInvalidPhotoCursor
		}
	}
	var groups [][]photoStackFrame
	scanned := map[string]PhotoBrowseRow{}
	frames, limit, lookedAhead := 0, size+1, false
	previous := boundary
	for after := boundary; ; {
		batch, err := readRaw(after, limit)
		if err != nil {
			return err
		}
		exhausted := len(batch) < limit
		if err := pager.prime(ctx, batch, after, exhausted); err != nil {
			return err
		}
		for _, row := range batch {
			scanned[row.AssetID] = row
		}
		for _, row := range batch {
			group, err := pager.classify(ctx, row)
			if err != nil {
				return err
			}
			if len(group) > 0 && frames >= size {
				page.Next = previous
				page.Next.StackFrontier, err = pager.frontier(ctx, previous)
				if err != nil {
					return err
				}
				return pager.fill(ctx, page, groups, scanned, identity)
			}
			if len(group) > 0 {
				groups = append(groups, group)
				frames += len(group)
			}
			previous = new(row.position)
		}
		if exhausted {
			return pager.fill(ctx, page, groups, scanned, identity)
		}
		after = new(batch[len(batch)-1].position)
		// A full page first reads only its unread members and one more row;
		// if more skipped rows follow, it returns to whole batches.
		limit = size + 1
		if frames >= size && !lookedAhead {
			lookedAhead = true
			limit = 1
			for _, group := range groups {
				for _, frame := range group {
					if _, ok := scanned[frame.AssetID]; !ok {
						limit++
					}
				}
			}
		}
	}
}
