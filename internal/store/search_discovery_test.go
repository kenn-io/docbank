package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func discoveryFile(t *testing.T, s *Store, parent int64, name string) Node {
	t.Helper()
	n, err := s.CreateFile(t.Context(), parent, name, fakeHash("ad"), 4, "application/pdf")
	require.NoError(t, err)
	return n
}

func TestSearchDiscoveryPrecedence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	name := discoveryFile(t, s, s.RootID(), "invocie.pdf")
	content, err := s.CreateFile(ctx, s.RootID(), "memo.txt", fakeHash("bc"), 4, "text/plain")
	require.NoError(t, err)
	typo := discoveryFile(t, s, s.RootID(), "invoice.pdf")
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: content.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "invocie"}))
	ordinary, oldTruncated, err := s.SearchPageWithOptions(ctx, "invocie", 20, SearchOptions{})
	require.NoError(t, err)
	hits, truncated, receipt, err := discoverUnbudgeted(t, s, "invocie", 20, SearchOptions{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(hits), len(ordinary))
	require.Equal(t, ordinary, hits[:len(ordinary)])
	require.Equal(t, oldTruncated, truncated)
	require.Equal(t, "complete", receipt.Status)
	require.NotNil(t, receipt.Reasons)
	require.Empty(t, receipt.Reasons)
	// Exact names and extracted text retain their order ahead of the typo.
	require.Equal(t, name.ID, hits[0].Node.ID)
	seen := map[int64]bool{}
	for _, h := range hits {
		require.False(t, seen[h.Node.ID])
		seen[h.Node.ID] = true
	}
	require.True(t, seen[typo.ID])
	for _, limit := range []int{1, 2} {
		ordinary, tr, err := s.SearchPageWithOptions(ctx, "invocie", limit, SearchOptions{})
		require.NoError(t, err)
		hits, gotTr, r, err := discoverUnbudgeted(t, s, "invocie", limit, SearchOptions{})
		require.NoError(t, err)
		require.Equal(t, ordinary, hits)
		require.Equal(t, tr, gotTr)
		require.Equal(t, FuzzyReceipt{"not_run", []string{"result_limit"}}, r)
	}
}

func TestSearchDiscoverySupplementalAndPaths(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	nested, err := s.Mkdir(ctx, s.RootID(), "資料")
	require.NoError(t, err)
	cases, err := s.Mkdir(ctx, nested.ID, "cases")
	require.NoError(t, err)
	acme, err := s.Mkdir(ctx, cases.ID, "acme")
	require.NoError(t, err)
	target := discoveryFile(t, s, acme.ID, "invoice.pdf")
	for _, query := range []string{"invocie", "cases/acme/invocie", "caess/acme/invoice"} {
		hits, tr, r, err := discoverUnbudgeted(t, s, query, 20, SearchOptions{})
		require.NoError(t, err)
		require.False(t, tr)
		require.Equal(t, "complete", r.Status)
		require.Len(t, hits, 1)
		require.Equal(t, target.ID, hits[0].Node.ID)
		require.Equal(t, "/資料/cases/acme/invoice.pdf", hits[0].Path)
		source := "fuzzy_name"
		if strings.Contains(query, "/") {
			source = "fuzzy_path"
		}
		require.Equal(t, source, hits[0].Match)
	}
	for _, query := range []string{"/cases/acme/invoice", "caess/acme/invocie"} {
		h, _, _, err := discoverUnbudgeted(t, s, query, 20, SearchOptions{})
		require.NoError(t, err)
		require.Empty(t, h)
	}
	// Ineligible supplemental queries must still preserve ordinary hits.
	for _, query := range []string{"inv", "資料"} {
		ordinary, ordinaryTruncated, err := s.SearchPageWithOptions(ctx, query, 20, SearchOptions{})
		require.NoError(t, err)
		require.NotEmpty(t, ordinary)
		hits, truncated, receipt, err := discoverUnbudgeted(t, s, query, 20, SearchOptions{})
		require.NoError(t, err)
		require.Equal(t, ordinary, hits)
		require.Equal(t, ordinaryTruncated, truncated)
		require.Equal(t, FuzzyReceipt{"not_run", []string{"query_ineligible"}}, receipt)
	}
	_, _, r, err := discoverUnbudgeted(t, s, "ab", 20, SearchOptions{})
	require.NoError(t, err)
	require.Equal(t, FuzzyReceipt{"not_run", []string{"query_ineligible"}}, r)
	blank, _, _, err := discoverUnbudgeted(t, s, " ", 20, SearchOptions{})
	require.ErrorIs(t, err, ErrFuzzyQuery)
	require.Nil(t, blank)
	// Text outside the supplemental syntax keeps ordinary search working.
	for _, query := range []string{"acme/", "acme//", "../acme"} {
		ordinary, _, err := s.SearchPageWithOptions(ctx, query, 20, SearchOptions{})
		require.NoError(t, err)
		require.NotEmpty(t, ordinary, query)
		hits, _, receipt, err := discoverUnbudgeted(t, s, query, 20, SearchOptions{})
		require.NoError(t, err)
		require.Equal(t, ordinary, hits)
		require.Equal(t, FuzzyReceipt{"not_run", []string{"query_ineligible"}}, receipt)
	}
}

// The FTS anchor is only a safe candidate superset when SQLite splits every
// matcher-comparable name into the same words as fuzzyWords.
func TestFuzzyComparableNamesMatchFTSTokens(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	names := []string{
		"Screenshot 2026-01-01 at 10.00.00\u202fAM.png", "invoice – final.pdf",
		"Invoice’s copy.pdf", "a©b€c·d\u200be\U0001F600f.txt", "plain_name-2026.txt",
		"café.pdf", "cas\u0300t.pdf", "m²n.pdf", "oⅠp.pdf", "q\ue000r.pdf", "w\ufe0fx.pdf",
	}
	ids := map[int64]string{}
	for _, name := range names {
		ids[discoveryFile(t, s, s.RootID(), name).ID] = name
	}
	_, err := s.writeDB.ExecContext(ctx,
		`CREATE VIRTUAL TABLE temp.nodes_fts_terms USING fts5vocab(main, nodes_fts, instance)`)
	require.NoError(t, err)
	rows, err := s.writeDB.QueryContext(ctx,
		`SELECT doc, term FROM temp.nodes_fts_terms ORDER BY doc, offset`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	terms := map[int64][]string{}
	for rows.Next() {
		var doc int64
		var term string
		require.NoError(t, rows.Scan(&doc, &term))
		terms[doc] = append(terms[doc], term)
	}
	require.NoError(t, rows.Err())
	compared := 0
	for id, name := range ids {
		if fuzzyComparable(name) {
			compared++
			require.Equal(t, fuzzyWords(name), terms[id], name)
		}
	}
	require.Equal(t, 5, compared)
}

func TestSearchDiscoveryNonASCIISeparators(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	screenshot := discoveryFile(t, s, s.RootID(), "Screenshot 2026-01-01 at 10.00.00\u202fAM.png")
	hits, _, receipt, err := discoverUnbudgeted(t, s, "screnshot", 20, SearchOptions{})
	require.NoError(t, err)
	require.Equal(t, "complete", receipt.Status)
	require.Len(t, hits, 1)
	require.Equal(t, screenshot.ID, hits[0].Node.ID)
}

func TestSearchDiscoveryScopeAndSnapshot(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	scope, err := s.Mkdir(ctx, s.RootID(), "cases")
	require.NoError(t, err)
	target := discoveryFile(t, s, scope.ID, "invoice.pdf")
	discoveryFile(t, s, s.RootID(), "invoice-other.pdf")
	tag, err := s.CreateTag(ctx, "reviewed")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, target.ID, target.Revision)
	require.NoError(t, err)
	opts := SearchOptions{UnderNodeID: scope.ID, TagID: tag.ID, MIMEType: "APPLICATION/PDF", ModifiedSince: target.ModifiedAt, ModifiedBefore: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), ContentVersionIDs: []string{target.CurrentVersionID}}
	h, _, _, err := discoverUnbudgeted(t, s, "invocie", 20, opts)
	require.NoError(t, err)
	require.Len(t, h, 1)
	require.Equal(t, target.ID, h[0].Node.ID)
	opts.ModifiedBefore = target.ModifiedAt
	opts.ModifiedSince = ""
	h, _, _, err = discoverUnbudgeted(t, s, "invocie", 20, opts)
	require.NoError(t, err)
	require.Empty(t, h)
	h, _, _, err = discoverUnbudgeted(t, s, "invocie", 20, SearchOptions{UnderNodeID: target.ID})
	require.Nil(t, h)
	require.ErrorIs(t, err, ErrNotDir)
	// Establish the supplemental transaction, mutate via the real writer, then
	// let candidate selection and paths finish inside the existing snapshot.
	deps := s.discoveryDependencies()
	// The injected writer proves snapshot isolation, not deadline performance.
	deps.budget = context.WithCancel
	deps.normalize = func(child context.Context, q rowQuerier, o SearchOptions) (SearchOptions, error) {
		normalized, e := s.normalizeSearchOptionsWithQuerier(child, q, o)
		if e != nil {
			return normalized, e
		}
		var count int
		require.NoError(t, q.QueryRowContext(child, "SELECT count(*) FROM nodes").Scan(&count))
		current, e := s.NodeByID(ctx, scope.ID)
		require.NoError(t, e)
		_, _, e = s.Move(ctx, scope.ID, s.RootID(), "renamed", current.Revision)
		require.NoError(t, e)
		return normalized, nil
	}
	h, _, _, err = s.searchDiscoveryPage(ctx, "invocie", 20, SearchOptions{UnderNodeID: scope.ID}, deps)
	require.NoError(t, err)
	require.Len(t, h, 1)
	require.Equal(t, "/cases/invoice.pdf", h[0].Path)
	h, _, _, err = discoverUnbudgeted(t, s, "invocie", 20, SearchOptions{UnderNodeID: scope.ID})
	require.NoError(t, err)
	require.Equal(t, "/renamed/invoice.pdf", h[0].Path)
	current, err := s.NodeByID(ctx, target.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, target.ID, current.Revision)
	require.NoError(t, err)
	h, _, _, err = discoverUnbudgeted(t, s, "invocie", 20, SearchOptions{UnderNodeID: scope.ID})
	require.NoError(t, err)
	require.Empty(t, h)
}

// A child deadline while normalizing is covered by
// TestSearchDiscoveryRollbackOutcomes. Production never cancels the child
// before discovery returns, so these cases only expire or cancel the parent.
func TestSearchDiscoveryCancellation(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"expired", "parent", "validation"} {
		t.Run(phase, func(t *testing.T) {
			s := newTestStore(t)
			parent, cancelParent := context.WithCancel(t.Context())
			defer cancelParent()
			discoveryFile(t, s, s.RootID(), "invoice.pdf")
			discoveryFile(t, s, s.RootID(), "invocie.txt")
			ordinary, ordinaryTruncated, err := s.SearchPageWithOptions(parent, "invocie", 20, SearchOptions{})
			require.NoError(t, err)
			require.NotEmpty(t, ordinary, "timeout preservation needs an ordinary hit")
			deps := s.discoveryDependencies()
			deps.budget = func(ctx context.Context) (context.Context, context.CancelFunc) {
				if phase == "expired" {
					child, cancel := context.WithDeadline(ctx, time.Time{})
					require.ErrorIs(t, child.Err(), context.DeadlineExceeded)
					return child, cancel
				}
				return context.WithTimeout(ctx, time.Minute)
			}
			// Snapshot acquisition and scope validation spend the budget too.
			check := func(child context.Context) {
				require.NotEqual(t, parent, child)
				_, ok := child.Deadline()
				require.True(t, ok)
			}
			if phase != "expired" {
				deps.beginRead = func(child context.Context) (*sql.Tx, error) {
					check(child)
					return s.db.BeginTx(child, &sql.TxOptions{ReadOnly: true})
				}
				deps.normalize = func(child context.Context, q rowQuerier, o SearchOptions) (SearchOptions, error) {
					check(child)
					if phase == "validation" {
						return SearchOptions{}, ErrNotDir
					}
					cancelParent()
					var value int
					if e := q.QueryRowContext(child, "SELECT 1").Scan(&value); e != nil {
						return SearchOptions{}, e
					}
					return s.normalizeSearchOptionsWithQuerier(child, q, o)
				}
			}
			h, truncated, r, err := s.searchDiscoveryPage(parent, "invocie", 20, SearchOptions{}, deps)
			switch phase {
			case "parent":
				require.ErrorIs(t, err, context.Canceled)
			case "validation":
				require.ErrorIs(t, err, ErrNotDir)
			default:
				require.NoError(t, err)
				require.Equal(t, ordinary, h)
				require.Equal(t, ordinaryTruncated, truncated)
				require.Equal(t, FuzzyReceipt{"limited", []string{"time_budget"}}, r)
			}
		})
	}
}

func TestSearchDiscoveryRollbackOutcomes(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"child_deadline", "parent_cancellation", "unrelated_storage"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newTestStore(t)
				parent, cancelParent := context.WithCancel(t.Context())
				defer cancelParent()
				discoveryFile(t, s, s.RootID(), "invoice.pdf")
				discoveryFile(t, s, s.RootID(), "invocie.txt")
				ordinary, ordinaryTruncated, err := s.SearchPageWithOptions(parent, "invocie", 20, SearchOptions{})
				require.NoError(t, err)
				require.NotEmpty(t, ordinary)
				deps := s.discoveryDependencies()
				deps.normalize = func(child context.Context, q rowQuerier, _ SearchOptions) (SearchOptions, error) {
					switch outcome {
					case "parent_cancellation":
						cancelParent()
					default:
						// Fake time advances to the real discovery deadline without a sleep.
						<-child.Done()
						require.ErrorIs(t, child.Err(), context.DeadlineExceeded)
					}
					// Let database/sql finish its automatic rollback before observing
					// the transaction. A live query context exposes ErrTxDone instead
					// of short-circuiting at the canceled query context's preflight.
					synctest.Wait()
					var value int
					txErr := q.QueryRowContext(context.WithoutCancel(parent), "SELECT 1").Scan(&value)
					require.ErrorIs(t, txErr, sql.ErrTxDone)
					if outcome == "unrelated_storage" {
						return SearchOptions{}, ErrNotDir
					}
					return SearchOptions{}, fmt.Errorf("normalizing scope: %w", txErr)
				}
				hits, truncated, receipt, err := s.searchDiscoveryPage(parent, "invocie", 20, SearchOptions{}, deps)
				switch outcome {
				case "parent_cancellation":
					require.ErrorIs(t, err, context.Canceled)
				case "unrelated_storage":
					require.ErrorIs(t, err, ErrNotDir)
				default:
					require.NoError(t, err)
					require.Equal(t, ordinary, hits)
					require.Equal(t, ordinaryTruncated, truncated)
					require.Equal(t, FuzzyReceipt{Status: "limited", Reasons: []string{"time_budget"}}, receipt)
				}
			})
		})
	}
}

// Direct bounded inserts keep synthetic cap fixtures fast and free of blobs.
func discoveryNames(t *testing.T, s *Store, parent int64, count int) {
	t.Helper()
	tx, err := s.writeDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(t.Context(), `INSERT INTO nodes(parent_id,name,kind,revision,created_at,modified_at) VALUES(?,?,'dir',1,?,?)`)
	require.NoError(t, err)
	defer func() { _ = stmt.Close() }()
	for i := range count {
		_, err = stmt.ExecContext(t.Context(), parent, fmt.Sprintf("invoice-%04d", i), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

// Recall tests use no deadline so a loaded host cannot turn them into
// time_budget receipts. TestSearchDiscoveryCancellation and
// TestSearchDiscoveryRollbackOutcomes cover the deadline itself.
func discoverUnbudgeted(
	t *testing.T, s *Store, query string, limit int, opts SearchOptions,
) ([]SearchHit, bool, FuzzyReceipt, error) {
	t.Helper()
	deps := s.discoveryDependencies()
	deps.budget = context.WithCancel
	return s.searchDiscoveryPage(t.Context(), query, limit, opts, deps)
}

func TestSearchDiscoveryLimits(t *testing.T) {
	t.Parallel()
	t.Run("candidate", func(t *testing.T) {
		s := newTestStore(t)
		discoveryNames(t, s, s.RootID(), 2048)
		h, tr, r, err := discoverUnbudgeted(t, s, "invocie", 1, SearchOptions{})
		require.NoError(t, err)
		require.Len(t, h, 1)
		require.True(t, tr)
		require.Equal(t, "complete", r.Status)
		discoveryFile(t, s, s.RootID(), "invoice-tail.pdf")
		_, tr, r, err = discoverUnbudgeted(t, s, "invocie", 1, SearchOptions{})
		require.NoError(t, err)
		require.True(t, tr)
		require.Equal(t, FuzzyReceipt{"limited", []string{"candidate_limit"}}, r)
		scoped, err := s.Mkdir(t.Context(), s.RootID(), "scoped")
		require.NoError(t, err)
		target := discoveryFile(t, s, scoped.ID, "invoice.pdf")
		h, _, r, err = discoverUnbudgeted(t, s, "invocie", 20, SearchOptions{UnderNodeID: scoped.ID})
		require.NoError(t, err)
		require.Len(t, h, 1)
		require.Equal(t, target.ID, h[0].Node.ID)
		require.Equal(t, "complete", r.Status)
	})
	t.Run("name", func(t *testing.T) {
		s := newTestStore(t)
		discoveryFile(t, s, s.RootID(), "invoice-"+strings.Repeat("a", 504))
		discoveryFile(t, s, s.RootID(), "invoice-"+strings.Repeat("a", 505))
		h, _, r, err := discoverUnbudgeted(t, s, "invocie", 20, SearchOptions{})
		require.NoError(t, err)
		require.Len(t, h, 1)
		require.Equal(t, FuzzyReceipt{"limited", []string{"name_limit"}}, r)
	})
	t.Run("ancestors", func(t *testing.T) {
		s := newTestStore(t)
		parent := s.RootID()
		for i := range 64 {
			n, err := s.Mkdir(t.Context(), parent, fmt.Sprintf("a%d", i))
			require.NoError(t, err)
			parent = n.ID
			if i == 62 {
				discoveryFile(t, s, parent, "invoice-ok.pdf")
			}
		}
		discoveryFile(t, s, parent, "invoice-deep.pdf")
		h, _, r, err := discoverUnbudgeted(t, s, "invocie", 20, SearchOptions{})
		require.NoError(t, err)
		require.Len(t, h, 1)
		require.Equal(t, FuzzyReceipt{"limited", []string{"path_limit"}}, r)
		// A path query compares a suffix the bounded walk already resolved, so
		// a deep candidate is either possibly matching or provably excluded.
		for _, tc := range []struct {
			query  string
			status string
		}{
			{"a63/invocie", "limited"}, {"other/invocie", "complete"},
			{"/a0/invocie", "complete"}, {"/a63/invocie", "complete"},
		} {
			h, _, r, err := discoverUnbudgeted(t, s, tc.query, 20, SearchOptions{})
			require.NoError(t, err)
			require.Empty(t, h, tc.query)
			require.Equal(t, tc.status, r.Status, tc.query)
		}
	})
	t.Run("path-bytes", func(t *testing.T) {
		for _, extra := range []int{0, 1} {
			s := newTestStore(t)
			parent := s.RootID()
			for range 7 {
				n, err := s.Mkdir(t.Context(), parent, strings.Repeat("a", 512))
				require.NoError(t, err)
				parent = n.ID
			}
			n, err := s.Mkdir(t.Context(), parent, strings.Repeat("b", 492+extra))
			require.NoError(t, err)
			discoveryFile(t, s, n.ID, "invoice.pdf")
			h, _, r, err := discoverUnbudgeted(t, s, "invocie", 20, SearchOptions{})
			require.NoError(t, err)
			if extra == 0 {
				require.Len(t, h, 1)
				require.Len(t, h[0].Path, 4096)
				require.Equal(t, "complete", r.Status)
			} else {
				require.Empty(t, h)
				require.Equal(t, FuzzyReceipt{"limited", []string{"path_limit"}}, r)
			}
		}
	})
	t.Run("known-suffix", func(t *testing.T) {
		// The oversized ancestor stops the walk after [wrong, invoice.pdf].
		s := newTestStore(t)
		huge, err := s.Mkdir(t.Context(), s.RootID(), strings.Repeat("x", 600))
		require.NoError(t, err)
		wrong, err := s.Mkdir(t.Context(), huge.ID, "wrong")
		require.NoError(t, err)
		discoveryFile(t, s, wrong.ID, "invoice.pdf")
		for _, tc := range []struct {
			query  string
			status string
		}{
			{"invocie", "limited"}, {"wrong/invocie", "limited"}, {"/x/wrong/invocie", "limited"},
			{"acme/invocie", "complete"}, {"foo/acme/invocie", "complete"},
			{"/wrong/invocie", "complete"},
		} {
			h, _, r, err := discoverUnbudgeted(t, s, tc.query, 20, SearchOptions{})
			require.NoError(t, err)
			require.Empty(t, h, tc.query)
			require.Equal(t, tc.status, r.Status, tc.query)
		}
	})
	t.Run("enormous-ancestor", func(t *testing.T) {
		s := newTestStore(t)
		n, err := s.Mkdir(t.Context(), s.RootID(), strings.Repeat("x", 100000))
		require.NoError(t, err)
		discoveryFile(t, s, n.ID, "invoice.pdf")
		h, _, r, err := discoverUnbudgeted(t, s, "invocie", 20, SearchOptions{})
		require.NoError(t, err)
		require.Empty(t, h)
		require.Equal(t, FuzzyReceipt{"limited", []string{"path_limit"}}, r)
	})
}
