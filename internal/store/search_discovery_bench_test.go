package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// Fixed synthetic fixture: sequential IDs, invoice candidates, a selective
// z-prefix target under cases/scoped, and twenty ordinary exact-page hits.
// The corpus is metadata only; seeding never enters timed loops.
func benchmarkDiscoveryFixture(b *testing.B, count int) (*Store, int64, int64, int64) {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	tx, err := s.writeDB.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	seeded := 1
	insert := func(parent int64, name string) int64 {
		seeded++
		result, e := tx.ExecContext(b.Context(), `INSERT INTO nodes(parent_id,name,kind,revision,created_at,modified_at) VALUES(?,?,'dir',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, parent, name)
		if e != nil {
			b.Fatal(e)
		}
		id, e := result.LastInsertId()
		if e != nil {
			b.Fatal(e)
		}
		return id
	}
	cases := insert(s.RootID(), "cases")
	scope := insert(cases, "scoped")
	target := insert(scope, "zreport.pdf")
	otherScopes := make([]int64, 8)
	for i := range otherScopes {
		otherScopes[i] = insert(cases, fmt.Sprintf("region-%d", i))
	}
	for i := range 25 {
		insert(scope, fmt.Sprintf("invoice-scoped-%05d", i))
	}
	deepParent := scope
	for i := range 20 {
		deepParent = insert(deepParent, fmt.Sprintf("deep-%d", i))
	}
	deepTarget := insert(deepParent, "dreport.pdf")
	for i := range 20 {
		insert(s.RootID(), fmt.Sprintf("ordinary-%05d", i))
	}
	for i := 0; seeded < count; i++ {
		insert(otherScopes[i%len(otherScopes)], fmt.Sprintf("invoice-%05d", i))
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return s, scope, target, deepTarget
}

var benchmarkDiscoveryHits []SearchHit

func BenchmarkSearchDiscovery(b *testing.B) {
	for _, size := range []int{1000, 10000, 50000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			s, scope, target, deepTarget := benchmarkDiscoveryFixture(b, size)
			ctx := b.Context()
			// Core recall oracles isolate semantics from scheduling. Timed calls
			// below still use the public 200 ms budget and report actual recall.
			deps := s.discoveryDependencies()
			deps.budget = context.WithCancel
			discover := func(query string, limit int, opts SearchOptions) ([]SearchHit, bool, FuzzyReceipt, error) {
				return s.searchDiscoveryPage(ctx, query, limit, opts, deps)
			}
			ordinary, oldTr, err := s.SearchPageWithOptions(ctx, "ordinary", 10, SearchOptions{})
			if err != nil {
				b.Fatal(err)
			}
			exact, tr, r, err := discover("ordinary", 10, SearchOptions{})
			if err != nil {
				b.Fatal(err)
			}
			if !reflect.DeepEqual(ordinary, exact) || tr != oldTr || r.Status != "not_run" {
				b.Fatal("ordinary/exact-page identity changed")
			}
			scopedHits, _, scopedReceipt, scopedErr := discover("invocie", 100, SearchOptions{UnderNodeID: scope})
			if scopedErr != nil || scopedReceipt.Status != "complete" || len(scopedHits) != 25 {
				b.Fatalf("scoped invoice oracle: hits=%d receipt=%+v error=%v", len(scopedHits), scopedReceipt, scopedErr)
			}
			if size == 1000 {
				common, _, coverage, err := discover("invocie", size, SearchOptions{})
				if err != nil || coverage.Status != "complete" || len(common) != size-53 {
					b.Fatalf("uncapped common-prefix oracle: hits=%d expected=%d receipt=%+v error=%v", len(common), size-53, coverage, err)
				}
			}
			if size == 50000 {
				benchmarkDiscoveryPlan(b, s)
			}
			for _, tc := range []struct {
				name, query         string
				opts                SearchOptions
				ordinary, selective bool
				targetID            int64
			}{
				{name: "ordinary", query: "ordinary", ordinary: true},
				{name: "exact-full", query: "ordinary"},
				{name: "selective", query: "zreprot", selective: true},
				{name: "common-prefix", query: "invocie"},
				{name: "path", query: "cases/scoped/zreprot", selective: true},
				{name: "scoped", query: "zreprot", opts: SearchOptions{UnderNodeID: scope}, selective: true},
				{name: "scoped-common", query: "invocie", opts: SearchOptions{UnderNodeID: scope}},
				{name: "deep-path", query: "deep-19/dreprot", selective: true, targetID: deepTarget},
			} {
				b.Run(tc.name, func(b *testing.B) {
					if tc.targetID == 0 {
						tc.targetID = target
					}
					if tc.ordinary {
						// Match the pinned-base harness's final untimed ordinary
						// call after other discovery oracles have touched the cache.
						h, truncated, err := s.SearchPageWithOptions(ctx, tc.query, 10, tc.opts)
						if err != nil || len(h) != 10 || !truncated {
							b.Fatalf("ordinary warmup oracle: hits=%d truncated=%v error=%v", len(h), truncated, err)
						}
					}
					if tc.selective {
						h, _, receipt, e := discover(tc.query, 10, tc.opts)
						if e != nil || len(h) != 1 || h[0].Node.ID != tc.targetID || receipt.Status != "complete" {
							b.Fatalf("selective oracle target=%d: hits=%v receipt=%+v error=%v", tc.targetID, h, receipt, e)
						}
					}
					b.ReportAllocs()
					b.ResetTimer()
					limited, timeouts, candidateLimits, returnedHits, targetHits := 0, 0, 0, 0, 0
					for range b.N {
						if tc.ordinary {
							hits, _, e := s.SearchPageWithOptions(ctx, tc.query, 10, tc.opts)
							if e != nil {
								b.Fatal(e)
							}
							benchmarkDiscoveryHits = hits
							returnedHits += len(hits)
							continue
						}
						hits, _, receipt, e := s.SearchDiscoveryPage(ctx, tc.query, 10, tc.opts)
						if e != nil {
							b.Fatal(e)
						}
						benchmarkDiscoveryHits = hits
						returnedHits += len(hits)
						if tc.selective {
							for _, hit := range hits {
								if hit.Node.ID != tc.targetID {
									b.Fatalf("unexpected selective hit: got %d want %d", hit.Node.ID, tc.targetID)
								}
							}
							if len(hits) > 1 || (receipt.Status == "complete" && len(hits) != 1) {
								b.Fatalf("invalid selective coverage: hits=%d receipt=%+v", len(hits), receipt)
							}
							targetHits += len(hits)
						}
						if receipt.Status == "limited" {
							limited++
						}
						for _, reason := range receipt.Reasons {
							if reason == "candidate_limit" {
								candidateLimits++
							}
							if reason == "time_budget" {
								timeouts++
							}
						}
					}
					b.ReportMetric(float64(returnedHits)/float64(b.N), "hits/op")
					if tc.selective {
						b.ReportMetric(float64(targetHits)/float64(b.N), "target_recall/op")
					}
					b.ReportMetric(float64(limited)/float64(b.N), "limited/op")
					b.ReportMetric(float64(timeouts)/float64(b.N), "timeouts/op")
					b.ReportMetric(float64(candidateLimits)/float64(b.N), "candidate_limits/op")
				})
			}
		})
	}
}

func BenchmarkFuzzy(b *testing.B) {
	q, err := parseFuzzyQuery("invocie")
	if err != nil {
		b.Fatal(err)
	}
	candidates := []string{"invoice.pdf", "impossible.pdf", "invocie.pdf", "invoices.pdf", "invoice2.pdf", "invxixe.pdf"}
	b.ReportAllocs()
	for range b.N {
		for _, c := range candidates {
			_, _ = matchFuzzyComponents(q, []string{c})
		}
	}
}

func benchmarkDiscoveryPlan(b *testing.B, s *Store) {
	b.Helper()
	rows, err := s.db.QueryContext(b.Context(), `EXPLAIN QUERY PLAN SELECT n.id FROM `+nodeFrom+` JOIN nodes_fts ON nodes_fts.rowid=n.id WHERE nodes_fts MATCH ? AND n.trashed_at IS NULL AND n.parent_id IS NOT NULL ORDER BY n.id LIMIT 2049`, `"i"*`)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			b.Error(err)
		}
	}()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			b.Fatal(err)
		}
		b.Log("candidate SQL plan:", detail)
	}
	if err := rows.Err(); err != nil {
		b.Fatal(err)
	}
}
