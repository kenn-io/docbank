package hybrid

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/search/rrf"
	"go.kenn.io/kit/search/sqlquery"
)

// Leg is one retrieval query in rank order. Scan reads the current row and
// returns the fusion key. CandidateLimit is the bound the caller placed in
// Query. A RawWindow probe on Query decides whether the window was full;
// without one, zero means the bound is unknown and the window is not
// reported full.
type Leg[K rrf.Key] struct {
	Name           string
	Weight         float64
	Query          sqlquery.Query
	CandidateLimit int
	Scan           func(*sql.Rows) (K, error)
}

// GroupLeg scans a group key and one alternate member from each row.
type GroupLeg[G rrf.Key, M comparable] struct {
	Name           string
	Weight         float64
	Query          sqlquery.Query
	CandidateLimit int
	Scan           func(*sql.Rows) (G, M, error)
}

// Report describes one executed leg. FullWindow means the raw candidate
// window was filled before source filters, or, when the backend does not
// say, that the returned row count reached the candidate limit. It does
// not mean the source has no further matches.
type Report struct {
	Name           string
	Returned       int
	CandidateLimit int
	FullWindow     bool
}

// Result is the fused ranking plus per-leg window metadata.
type Result[K rrf.Key] struct {
	Hits []rrf.Hit[K]
	Legs []Report
}

// GroupResult keeps alternate members for a later eligibility check.
type GroupResult[G rrf.Key, M comparable] struct {
	Hits []rrf.GroupHit[G, M]
	Legs []Report
}

// Run executes legs sequentially through db and fuses their key order.
// k is the reciprocal-rank constant. Legs stop when ctx is cancelled.
// A scan or query error returns no fused result. k, leg names and weights
// follow rrf.ValidateLegs and are checked before any query runs.
func Run[K rrf.Key](ctx context.Context, db sqlquery.Queryer, k float64, legs []Leg[K]) (Result[K], error) {
	if db == nil {
		return Result[K]{}, errors.New("hybrid: database handle is required")
	}
	if err := rrf.ValidateLegs(k, len(legs), func(i int) (string, float64) {
		return legs[i].Name, legs[i].Weight
	}); err != nil {
		return Result[K]{}, fmt.Errorf("hybrid: %w", err)
	}
	ranked := make([]rrf.Leg[K], len(legs))
	reports := make([]Report, len(legs))
	for i, leg := range legs {
		if leg.Scan == nil {
			return Result[K]{}, fmt.Errorf("hybrid: leg %q has no scanner", leg.Name)
		}
		if leg.CandidateLimit < 0 {
			return Result[K]{}, fmt.Errorf("hybrid: leg %q candidate limit is negative", leg.Name)
		}
		keys, err := leg.Query.All(ctx, db, leg.Scan)
		if err != nil {
			return Result[K]{}, fmt.Errorf("hybrid: leg %q: %w", leg.Name, err)
		}
		ranked[i] = rrf.Leg[K]{Name: leg.Name, Weight: leg.Weight, Keys: keys}
		full, err := windowFull(ctx, db, leg.Query, leg.CandidateLimit, len(keys))
		if err != nil {
			return Result[K]{}, fmt.Errorf("hybrid: leg %q window: %w", leg.Name, err)
		}
		reports[i] = report(leg.Name, leg.CandidateLimit, len(keys), full)
	}
	hits, err := rrf.Fuse(k, ranked)
	if err != nil {
		return Result[K]{}, fmt.Errorf("hybrid: %w", err)
	}
	return Result[K]{Hits: hits, Legs: reports}, nil
}

// RunGroups executes legs that return group and member columns. Members stay
// attached to the fused group so a caller can drop ineligible alternates
// before choosing a representative.
func RunGroups[G rrf.Key, M comparable](ctx context.Context, db sqlquery.Queryer, k float64, legs []GroupLeg[G, M]) (GroupResult[G, M], error) {
	return runGroups(ctx, db, k, legs, rrf.FuseGroups[G, M])
}

// RunGroupsEvery executes group legs and keeps only groups that every leg
// found. Each leg is one required concept. Different members may carry the evidence
// for different legs. A leg with a full window may have missed a group, and
// then the intersection drops it too, so check AnyFullWindow before treating
// the result as complete.
func RunGroupsEvery[G rrf.Key, M comparable](ctx context.Context, db sqlquery.Queryer, k float64, legs []GroupLeg[G, M]) (GroupResult[G, M], error) {
	return runGroups(ctx, db, k, legs, rrf.FuseGroupsEvery[G, M])
}

// AnyFullWindow reports whether any leg filled its candidate window.
func (r GroupResult[G, M]) AnyFullWindow() bool {
	for _, leg := range r.Legs {
		if leg.FullWindow {
			return true
		}
	}
	return false
}

func runGroups[G rrf.Key, M comparable](
	ctx context.Context, db sqlquery.Queryer, k float64, legs []GroupLeg[G, M],
	fuse func(float64, []rrf.GroupLeg[G, M]) ([]rrf.GroupHit[G, M], error),
) (GroupResult[G, M], error) {
	if db == nil {
		return GroupResult[G, M]{}, errors.New("hybrid: database handle is required")
	}
	if err := rrf.ValidateLegs(k, len(legs), func(i int) (string, float64) {
		return legs[i].Name, legs[i].Weight
	}); err != nil {
		return GroupResult[G, M]{}, fmt.Errorf("hybrid: %w", err)
	}
	ranked := make([]rrf.GroupLeg[G, M], len(legs))
	reports := make([]Report, len(legs))
	for i, leg := range legs {
		if leg.Scan == nil {
			return GroupResult[G, M]{}, fmt.Errorf("hybrid: leg %q has no scanner", leg.Name)
		}
		if leg.CandidateLimit < 0 {
			return GroupResult[G, M]{}, fmt.Errorf("hybrid: leg %q candidate limit is negative", leg.Name)
		}
		type row struct {
			group  G
			member M
		}
		rows, err := leg.Query.All(ctx, db, func(rows *sql.Rows) (row, error) {
			group, member, err := leg.Scan(rows)
			return row{group: group, member: member}, err
		})
		if err != nil {
			return GroupResult[G, M]{}, fmt.Errorf("hybrid: leg %q: %w", leg.Name, err)
		}
		groups := make([]rrf.Group[G, M], 0, len(rows))
		for _, item := range rows {
			if len(groups) == 0 || groups[len(groups)-1].Key != item.group {
				groups = append(groups, rrf.Group[G, M]{Key: item.group})
			}
			last := len(groups) - 1
			groups[last].Members = append(groups[last].Members, item.member)
		}
		ranked[i] = rrf.GroupLeg[G, M]{Name: leg.Name, Weight: leg.Weight, Groups: groups}
		full, err := windowFull(ctx, db, leg.Query, leg.CandidateLimit, len(rows))
		if err != nil {
			return GroupResult[G, M]{}, fmt.Errorf("hybrid: leg %q window: %w", leg.Name, err)
		}
		reports[i] = report(leg.Name, leg.CandidateLimit, len(rows), full)
	}
	hits, err := fuse(k, ranked)
	if err != nil {
		return GroupResult[G, M]{}, fmt.Errorf("hybrid: %w", err)
	}
	return GroupResult[G, M]{Hits: hits, Legs: reports}, nil
}

func report(name string, limit, returned int, full bool) Report {
	return Report{
		Name:           name,
		Returned:       returned,
		CandidateLimit: limit,
		FullWindow:     full,
	}
}

func windowFull(ctx context.Context, db sqlquery.Queryer, query sqlquery.Query, limit, returned int) (bool, error) {
	if query.RawWindow != nil {
		return query.RawWindow(ctx, db)
	}
	return limit > 0 && returned >= limit, nil
}
