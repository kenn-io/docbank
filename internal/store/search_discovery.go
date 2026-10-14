package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	fuzzyCandidateLimit = 2048
	fuzzyNameRunes      = 512
	fuzzyAncestorLimit  = 64
	fuzzyPathBytes      = 4096
	fuzzyTimeBudget     = 200 * time.Millisecond
)

// Fuzzy receipt statuses and reasons. Reasons sort alphabetically.
const (
	FuzzyStatusComplete        = "complete"
	FuzzyStatusLimited         = "limited"
	FuzzyStatusNotRun          = "not_run"
	FuzzyReasonCandidateLimit  = "candidate_limit"
	FuzzyReasonNameLimit       = "name_limit"
	FuzzyReasonPathLimit       = "path_limit"
	FuzzyReasonQueryIneligible = "query_ineligible"
	FuzzyReasonResultLimit     = "result_limit"
	FuzzyReasonTimeBudget      = "time_budget"
)

// FuzzyReceipt describes coverage of the bounded supplemental matching rules.
// It is independent of truncation, which requires a known omitted match.
type FuzzyReceipt struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
}

type discoveryDeps struct {
	beginRead func(context.Context) (*sql.Tx, error)
	normalize func(context.Context, rowQuerier, SearchOptions) (SearchOptions, error)
	budget    func(context.Context) (context.Context, context.CancelFunc)
}

func (s *Store) discoveryDependencies() discoveryDeps {
	return discoveryDeps{
		beginRead: func(ctx context.Context) (*sql.Tx, error) {
			return s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		},
		normalize: s.normalizeSearchOptionsWithQuerier,
		budget: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, fuzzyTimeBudget)
		},
	}
}

// SearchDiscoveryPage preserves ordinary hits and fills remaining slots with
// scoped filename/path matches from one bounded supplemental read snapshot.
func (s *Store) SearchDiscoveryPage(
	ctx context.Context, query string, limit int, opts SearchOptions,
) ([]SearchHit, bool, FuzzyReceipt, error) {
	return s.searchDiscoveryPage(ctx, query, limit, opts, s.discoveryDependencies())
}

func (s *Store) searchDiscoveryPage(
	ctx context.Context, query string, limit int, opts SearchOptions, deps discoveryDeps,
) ([]SearchHit, bool, FuzzyReceipt, error) {
	receipt := FuzzyReceipt{Status: FuzzyStatusComplete, Reasons: []string{}}
	q, err := parseFuzzyQuery(query)
	if err != nil {
		return nil, false, receipt, err
	}
	if limit <= 0 {
		limit = 50
	}
	ordinary, truncated, err := s.SearchPageWithOptions(ctx, query, limit, opts)
	if err != nil {
		return nil, false, receipt, err
	}
	if len(ordinary) >= limit {
		return ordinary, truncated, notRun(FuzzyReasonResultLimit), nil
	}
	if !q.eligible {
		return ordinary, truncated, notRun(FuzzyReasonQueryIneligible), nil
	}
	// Acquisition and scope validation are work too; start the budget first.
	child, cancel := deps.budget(ctx)
	defer cancel()
	supplemental, reasons, err := s.discoverySnapshot(child, q, opts, ordinary, deps)
	if ctx.Err() != nil {
		return nil, false, receipt, ctx.Err()
	}
	// Cancellation can finish database/sql's automatic rollback before an
	// operation observes it, leaving ErrTxDone instead of the context error.
	if child.Err() != nil &&
		(err == nil || errors.Is(err, child.Err()) || errors.Is(err, sql.ErrTxDone)) {
		timedOut := FuzzyReceipt{FuzzyStatusLimited, []string{FuzzyReasonTimeBudget}}
		return ordinary, truncated, timedOut, nil
	}
	if err != nil {
		return nil, false, receipt, err
	}
	if len(reasons) > 0 {
		receipt.Status = FuzzyStatusLimited
		receipt.Reasons = reasons
	}
	remaining := limit - len(ordinary)
	if len(supplemental) > remaining {
		truncated = true
		supplemental = supplemental[:remaining]
	}
	return append(ordinary, supplemental...), truncated, receipt, nil
}

func notRun(reason string) FuzzyReceipt {
	return FuzzyReceipt{FuzzyStatusNotRun, []string{reason}}
}

type discoveryCandidate struct {
	hit           SearchHit
	rank          fuzzyRank
	components    []string // leaf first, accumulated only within the byte bound
	next          *int64
	bytes         int
	seen          map[int64]bool
	done, limited bool
}

func (s *Store) discoverySnapshot(
	ctx context.Context, q fuzzyQuery, opts SearchOptions, ordinary []SearchHit, deps discoveryDeps,
) ([]SearchHit, []string, error) {
	tx, err := deps.beginRead(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	opts, err = deps.normalize(ctx, tx, opts)
	if err != nil {
		return nil, nil, err
	}
	candidates, reasons, err := discoveryCandidates(ctx, tx, q, opts, ordinary)
	if err != nil {
		return nil, nil, err
	}
	if err := resolveDiscoveryPaths(ctx, tx, candidates); err != nil {
		return nil, nil, err
	}
	verified := make([]discoveryCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		// Leaf-first components become a root-first path or known suffix.
		slices.Reverse(candidate.components)
		if candidate.limited {
			if !fuzzySuffixRulesOut(q, candidate.components) {
				reasons = append(reasons, FuzzyReasonPathLimit)
			}
			continue
		}
		rank, ok := matchFuzzyComponents(q, candidate.components)
		if !ok {
			continue
		}
		candidate.rank = rank
		candidate.hit.Path = "/" + strings.Join(candidate.components, "/")
		verified = append(verified, candidate)
	}
	slices.SortFunc(verified, func(a, b discoveryCandidate) int {
		if a.rank.less(b.rank) {
			return -1
		}
		if b.rank.less(a.rank) {
			return 1
		}
		if n := strings.Compare(a.hit.Path, b.hit.Path); n != 0 {
			return n
		}
		if a.hit.Node.ID < b.hit.Node.ID {
			return -1
		}
		if a.hit.Node.ID > b.hit.Node.ID {
			return 1
		}
		return 0
	})
	hits := make([]SearchHit, len(verified))
	for i, c := range verified {
		hits[i] = c.hit
	}
	slices.Sort(reasons)
	reasons = slices.Compact(reasons)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return hits, reasons, nil
}

func discoveryCandidates(
	ctx context.Context, tx *sql.Tx, q fuzzyQuery, opts SearchOptions, ordinary []SearchHit,
) ([]discoveryCandidate, []string, error) {
	filter, args := searchFilterSQL(opts)
	args = append([]any{fuzzyAnchor(q)}, args...)
	args = append(args, fuzzyCandidateLimit+1)
	// Bound names in SQL before they cross the driver. The 513th rune is the
	// overflow sentinel; paths never use the unbounded ancestry helpers.
	before, after, found := strings.Cut(nodeCols, "n.name,")
	if !found {
		return nil, nil, errors.New("fuzzy candidate columns: node name column not found")
	}
	columns := before + "substr(n.name,1,513)," + after
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM `+nodeFrom+`
 JOIN nodes_fts ON nodes_fts.rowid=n.id
 WHERE nodes_fts MATCH ? AND n.trashed_at IS NULL AND n.parent_id IS NOT NULL `+filter+`
 ORDER BY n.id LIMIT ?`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[int64]bool, len(ordinary))
	for _, h := range ordinary {
		seen[h.Node.ID] = true
	}
	var candidates []discoveryCandidate
	var reasons []string
	terminal := fuzzyQuery{components: [][]string{q.components[len(q.components)-1]}}
	source := "fuzzy_name"
	if q.rooted || len(q.components) > 1 {
		source = "fuzzy_path"
	}
	count := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		count++
		if count > fuzzyCandidateLimit {
			reasons = append(reasons, FuzzyReasonCandidateLimit)
			break
		}
		n, err := scanNode(rows)
		if err != nil {
			return nil, nil, err
		}
		if utf8.RuneCountInString(n.Name) > fuzzyNameRunes {
			reasons = append(reasons, FuzzyReasonNameLimit)
			continue
		}
		if seen[n.ID] {
			continue
		}
		if _, ok := matchFuzzyComponents(terminal, []string{n.Name}); !ok {
			continue
		}
		candidates = append(candidates, discoveryCandidate{
			hit: SearchHit{Node: n, Match: source}, components: []string{n.Name},
			next: n.ParentID, bytes: len(n.Name) + 1, seen: map[int64]bool{n.ID: true},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return candidates, reasons, nil
}

type discoveryAncestor struct {
	id     int64
	parent *int64
	name   string
}

// Read one batched ancestry layer at a time. Each candidate retains at most
// 4096 bytes, and each SQL name at most 513 runes, even under huge ancestors.
func resolveDiscoveryPaths(ctx context.Context, tx *sql.Tx, candidates []discoveryCandidate) error {
	for range fuzzyAncestorLimit {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids := []int64{}
		unique := map[int64]bool{}
		for _, c := range candidates {
			if !c.done && !c.limited && c.next != nil && !unique[*c.next] {
				unique[*c.next] = true
				ids = append(ids, *c.next)
			}
		}
		if len(ids) == 0 {
			break
		}
		ancestors := make(map[int64]discoveryAncestor, len(ids))
		// Stay below SQLite's historical bind-variable limit in both drivers.
		for offset := 0; offset < len(ids); offset += 400 {
			end := min(offset+400, len(ids))
			args := make([]any, end-offset)
			for i, id := range ids[offset:end] {
				args[i] = id
			}
			if err := readDiscoveryAncestorBatch(ctx, tx, args, ancestors); err != nil {
				return err
			}
		}
		for i := range candidates {
			c := &candidates[i]
			if c.done || c.limited || c.next == nil {
				continue
			}
			a, ok := ancestors[*c.next]
			if !ok {
				return fmt.Errorf("fuzzy path ancestor %d: %w", *c.next, ErrNotFound)
			}
			if c.seen[a.id] {
				return errors.New("cycle in fuzzy path ancestry")
			}
			c.seen[a.id] = true
			if a.parent == nil {
				if a.name != "" {
					return errors.New("invalid fuzzy path root")
				}
				c.done = true
				continue
			}
			if utf8.RuneCountInString(a.name) > fuzzyNameRunes || c.bytes+len(a.name)+1 > fuzzyPathBytes {
				c.limited = true
				continue
			}
			c.components = append(c.components, a.name)
			c.bytes += len(a.name) + 1
			c.next = a.parent
		}
	}
	for i := range candidates {
		if !candidates[i].done {
			candidates[i].limited = true
		}
	}
	return nil
}

func readDiscoveryAncestorBatch(
	ctx context.Context, tx *sql.Tx, args []any, ancestors map[int64]discoveryAncestor,
) (err error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := tx.QueryContext(ctx,
		`SELECT id,parent_id,substr(name,1,513) FROM nodes WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	// Close before the next batch, including on a failed Scan. Preserve both
	// iteration and close errors without accumulating deferred open cursors.
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var a discoveryAncestor
		if err := rows.Scan(&a.id, &a.parent, &a.name); err != nil {
			return err
		}
		ancestors[a.id] = a
	}
	return rows.Err()
}
