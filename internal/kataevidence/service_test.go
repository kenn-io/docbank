package kataevidence

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/kataclient"
)

// testKata runs real Kata storage and guards. The HTTP faults model an
// interrupted comment request and a competing writer, which katatest doesn't schedule.
type testKata struct {
	*kataclient.Client

	t               *testing.T
	api             *client.Client
	projectID       int64
	ref             string
	commentFails    bool
	conflictOnce    bool
	concurrentEntry jsontext.Value
}

func newTestKata(t *testing.T, metadata map[string]jsontext.Value, body string) *testKata {
	t.Helper()
	backend := katatest.New(t, katatest.WithToken("synthetic-key"))
	api := backend.Client(t)
	project, err := api.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	k := &testKata{t: t, api: api, projectID: project.Project.ID}
	direct := kataclient.New(config.KataConnection{Endpoint: backend.Endpoint, APIKey: "synthetic-key", Project: "docbank"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") && k.commentFails {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/metadata") && k.conflictOnce {
			k.conflictOnce = false
			issue, err := direct.Get(r.Context(), k.ref)
			if !assert.NoError(t, err) {
				http.Error(w, "synthetic competing write failed", http.StatusInternalServerError)
				return
			}
			env, err := parseEnvelope(issue.Metadata[MetadataKey])
			if !assert.NoError(t, err) {
				http.Error(w, "synthetic competing write failed", http.StatusInternalServerError)
				return
			}
			env.entries = append(env.entries, k.concurrentEntry)
			data, err := env.encode()
			if !assert.NoError(t, err) {
				http.Error(w, "synthetic competing write failed", http.StatusInternalServerError)
				return
			}
			_, err = direct.Patch(r.Context(), k.ref, MetadataKey, issue.Metadata[MetadataKey], data)
			if !assert.NoError(t, err) {
				http.Error(w, "synthetic competing write failed", http.StatusInternalServerError)
				return
			}
			_, err = direct.Patch(r.Context(), k.ref, "unrelated", nil, jsontext.Value(`"preserved"`))
			if !assert.NoError(t, err) {
				http.Error(w, "synthetic competing write failed", http.StatusInternalServerError)
				return
			}
		}
		backend.Service.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	k.Client = kataclient.New(config.KataConnection{Endpoint: server.URL, APIKey: "synthetic-key", Project: "docbank"})
	if metadata != nil {
		issue, err := direct.Create(t.Context(), "seed", "Synthetic issue", body, metadata)
		require.NoError(t, err)
		k.ref = issue.QualifiedRef
	}
	return k
}

func (k *testKata) snapshot() kataclient.Issue {
	issue, err := k.Get(k.t.Context(), k.ref)
	require.NoError(k.t, err)
	return issue
}

func (k *testKata) commentCount() int {
	issue, err := k.api.ShowIssue(k.t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: k.projectID, Ref: k.ref}})
	require.NoError(k.t, err)
	return len(issue.Comments)
}

func (k *testKata) createdCount() int {
	issues, err := k.api.ListAllIssues(k.t.Context(), &generated.ListAllIssuesRequestOptions{})
	require.NoError(k.t, err)
	return len(issues.Issues)
}

type memoryArchive struct {
	state      string
	reads      int
	prepareErr error
	evidence   *Evidence
}

func (*memoryArchive) VaultUID() string { return "11111111-1111-4111-8111-111111111111" }

func (a *memoryArchive) Prepare(context.Context, Selector) (Evidence, error) {
	if a.prepareErr != nil {
		return Evidence{}, a.prepareErr
	}
	if a.evidence != nil {
		return *a.evidence, nil
	}
	return NewEvidence(nativeReference(), "hello", "notes.txt"), nil
}

func (a *memoryArchive) Resolve(_ context.Context, ref Reference, _ string) (Resolution, error) {
	a.reads++
	return Resolution{State: a.state, Evidence: NewEvidence(ref, "hello", "notes.txt")}, nil
}

func TestCiteCreateAndReplay(t *testing.T) {
	k := newTestKata(t, nil, "")
	a := &memoryArchive{state: "available"}
	s := Service{Kata: k, Archive: a}
	r := CiteRequest{Selector: Selector{NodeID: 2, EndRune: 5}, Title: "Follow up", Brief: "Review this.", IdempotencyKey: "create-1"}
	issue, err := s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, "Review this.\n\nEvidence: notes.txt\n\n> hello\n", issue.Body)
	var envelope struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(issue.Metadata[MetadataKey], &envelope))
	require.Equal(t, 1, envelope.Version)
	require.Len(t, envelope.Entries, 1)
	require.Equal(t, "docbank", envelope.Entries[0].Reference.Kind)
	require.Nil(t, envelope.Entries[0].Pending)
	require.Equal(t, "hello", envelope.Entries[0].Quote)
	require.Equal(t, "notes.txt", envelope.Entries[0].Label)
	a.prepareErr = ErrUnavailable
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.createdCount())
	r.Title = "Different request"
	_, err = s.Cite(t.Context(), r)
	require.ErrorContains(t, err, "idempotency")
	t.Run("distinct_key", func(t *testing.T) {
		a.prepareErr = nil
		r.Title, r.IdempotencyKey = "Follow up", "create-2"
		second, err := s.Cite(t.Context(), r)
		require.NoError(t, err)
		require.NotEqual(t, issue.UID, second.UID)
		require.Equal(t, issue.Body, second.Body)
		require.Equal(t, 2, k.createdCount())
	})
}

func TestAppendPendingRetryPreservesOtherEvidence(t *testing.T) {
	foreign := jsontext.Value(`{"version":1,"entries":[{"id":"old","passage":"old","reference":{"kind":"future","version":9},"future":"keep"}],"future":"keep"}`)
	k := newTestKata(t, map[string]jsontext.Value{MetadataKey: foreign, "msgvault.evidence": jsontext.Value(`{"foreign":true}`)}, "")
	k.commentFails, k.conflictOnce = true, true
	concurrent := nativeReference()
	concurrent.Docbank.NodeID = 3
	var err error
	k.concurrentEntry, err = json.Marshal(Entry{ID: ID(concurrent), Reference: concurrent, Quote: "other", Label: "other.txt"})
	require.NoError(t, err)
	s := Service{Kata: k, Archive: &memoryArchive{state: "available"}}
	r := CiteRequest{Issue: k.ref, Selector: Selector{NodeID: 2, EndRune: 5}}
	_, err = s.Cite(t.Context(), r)
	require.ErrorContains(t, err, "status 503")
	require.Contains(t, string(k.snapshot().Metadata[MetadataKey]), `"pending"`)
	k.commentFails = false
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.commentCount())
	require.NotContains(t, string(k.snapshot().Metadata[MetadataKey]), `"pending"`)
	require.Contains(t, string(k.snapshot().Metadata[MetadataKey]), `"future":"keep"`)
	require.JSONEq(t, `{"foreign":true}`, string(k.snapshot().Metadata["msgvault.evidence"]))
	require.Equal(t, `"preserved"`, string(k.snapshot().Metadata["unrelated"]))
	var envelope struct {
		Entries []Entry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(k.snapshot().Metadata[MetadataKey], &envelope))
	require.Len(t, envelope.Entries, 3)
	require.EqualValues(t, 3, envelope.Entries[1].Reference.Docbank.NodeID, "preserve the concurrent citation")
	require.EqualValues(t, 2, envelope.Entries[2].Reference.Docbank.NodeID, "append the requested citation")
	require.Equal(t, "hello", envelope.Entries[2].Quote)
	require.Equal(t, "notes.txt", envelope.Entries[2].Label)
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.commentCount())
}

func TestCitationOnlyAppendRetryFinishesPendingAfterSourceDisappears(t *testing.T) {
	ref := renditionReference()
	citation := ref.Rendition
	evidence := NewEvidence(ref, "hello", "notes.txt")
	k := newTestKata(t, map[string]jsontext.Value{}, "")
	k.commentFails = true
	a := &memoryArchive{evidence: &evidence}
	s := Service{Kata: k, Archive: a, Now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }}
	r := CiteRequest{Issue: k.ref, Selector: Selector{NodeID: 2, EndRune: 5, Citation: citation}}
	_, err := s.Cite(t.Context(), r)
	require.ErrorContains(t, err, "status 503")
	saved := k.snapshot()
	var pending struct {
		Entries []Entry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(saved.Metadata[MetadataKey], &pending))
	require.Len(t, pending.Entries, 1)
	require.NotNil(t, pending.Entries[0].Pending)
	require.Equal(t, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), pending.Entries[0].Pending.SavedAt)
	k.commentFails = false
	a.prepareErr = ErrUnavailable
	for _, field := range []string{"vault", "version"} {
		altered := *citation
		if field == "vault" {
			altered.VaultUID[0]++
		} else {
			altered.ContentVersionID[0]++
		}
		conflicting := r
		conflicting.Selector.ExpectedVersion = citation.ContentVersionID.String()
		conflicting.Selector.Citation = &altered
		_, err = s.Cite(t.Context(), conflicting)
		require.ErrorIs(t, err, ErrUnavailable, "different citation %s must not finish the pending comment", field)
		require.Zero(t, k.commentCount())
	}
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.commentCount())
	require.NotContains(t, string(k.snapshot().Metadata[MetadataKey]), `"pending"`)
}

func TestContextAvailableAndUnreachable(t *testing.T) {
	k := newTestKata(t, nil, "")
	a := &memoryArchive{state: "available"}
	s := Service{Kata: k, Archive: a}
	created, err := s.Cite(t.Context(), CiteRequest{Title: "Follow up", IdempotencyKey: "create-1", Selector: Selector{NodeID: 2, EndRune: 5}})
	require.NoError(t, err)
	k.ref = created.QualifiedRef
	page, err := s.Context(t.Context(), ContextRequest{Issue: k.ref})
	require.NoError(t, err)
	require.Len(t, page.Passages, 1)
	require.Equal(t, "available", page.Passages[0].State)
	require.Equal(t, "hello", page.Passages[0].SavedQuote)
	s.Archive = nil
	page, err = s.Context(t.Context(), ContextRequest{Issue: k.ref})
	require.NoError(t, err)
	require.Equal(t, "unreachable", page.Passages[0].State)
	require.Equal(t, "hello", page.Passages[0].SavedQuote)
}

func TestContextPreservesPendingComment(t *testing.T) {
	ref := nativeReference()
	entry := Entry{ID: ID(ref), Reference: ref, Quote: "hello", Label: "notes.txt", Pending: &Pending{SavedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	metadata, err := json.Marshal(map[string]any{"version": 1, "entries": []Entry{entry}})
	require.NoError(t, err)
	k := newTestKata(t, map[string]jsontext.Value{MetadataKey: metadata}, "")
	s := Service{Kata: k, Archive: &memoryArchive{state: "available"}}
	page, err := s.Context(t.Context(), ContextRequest{Issue: k.ref})
	require.NoError(t, err)
	encoded, err := json.Marshal(page.Passages)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"pending":{"saved_at":"2026-01-01T00:00:00Z"}`)
	require.Equal(t, "available", page.Passages[0].State)
	require.Zero(t, k.commentCount())
}

func TestAppendRefusesEnvelopeThatWouldExceedReadLimit(t *testing.T) {
	previous, err := json.Marshal(map[string]any{"version": 1, "entries": []any{}, "future": strings.Repeat("x", (512<<10)-200)})
	require.NoError(t, err)
	k := newTestKata(t, map[string]jsontext.Value{MetadataKey: previous}, "")
	s := Service{Kata: k, Archive: &memoryArchive{state: "available"}}
	_, err = s.Cite(t.Context(), CiteRequest{Issue: k.ref, Selector: Selector{NodeID: 2, EndRune: 5}})
	require.Error(t, err)
	require.JSONEq(t, string(previous), string(k.snapshot().Metadata[MetadataKey]), "refuse oversized metadata before changing Kata")
	require.Zero(t, k.commentCount())
}

func TestAppendRejectsFullEnvelopeAtQuoteLimit(t *testing.T) {
	entries := make([]jsontext.Value, MaxEntries)
	for i := range entries {
		entries[i] = jsontext.Value(`{"reference":{"kind":"future"}}`)
	}
	previous, err := json.Marshal(map[string]any{"version": 1, "entries": entries})
	require.NoError(t, err)
	k := newTestKata(t, map[string]jsontext.Value{MetadataKey: previous}, "")
	s := Service{Kata: k, Archive: &memoryArchive{state: "available"}}

	_, err = s.Cite(t.Context(), CiteRequest{Issue: k.ref, Selector: Selector{NodeID: 2, EndRune: 5}})

	require.ErrorIs(t, err, ErrEntryLimit)
	require.ErrorContains(t, err, "already holds the maximum of 64 Docbank quotes")
	require.NotContains(t, strings.ToLower(err.Error()), "retry")
	require.JSONEq(t, string(previous), string(k.snapshot().Metadata[MetadataKey]))
	require.Zero(t, k.commentCount())
}

func TestContextUnsupportedNativeDoesNotHideOtherPassages(t *testing.T) {
	future := nativeReference()
	future.Version = 2
	unsupported, err := json.Marshal(Entry{ID: ID(future), Reference: future, Quote: "hello", Label: "notes.txt"})
	require.NoError(t, err)
	valid, err := json.Marshal(Entry{ID: ID(nativeReference()), Reference: nativeReference(), Quote: "hello", Label: "notes.txt"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name       string
		entry      jsontext.Value
		savedQuote string
	}{
		{"future_version", unsupported, "hello"},
		{"malformed_docbank", jsontext.Value(`{"reference":{"kind":"docbank","docbank":[]}}`), ""},
		{"malformed_reference", jsontext.Value(`{"reference":[]}`), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"version": 1, "entries": []jsontext.Value{tc.entry, valid}})
			require.NoError(t, err)
			k := newTestKata(t, map[string]jsontext.Value{MetadataKey: data}, "Evidence: notes.txt\n\n> hello\n")
			a := &memoryArchive{state: "available"}
			page, err := (Service{Kata: k, Archive: a}).Context(t.Context(), ContextRequest{Issue: k.ref})
			require.NoError(t, err)
			require.Len(t, page.Passages, 2)
			require.Equal(t, "unsupported", page.Passages[0].State)
			require.Equal(t, tc.savedQuote, page.Passages[0].SavedQuote, "malformed references cannot claim another entry's quote")
			require.Equal(t, "available", page.Passages[1].State)
			require.Equal(t, "hello", page.Passages[1].SavedQuote)
			require.Equal(t, 1, a.reads)
		})
	}
}

func TestCiteValidationNamesFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CiteRequest)
		detail string
	}{
		{"start", func(r *CiteRequest) { r.Selector.StartRune = -1 }, "start_rune must be nonnegative"},
		{"end", func(r *CiteRequest) { r.Selector.EndRune = 1001 }, "end_rune must follow start_rune by at most 1000 code points"},
		{"backward", func(r *CiteRequest) { r.Selector.StartRune, r.Selector.EndRune = 5, 3 }, "end_rune must follow start_rune by at most 1000 code points"},
		{"quote", func(r *CiteRequest) { r.Selector.Quote = strings.Repeat("x", 1001) }, "quote must contain at most 1000 UTF-8 code points"},
		{"expected_version", func(r *CiteRequest) { r.Selector.ExpectedVersion = "invalid" }, "expected_version requires a content version UUID"},
		{"brief", func(r *CiteRequest) { r.Brief = strings.Repeat("x", 2001) }, "brief exceeds 2000 code points"},
		{"title", func(r *CiteRequest) { r.Title = strings.Repeat("x", 513) }, "title exceeds 512 code points"},
		{"issue", func(r *CiteRequest) { r.Issue = strings.Repeat("x", 257) }, "issue exceeds 256 bytes"},
		{"citation", func(r *CiteRequest) { r.Selector.Citation = &document.TextCitation{NodeID: 3} }, "citation does not match the selected node or range"},
		{"append", func(r *CiteRequest) { r.Issue = "abcd" }, "issue cannot be combined with title, brief, or idempotency_key"},
		{"missing_title", func(r *CiteRequest) { r.Title = "" }, "title is required"},
		{"missing_key", func(r *CiteRequest) { r.IdempotencyKey = "" }, "idempotency_key requires 1 to 128 bytes without line breaks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := CiteRequest{Selector: Selector{NodeID: 2, EndRune: 5}, Title: "Review", IdempotencyKey: "key"}
			tc.change(&request)
			_, err := (Service{}).Cite(t.Context(), request)
			require.ErrorIs(t, err, ErrInvalid)
			require.EqualError(t, err, "invalid Kata evidence request: "+tc.detail)
		})
	}
}

func TestContextUnknownKindsKeepPagination(t *testing.T) {
	entries := make([]jsontext.Value, ContextPageSize+1)
	for i := range entries {
		entries[i] = jsontext.Value(`{"id":"future","reference":{"kind":"future","version":2}}`)
	}
	data, err := json.Marshal(map[string]any{"version": 1, "entries": entries})
	require.NoError(t, err)
	k := newTestKata(t, map[string]jsontext.Value{MetadataKey: data, "msgvault.evidence": jsontext.Value(`{"version":99}`)}, "")
	s := Service{Kata: k}
	page, err := s.Context(t.Context(), ContextRequest{Issue: k.ref})
	require.NoError(t, err)
	require.Len(t, page.Passages, 10)
	require.Equal(t, 10, page.NextOffset)
	for _, entry := range page.Passages {
		require.Equal(t, "unsupported", entry.State)
	}
	page, err = s.Context(t.Context(), ContextRequest{Issue: k.ref, Offset: page.NextOffset})
	require.NoError(t, err)
	require.Len(t, page.Passages, 1)
	require.Zero(t, page.NextOffset)
}

func TestContextRejectsOtherProject(t *testing.T) {
	_, err := (Service{Kata: newTestKata(t, nil, "")}).Context(t.Context(), ContextRequest{Issue: "other#abcd"})
	require.ErrorContains(t, err, "wrong_project")
}

func TestCiteContainsEveryQuoteLineEnding(t *testing.T) {
	for _, words := range []string{"first\r# second", "first\r\n# second", "first\n# second"} {
		for _, appendQuote := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/append=%t", words, appendQuote), func(t *testing.T) {
				k := newTestKata(t, map[string]jsontext.Value{}, "")
				evidence := NewEvidence(nativeReference(), words, "notes.txt")
				service := Service{Kata: k, Archive: &memoryArchive{evidence: &evidence}}
				request := CiteRequest{Title: "Review", IdempotencyKey: "line-endings", Selector: Selector{NodeID: 2, EndRune: utf8.RuneCountInString(words)}}
				if appendQuote {
					request.Title, request.IdempotencyKey, request.Issue = "", "", k.ref
				}
				issue, err := service.Cite(t.Context(), request)
				require.NoError(t, err)
				if appendQuote {
					comments, err := k.GetComments(t.Context(), k.ref)
					require.NoError(t, err)
					require.Len(t, comments, 1)
					require.Contains(t, comments[0], "\n\n> first\n> # second\n")
				} else {
					require.Equal(t, "Evidence: notes.txt\n\n> first\n> # second\n", issue.Body)
				}
				var saved struct {
					Entries []Entry `json:"entries"`
				}
				require.NoError(t, json.Unmarshal(issue.Metadata[MetadataKey], &saved))
				require.Equal(t, words, saved.Entries[0].Quote)
			})
		}
	}
}
