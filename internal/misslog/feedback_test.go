package misslog

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func feedbackService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default().MissLog
	cfg.Enabled = true
	return New(st, cfg, nil), st
}

func TestFeedbackCachesHitsAndMergesTriggers(t *testing.T) {
	t.Parallel()
	svc, st := feedbackService(t)
	ticket := svc.Observe(t.Context(), Receipt{Query: "synthetic question", Hits: []Hit{{Rank: 1, NodeID: 2, Path: "/synthetic.txt"}}})
	require.Len(t, ticket.ID, 48)
	require.Len(t, ticket.Capability, 64)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	require.ErrorIs(t, svc.Feedback(t.Context(), ticket.ID, "wrong", Feedback{Verdict: "wrong"}, false), ErrUnavailable)
	require.NoError(t, svc.Feedback(t.Context(), ticket.ID, ticket.Capability, Feedback{Verdict: "low_confidence", Note: "synthetic correction"}, false))
	row, err := st.MissLogRecordByID(t.Context(), ticket.ID)
	require.NoError(t, err)
	require.NotContains(t, row.Receipt, ticket.Capability)
	var got Receipt
	require.NoError(t, json.Unmarshal([]byte(row.Receipt), &got))
	require.Equal(t, []string{"low_confidence"}, got.Triggers)
	require.Equal(t, "low_confidence", got.Feedback.Verdict)
	require.Len(t, got.Hits, 1)
	require.NoError(t, svc.Feedback(t.Context(), ticket.ID, "", Feedback{Verdict: "none_in_vault"}, true))
	row, err = st.MissLogRecordByID(t.Context(), ticket.ID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(row.Receipt), &got))
	require.Equal(t, []string{"low_confidence", "correction"}, got.Triggers)
	require.Equal(t, "none_in_vault", got.Feedback.Verdict)
	empty := svc.Observe(t.Context(), Receipt{Query: "synthetic miss"})
	require.NoError(t, svc.Feedback(t.Context(), empty.ID, empty.Capability, Feedback{Verdict: "missing"}, false))
	row, err = st.MissLogRecordByID(t.Context(), empty.ID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(row.Receipt), &got))
	require.Equal(t, []string{"no_match", "correction"}, got.Triggers)
	restart := New(st, svc.config, nil)
	require.ErrorIs(t, restart.Feedback(t.Context(), empty.ID, empty.Capability, Feedback{Verdict: "wrong"}, false), ErrUnavailable)
	require.NoError(t, restart.Feedback(t.Context(), empty.ID, "", Feedback{Verdict: "wrong"}, true))
}

func TestFeedbackCacheBoundsExpiryAndDeletion(t *testing.T) {
	t.Parallel()
	svc, _ := feedbackService(t)
	now := time.Now()
	svc.now = func() time.Time { return now }
	first := svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: 2}}})
	for range 50 {
		svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: 2}}})
	}
	require.ErrorIs(t, svc.Feedback(t.Context(), first.ID, first.Capability, Feedback{Verdict: "wrong"}, false), ErrUnavailable)
	last := svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: 2}}})
	now = now.Add(15 * time.Minute)
	require.ErrorIs(t, svc.Feedback(t.Context(), last.ID, last.Capability, Feedback{Verdict: "wrong"}, false), ErrUnavailable)
	large := svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: 2}}, Scope: map[string]any{"fence": strings.Repeat("s", 5<<20)}})
	newer := svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: 2}}, Scope: map[string]any{"fence": strings.Repeat("s", 5<<20)}})
	require.NotEmpty(t, large.ID)
	require.NotEmpty(t, newer.ID)
	require.ErrorIs(t, svc.Feedback(t.Context(), large.ID, large.Capability, Feedback{Verdict: "wrong"}, false), ErrUnavailable)
	require.Empty(t, svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: 2}}, Scope: map[string]any{"fence": strings.Repeat("s", 8<<20)}}).ID)
	for _, deletion := range []string{"id", "date", "purge"} {
		t.Run(deletion, func(t *testing.T) {
			ticket := svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: 2}}})
			var err error
			switch deletion {
			case "id":
				err = svc.Delete(t.Context(), ticket.ID, time.Time{})
			case "date":
				err = svc.Delete(t.Context(), "", now.Add(time.Second))
			default:
				err = svc.Delete(t.Context(), "", time.Time{})
			}
			require.NoError(t, err)
			require.ErrorIs(t, svc.Feedback(t.Context(), ticket.ID, ticket.Capability, Feedback{Verdict: "wrong"}, false), ErrUnavailable)
		})
	}
}

func TestFeedbackDeleteCannotResurrect(t *testing.T) {
	t.Parallel()
	svc, st := feedbackService(t)
	for i := range 30 {
		ticket := svc.Observe(t.Context(), Receipt{Query: strconv.Itoa(i), Hits: []Hit{{Rank: 1, NodeID: 2}}})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = svc.Feedback(t.Context(), ticket.ID, ticket.Capability, Feedback{Verdict: "wrong"}, false)
		}()
		go func() { defer wg.Done(); _ = svc.Delete(t.Context(), ticket.ID, time.Time{}) }()
		wg.Wait()
		_, err := st.MissLogRecordByID(t.Context(), ticket.ID)
		require.ErrorIs(t, err, store.ErrNotFound)
		require.ErrorIs(t, svc.Feedback(t.Context(), ticket.ID, ticket.Capability, Feedback{Verdict: "wrong"}, false), ErrUnavailable)
	}
}

func TestFeedbackValidationAndPersistenceFailure(t *testing.T) {
	t.Parallel()
	svc, st := feedbackService(t)
	ticket := svc.Observe(t.Context(), Receipt{Query: "synthetic", Hits: []Hit{{Rank: 1, NodeID: 2}}})
	for _, input := range []Feedback{{Verdict: "unknown"}, {Verdict: "none_in_vault", Expected: []Identity{{NodeID: 2}}}, {Verdict: "wrong", Note: strings.Repeat("x", 1001)}, {Verdict: "wrong", Expected: []Identity{{NodeID: 999999}}}, {Verdict: "wrong", Expected: make([]Identity, 21)}} {
		require.ErrorIs(t, svc.Feedback(t.Context(), ticket.ID, "", input, true), ErrInvalid)
	}
	require.NoError(t, st.Close())
	require.ErrorIs(t, svc.Feedback(t.Context(), ticket.ID, ticket.Capability, Feedback{Verdict: "wrong"}, false), ErrPersistence)
}

func FuzzFeedbackValidation(f *testing.F) {
	f.Add("wrong", "", int64(0))
	f.Add("none_in_vault", "", int64(0))
	f.Add("low_confidence", strings.Repeat("界", 1000), int64(2))
	f.Add("none_in_vault", "synthetic", int64(1))
	f.Add("missing", "é", int64(-1))
	f.Fuzz(func(t *testing.T, verdict, note string, node int64) {
		feedback := Feedback{Verdict: verdict, Note: note}
		if node != 0 {
			feedback.Expected = []Identity{{NodeID: node}}
		}

		allowed := false
		switch verdict {
		case "wrong", "missing", "low_confidence", "none_in_vault":
			allowed = true
		}
		wantValid := allowed && utf8.ValidString(note) && len([]rune(note)) <= 1000 && node >= 0 && (verdict != "none_in_vault" || node == 0)
		err := ValidateFeedback(feedback)
		require.Equal(t, wantValid, err == nil)
		if err == nil {
			encoded, err := json.Marshal(feedback)
			require.NoError(t, err)
			var decoded Feedback
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			require.Equal(t, feedback, decoded)
		}
	})
}

func TestFeedbackDoesNotClaimSuccessAfterRetentionPrunesIt(t *testing.T) {
	t.Parallel()
	svc, st := feedbackService(t)
	svc.config.MaxRecords = 1
	now := time.Now()
	svc.now = func() time.Time { return now }
	pending := svc.Observe(t.Context(), Receipt{Query: "older hit", Hits: []Hit{{Rank: 1, NodeID: 2}}})
	now = now.Add(time.Second)
	newer := svc.Observe(t.Context(), Receipt{Query: "newer miss"})
	require.NotEmpty(t, newer.ID)
	require.Error(t, svc.Feedback(t.Context(), pending.ID, pending.Capability, Feedback{Verdict: "wrong"}, false))
	_, err := st.MissLogRecordByID(t.Context(), pending.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestReceiptPreservesBoundedEvidenceAndRejectsMissingHitIdentity(t *testing.T) {
	t.Parallel()
	svc, _ := feedbackService(t)
	require.Empty(t, svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1}}}).ID)
	evidence := make([]string, 64)
	for i := range evidence {
		evidence[i] = fmt.Sprint("synthetic evidence ", i)
	}
	ticket := svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: 2, EvidenceIDs: evidence}}})
	require.NotEmpty(t, ticket.ID)
	require.NoError(t, svc.Feedback(t.Context(), ticket.ID, ticket.Capability, Feedback{Verdict: "wrong"}, false))
	row, err := svc.store.MissLogRecordByID(t.Context(), ticket.ID)
	require.NoError(t, err)
	var receipt Receipt
	require.NoError(t, json.Unmarshal([]byte(row.Receipt), &receipt))
	require.Equal(t, evidence, receipt.Hits[0].EvidenceIDs)
}

func TestAutomaticReceiptAdmissionIsSeparateFromFeedback(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	cfg := config.Default().MissLog
	cfg.Enabled = true
	ordinary, automatic := 0, 0
	svc := New(st, cfg, nil, func(_ context.Context, fn func() error) error { ordinary++; return fn() }, func(context.Context, func() error) error { automatic++; return context.Canceled })
	require.Empty(t, svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: 2}}}).ID)
	require.Equal(t, 1, automatic)
	require.Equal(t, 1, ordinary) // startup pruning uses ordinary admission
	receipt := Receipt{ID: strings.Repeat("a", 48), CreatedAt: time.Now().UTC(), VaultUID: st.VaultID(), Query: "synthetic existing miss", Triggers: []string{"no_match"}}
	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.NoError(t, st.AddMissLogRecord(t.Context(), store.MissLogRecord{ID: receipt.ID, CreatedAt: receipt.CreatedAt, Receipt: string(encoded)}, time.Now(), 90, 5000))
	require.NoError(t, svc.Feedback(t.Context(), receipt.ID, "", Feedback{Verdict: "wrong"}, true))
	require.Equal(t, 2, ordinary)
}

func TestFeedbackExpectedIdentitiesMustBelongToTheirDocuments(t *testing.T) {
	t.Parallel()
	svc, st := feedbackService(t)
	first, err := st.CreateFile(t.Context(), st.RootID(), "first.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	second, err := st.CreateFile(t.Context(), st.RootID(), "second.txt", strings.Repeat("b", 64), 1, "text/plain")
	require.NoError(t, err)
	ticket := svc.Observe(t.Context(), Receipt{Hits: []Hit{{Rank: 1, NodeID: first.ID, ContentVersionID: first.CurrentVersionID}}})
	for _, expected := range [][]Identity{
		{{NodeID: second.ID, ContentVersionID: first.CurrentVersionID}},
		{{NodeID: first.ID}, {NodeID: first.ID, ContentVersionID: first.CurrentVersionID}},
		{{NodeID: first.ID, ContentVersionID: "11111111-1111-4111-8111-111111111111"}},
	} {
		require.ErrorIs(t, svc.Feedback(t.Context(), ticket.ID, ticket.Capability, Feedback{Verdict: "wrong", Expected: expected}, false), ErrInvalid)
	}
	require.NoError(t, svc.Feedback(t.Context(), ticket.ID, ticket.Capability, Feedback{Verdict: "wrong", Expected: []Identity{{NodeID: second.ID}}}, false))
	row, err := st.MissLogRecordByID(t.Context(), ticket.ID)
	require.NoError(t, err)
	var receipt Receipt
	require.NoError(t, json.Unmarshal([]byte(row.Receipt), &receipt))
	require.Equal(t, []Identity{{NodeID: second.ID}}, receipt.Feedback.Expected)
}

func TestAutomaticReceiptDoesNotWaitForFeedbackSerialization(t *testing.T) {
	t.Parallel()
	svc, _ := feedbackService(t)
	svc.mu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			svc.mu.Unlock()
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan Ticket, 1)
	go func() {
		result <- svc.Observe(ctx, Receipt{Query: "synthetic nonblocking read", Hits: []Hit{{Rank: 1, NodeID: 2}}})
	}()
	select {
	case ticket := <-result:
		require.Empty(t, ticket.ID)
	case <-ctx.Done():
		svc.mu.Unlock()
		locked = false
		<-result
		t.Fatal("automatic search receipt waited for feedback serialization")
	}
}

func TestEnabledServiceStateIsNilSafe(t *testing.T) {
	t.Parallel()
	var absent *Service
	require.False(t, absent.Enabled())
	svc, st := feedbackService(t)
	require.True(t, svc.Enabled())
	require.False(t, New(st, config.Default().MissLog, nil).Enabled())
}
