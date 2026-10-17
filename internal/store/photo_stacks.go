package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
)

// MaxPhotoStackFrames bounds a stack to one decision request. A longer
// connected run stays as individual photos rather than a misleading partial stack.
const MaxPhotoStackFrames = 500

// MaxPhotoStackGroupIDs bounds one explicit-ID grouping request to a Cull run.
const MaxPhotoStackGroupIDs = 250_000

const photoStackKeyLayout = "2006-01-02T15:04:05.000000000"

var ErrStalePhotoStack = errors.New("photo stack changed; refresh photos before deciding")

// PhotoStack is one burst of at least two frames in ascending capture order.
// Ranks and picks let a client re-derive the representative after its own edits.
type PhotoStack struct {
	AssetIDs            []string
	RepresentativeID    string
	PickedIDs           []string
	RepresentativeRanks map[string]int64
}

// photoStackFrame is one live asset's grouping input. Folder is empty when the
// photo cannot join a stack.
type photoStackFrame struct {
	AssetID string
	Folder  string
	SortKey string
	Rank    int64
	Picked  bool
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
func photoStackChained(previous, next photoStackFrame, gap int) bool {
	reach, ok := photoStackReach(previous.SortKey, gap, true)
	return ok && previous.Folder != "" && previous.Folder == next.Folder && next.SortKey <= reach
}

// groupPhotoStackFrames returns the stacks among frames, each in ascending
// capture order. Frames outside every stack are implicit singletons.
func groupPhotoStackFrames(frames []photoStackFrame, gap int) [][]photoStackFrame {
	frames = slices.DeleteFunc(slices.Clone(frames), func(frame photoStackFrame) bool { return frame.Folder == "" })
	slices.SortFunc(frames, func(a, b photoStackFrame) int {
		return cmp.Or(strings.Compare(a.Folder, b.Folder), comparePhotoStackFrames(a, b))
	})
	var groups [][]photoStackFrame
	for start := 0; start < len(frames); {
		end := start + 1
		for end < len(frames) && photoStackChained(frames[end-1], frames[end], gap) {
			end++
		}
		if size := end - start; size > 1 && size <= MaxPhotoStackFrames {
			groups = append(groups, frames[start:end])
		}
		start = end
	}
	return groups
}

// newPhotoStack applies the representative rule: the latest explicit choice,
// else the only picked frame, else the first frame.
func newPhotoStack(group []photoStackFrame) PhotoStack {
	stack := PhotoStack{AssetIDs: make([]string, len(group))}
	var chosen, picked *photoStackFrame
	picks := 0
	for i := range group {
		frame := &group[i]
		stack.AssetIDs[i] = frame.AssetID
		if frame.Rank > 0 {
			if stack.RepresentativeRanks == nil {
				stack.RepresentativeRanks = map[string]int64{}
			}
			stack.RepresentativeRanks[frame.AssetID] = frame.Rank
			if chosen == nil || frame.Rank > chosen.Rank {
				chosen = frame
			}
		}
		if frame.Picked {
			stack.PickedIDs = append(stack.PickedIDs, frame.AssetID)
			picked = frame
			picks++
		}
	}
	switch {
	case chosen != nil:
		stack.RepresentativeID = chosen.AssetID
	case picks == 1:
		stack.RepresentativeID = picked.AssetID
	default:
		stack.RepresentativeID = group[0].AssetID
	}
	return stack
}

// photoStackChoiceColumns are the representative inputs: the explicit rank and
// whether any live non-sidecar file is picked.
const photoStackChoiceColumns = `COALESCE(a.representative_rank,0),
 EXISTS(SELECT 1 FROM photo_files pick JOIN nodes pn ON pn.id=pick.node_id WHERE pick.asset_id=a.asset_id AND pick.role!='sidecar' AND pn.trashed_at IS NULL AND pick.flag='pick')`

// The display file's timeline row decides grouping. Only photos join stacks.
const photoStackFrameColumns = `a.asset_id,CASE WHEN a.kind='photo' THEN COALESCE(pl.folder_key,'') ELSE '' END,COALESCE(pl.sort_key,''),` + photoStackChoiceColumns

// photoStackFrames reads the live, visible members of ids. Missing IDs are unavailable.
func photoStackFrames(ctx context.Context, q metadataQuerier, ids []string, hidden bool) (map[string]photoStackFrame, error) {
	frames := make(map[string]photoStackFrame, len(ids))
	err := queryInChunks(ctx, q, `SELECT `+photoStackFrameColumns+` FROM `+photoBrowseDisplayFrom+` WHERE `+photoBrowseLiveDisplay+` AND `+photoVisibilityPredicate(hidden)+` AND a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var frame photoStackFrame
		if err := rows.Scan(&frame.AssetID, &frame.Folder, &frame.SortKey, &frame.Rank, &frame.Picked); err != nil {
			return err
		}
		frames[frame.AssetID] = frame
		return nil
	})
	return frames, err
}

// PhotoStackGroups groups an explicit ID set, such as a frozen Cull run, under
// the current gap. Groups keep request order inside and are ordered by their
// first member; IDs that are no longer live and visible are unavailable.
func (s *Store) PhotoStackGroups(ctx context.Context, ids []string, hidden bool) (groups [][]string, unavailable []string, err error) {
	if len(ids) > MaxPhotoStackGroupIDs {
		return nil, nil, ErrInvalidPhotoAsset
	}
	position := make(map[string]int, len(ids))
	for i, id := range ids {
		if _, duplicate := position[id]; duplicate || validateUUIDv4(id) != nil {
			return nil, nil, ErrInvalidPhotoAsset
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
		frames, err := photoStackFrames(ctx, tx, ids, hidden)
		if err != nil {
			return err
		}
		unavailable = []string{}
		for _, id := range ids {
			if _, ok := frames[id]; !ok {
				unavailable = append(unavailable, id)
			}
		}
		groups = photoStackRequestGroups(slices.Collect(maps.Values(frames)), settings.BurstGapSeconds, position)
		return nil
	})
	return groups, unavailable, err
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
	Hidden      bool
	AssetIDs    []string
	Targets     []PhotoAuthoredEditTarget
	Decision    string
	CurrentID   string
	SelectedIDs []string
}

// DecidePhotoStack applies one decision to an observed stack in one
// transaction. The submitted members must still form one stack under the
// stored folder and capture keys, and every file must keep its observed revision.
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
	unique := slices.Clone(request.AssetIDs)
	slices.Sort(unique)
	if len(slices.Compact(unique)) != len(request.AssetIDs) {
		return PhotoAuthoredReceipt{}, ErrInvalidPhotoAsset
	}
	var result PhotoAuthoredReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if request.Hidden {
			if _, err := s.hiddenSession(ctx, tx); err != nil {
				return err
			}
		}
		settings, err := photoSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		// Only the observed query or frozen run defines membership, so unseen
		// neighboring imports stay untouched.
		found, err := photoStackFrames(ctx, tx, request.AssetIDs, request.Hidden)
		if err != nil {
			return err
		}
		if len(found) != len(request.AssetIDs) {
			return ErrStalePhotoStack
		}
		frames := slices.SortedFunc(maps.Values(found), comparePhotoStackFrames)
		for i := 1; i < len(frames); i++ {
			if !photoStackChained(frames[i-1], frames[i], settings.BurstGapSeconds) {
				return ErrStalePhotoStack
			}
		}
		// Prepare every live source file inside the transaction, so a client
		// cannot omit a sibling or one half of a RAW/JPEG pair.
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
			wanted, ok := expected[target.FileID]
			if !ok {
				return ErrStalePhotoStack
			}
			if wanted.PhotoAuthoredTargetRevision != target.PhotoAuthoredTargetRevision {
				return ErrStaleRevision
			}
			if wanted.ContentVersionID != "" {
				var current string
				if err := tx.QueryRowContext(ctx, `SELECT current_version_id FROM nodes WHERE id=?`, target.NodeID).Scan(&current); err != nil {
					return err
				}
				if current != wanted.ContentVersionID || target.DisplayFileID != target.FileID {
					return ErrStaleRevision
				}
			}
		}
		if request.Decision == "representative" {
			result, err = s.choosePhotoRepresentativeTx(ctx, tx, newPhotoStack(frames), found[request.CurrentID])
			return err
		}
		flagged := map[string]bool{}
		for _, target := range preparation.Targets {
			if target.Values.Flag != "" {
				flagged[target.AssetID] = true
			}
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
			// Explicitly chosen frames become picks; automatic rejects leave flagged frames alone.
			flag := "pick"
			if !slices.Contains(chosen, target.AssetID) {
				if flagged[target.AssetID] {
					continue
				}
				flag = "reject"
			}
			targets = append(targets, PhotoAuthoredTarget{FileID: target.FileID, Revision: target.Revision, Patch: PhotoAuthoredPatch{Flag: new(flag)}})
		}
		if len(targets) == 0 {
			result = PhotoAuthoredReceipt{Before: []PhotoAuthoredSnapshot{}, After: []PhotoAuthoredSnapshot{}}
			return nil
		}
		result, err = s.applyPhotoAuthoredValuesTx(ctx, tx, targets, photoAuthoredApply{persist: true})
		return err
	})
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	return result, nil
}

// choosePhotoRepresentativeTx stores an explicit choice with the next rank.
// Flags stay unchanged, so every frame keeps its picks and rejects.
func (s *Store) choosePhotoRepresentativeTx(ctx context.Context, tx *sql.Tx, stack PhotoStack, frame photoStackFrame) (PhotoAuthoredReceipt, error) {
	if stack.RepresentativeID == frame.AssetID && frame.Rank > 0 {
		return PhotoAuthoredReceipt{Before: []PhotoAuthoredSnapshot{}, After: []PhotoAuthoredSnapshot{}}, nil
	}
	var rank int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(representative_rank),0)+1 FROM photo_assets WHERE representative_rank IS NOT NULL`).Scan(&rank); err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	change, err := setPhotoRepresentativeTx(ctx, tx, frame.AssetID, -1, &rank)
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	return persistPhotoRepresentativeReceiptTx(ctx, tx, PhotoAuthoredReceipt{Representatives: []PhotoRepresentativeChange{change}})
}

// setPhotoRepresentativeTx changes one asset's rank and revision. A
// non-negative revision is the precondition an undo checks.
func setPhotoRepresentativeTx(ctx context.Context, tx *sql.Tx, assetID string, revision int64, rank *int64) (PhotoRepresentativeChange, error) {
	change := PhotoRepresentativeChange{AssetID: assetID, After: rank}
	var before sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT revision,representative_rank FROM photo_assets WHERE asset_id=?`, assetID).Scan(&change.BeforeRevision, &before); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return change, ErrNotFound
		}
		return change, err
	}
	if revision >= 0 && change.BeforeRevision != revision {
		return change, ErrStaleRevision
	}
	if before.Valid {
		change.Before = new(before.Int64)
	}
	change.AfterRevision = change.BeforeRevision + 1
	_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET representative_rank=?,revision=?,updated_at=? WHERE asset_id=?`, nullablePhotoInt(rank), change.AfterRevision, nowRFC3339(), assetID)
	return change, err
}

func nullablePhotoInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// photoStackPager emits whole query-scoped stacks from the live capture-time
// keyset. A chain whose gap reach stays strictly inside the rows a page has
// read is grouped from those rows; a chain that touches an edge walks the
// folder index. Each component is resolved once per page.
type photoStackPager struct {
	q          metadataQuerier
	scope      photoBrowseScope
	gap        int
	descending bool
	probes     [2]*sql.Stmt
	args       []any
	components map[string]*photoStackComponent
}

type photoStackComponent struct {
	frames    []photoStackFrame
	oversized bool
}

// A probe reads every in-scope frame within this many gaps, so one query
// usually finishes a burst in one direction.
const photoStackProbeGaps = 8

const photoStackProbeFrom = `photo_timeline pl INDEXED BY photo_timeline_folder
 CROSS JOIN photo_files f ON f.file_id=pl.file_id
 CROSS JOIN photo_assets a ON a.asset_id=f.asset_id AND a.display_file_id=f.file_id
 CROSS JOIN nodes n ON n.id=f.node_id
 CROSS JOIN content_versions v ON v.version_id=n.current_version_id
 LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
 LEFT JOIN photo_technical_metadata p ON p.generation_id=h.generation_id`

func newPhotoStackPager(ctx context.Context, q metadataQuerier, scope photoBrowseScope, gap int, descending bool) (*photoStackPager, error) {
	preparer, ok := q.(interface {
		PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	})
	if !ok {
		return nil, errors.New("photo stack paging requires a statement-preparing connection")
	}
	pager := &photoStackPager{q: q, scope: scope, gap: gap, descending: descending, components: map[string]*photoStackComponent{}}
	// Each probe reads the in-scope frames after one frame, up to a window bound.
	for i, bounds := range [2]string{
		`pl.sort_key<=? AND pl.sort_key>=? AND (pl.sort_key<? OR a.asset_id<?) ORDER BY pl.sort_key DESC,a.asset_id DESC LIMIT ?`,
		`pl.sort_key>=? AND pl.sort_key<=? AND (pl.sort_key>? OR a.asset_id>?) ORDER BY pl.sort_key ASC,a.asset_id ASC LIMIT ?`,
	} {
		statement, args, err := scope.bind(`SELECT a.asset_id,pl.sort_key FROM ` + photoStackProbeFrom + ` WHERE ` + scope.where() + ` AND a.kind='photo' AND pl.folder_key=? AND ` + bounds)
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

// linked reports whether next continues the chain from current in the walk direction.
func (p *photoStackPager) linked(current, next photoStackFrame, forward bool) bool {
	if forward {
		return photoStackChained(current, next, p.gap)
	}
	return photoStackChained(next, current, p.gap)
}

// probe reads the in-scope frames after current in one direction, up to window.
func (p *photoStackPager) probe(ctx context.Context, current photoStackFrame, window string, forward bool) (frames []photoStackFrame, err error) {
	direction := 0
	if forward {
		direction = 1
	}
	args := append(slices.Clone(p.args), current.Folder, current.SortKey, window, current.SortKey, current.AssetID, MaxPhotoStackFrames+1)
	rows, err := p.probes[direction].QueryContext(ctx, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		next := photoStackFrame{Folder: current.Folder}
		if err := rows.Scan(&next.AssetID, &next.SortKey); err != nil {
			return nil, err
		}
		frames = append(frames, next)
	}
	return frames, rows.Err()
}

// walk appends frame's chain in one direction until it breaks or exceeds the cap.
func (p *photoStackPager) walk(ctx context.Context, frame photoStackFrame, forward bool, component *photoStackComponent) error {
	for current := frame; len(component.frames) <= MaxPhotoStackFrames; {
		window, ok := photoStackReach(current.SortKey, p.gap*photoStackProbeGaps, forward)
		if !ok {
			return nil
		}
		read, err := p.probe(ctx, current, window, forward)
		if err != nil {
			return err
		}
		for _, next := range read {
			if !p.linked(current, next, forward) {
				return nil
			}
			component.frames = append(component.frames, next)
			current = next
		}
		// Continue only while unread frames could still join: the window was
		// full, or the last frame's reach passes the window bound.
		reach, ok := photoStackReach(current.SortKey, p.gap, forward)
		if !ok || len(read) <= MaxPhotoStackFrames && (forward && reach <= window || !forward && reach >= window) {
			return nil
		}
	}
	return nil
}

// component walks frame's stack in both directions, stopping once it exceeds the cap.
func (p *photoStackPager) component(ctx context.Context, frame photoStackFrame) (*photoStackComponent, error) {
	if found := p.components[frame.AssetID]; found != nil {
		return found, nil
	}
	component := &photoStackComponent{frames: []photoStackFrame{frame}}
	for _, forward := range []bool{false, true} {
		if err := p.walk(ctx, frame, forward, component); err != nil {
			return nil, err
		}
	}
	p.remember(component)
	return component, nil
}

func (p *photoStackPager) remember(component *photoStackComponent) {
	component.oversized = len(component.frames) > MaxPhotoStackFrames
	slices.SortFunc(component.frames, comparePhotoStackFrames)
	for _, member := range component.frames {
		p.components[member.AssetID] = component
	}
}

// prime groups a raw batch in Go. The batch holds every in-scope row whose key
// lies strictly between its edges: after, the position it was read after, and
// its last known key unless the keyset ended. A chain whose gap reach stays
// inside both edges is complete; one that touches an edge is left for probes.
func (p *photoStackPager) prime(batch []PhotoBrowseRow, after *PhotoBrowsePosition, exhausted bool) {
	if after != nil && after.Missing {
		return
	}
	folders := map[string][]photoStackFrame{}
	open, last := !exhausted, ""
	for _, row := range batch {
		if row.position.Missing {
			open = false
			continue
		}
		last = row.position.Key
		if row.folderKey != "" {
			folders[row.folderKey] = append(folders[row.folderKey], photoStackFrame{AssetID: row.AssetID, Folder: row.folderKey, SortKey: row.position.Key})
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
		reach, ok := photoStackReach(frame.SortKey, p.gap, forward)
		return ok && (forward && reach < edge || !forward && reach > edge)
	}
	for _, frames := range folders {
		slices.SortFunc(frames, comparePhotoStackFrames)
		for first := 0; first < len(frames); {
			next := first + 1
			for next < len(frames) && photoStackChained(frames[next-1], frames[next], p.gap) {
				next++
			}
			chain := frames[first:next]
			first = next
			// A probed component already holds its members, including any
			// oversized run this batch saw only part of.
			if slices.ContainsFunc(chain, func(frame photoStackFrame) bool { return p.components[frame.AssetID] != nil }) {
				continue
			}
			if inside(chain[0], false) && inside(chain[len(chain)-1], true) {
				p.remember(&photoStackComponent{frames: slices.Clone(chain)})
			}
		}
	}
}

// classify returns the frames a raw row emits: the row alone, its whole stack
// at the first member in scan order, or nothing for any other member.
func (p *photoStackPager) classify(ctx context.Context, row PhotoBrowseRow) ([]photoStackFrame, error) {
	frame := photoStackFrame{AssetID: row.AssetID, Folder: row.folderKey, SortKey: row.position.Key}
	if frame.Folder == "" || row.position.Missing {
		return []photoStackFrame{frame}, nil
	}
	component, err := p.component(ctx, frame)
	if err != nil {
		return nil, err
	}
	if component.oversized || len(component.frames) == 1 {
		return []photoStackFrame{frame}, nil
	}
	first := component.frames[0]
	if p.descending {
		first = component.frames[len(component.frames)-1]
	}
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
		if err := rows.Scan(&frame.AssetID, &frame.Rank, &frame.Picked); err != nil {
			return err
		}
		inputs[frame.AssetID] = frame
		return nil
	})
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(unread, photoSelectionChunkSize) {
		statement, args, err := p.scope.bind(`SELECT `+photoBrowseSelect+`,pl.sort_key FROM `+photoBrowseDisplayFrom+` WHERE `+p.scope.where()+` AND a.asset_id IN (`+placeholders(len(chunk))+`)`, anyArgs(chunk)...)
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
		if len(group) == 1 {
			page.Items = append(page.Items, scanned[group[0].AssetID])
			continue
		}
		stack := make([]photoStackFrame, len(group))
		for i, frame := range group {
			input, ok := inputs[frame.AssetID]
			row, scannedOK := scanned[frame.AssetID]
			if !ok || !scannedOK {
				return errors.New("photo stack members changed inside one read snapshot")
			}
			stack[i] = photoStackFrame{AssetID: frame.AssetID, Folder: frame.Folder, SortKey: frame.SortKey, Rank: input.Rank, Picked: input.Picked}
			if !p.descending {
				page.Items = append(page.Items, row)
			}
		}
		if p.descending {
			for _, frame := range slices.Backward(group) {
				page.Items = append(page.Items, scanned[frame.AssetID])
			}
		}
		page.Stacks = append(page.Stacks, newPhotoStack(stack))
	}
	return nil
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
	readRaw func(*PhotoBrowsePosition, int) ([]PhotoBrowseRow, error), page *PhotoBrowsePage,
) (err error) {
	pager, err := newPhotoStackPager(ctx, q, scope, gap, descending)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, pager.Close()) }()
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
		pager.prime(batch, after, exhausted)
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
