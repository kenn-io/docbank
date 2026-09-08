package qmdbridge

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/store"
)

func TestSearchFreezesRequestBeforeEveryCallback(t *testing.T) {
	root, source, receipt := exportFixture(t)
	rerank := true
	request := Request{Searches: []Search{{Type: SearchLexical, Query: "original query"}}, Intent: "original intent", Limit: 3, MinScore: .2, Rerank: &rerank}
	mutate := func() {
		request.Searches[0].Query = "mutated query"
		request.Intent = "mutated intent"
		*request.Rerank = false
	}
	authority := &authorityStub{normalizeHook: mutate, live: []store.QMDExportLiveCandidate{{NodeID: source.NodeID, NodeRevision: 4, ContentVersionID: source.ContentVersionID, Path: "/source.pdf"}}}
	authorizer := &authorizerStub{hook: mutate}
	var payload wireRequest
	server := qmdServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &payload, json.RejectUnknownMembers(true))) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resultBody(receipt.Manifest.Entries[0].URI)))
	})
	client := newTestClient(t, server, root, authorizer, authority, 1<<20)
	client.secrets = secretStub{hook: mutate}
	_, err := client.Search(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, payload.Searches, 1)
	assert.Equal(t, "original query", payload.Searches[0].Query)
	assert.Equal(t, "original intent", payload.Intent)
	require.NotNil(t, payload.Rerank)
	assert.True(t, *payload.Rerank)
	operation := authorizer.gotOperation()
	assert.Equal(t, len("original query")+len("original intent"), operation.DisclosedBytes)
	assert.Equal(t, 1, operation.QueryCount)
}

func TestSearchRejectsOverCountBeforeCopyingCallerSlice(t *testing.T) {
	client := &Client{profile: Profile{RequestTimeout: time.Second}}
	searches := make([]Search, 4096)
	const attempts = 32
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range attempts {
		_, err := client.Search(context.Background(), Request{Searches: searches})
		if err == nil {
			t.Fatal("oversized search set was accepted")
		}
	}
	runtime.ReadMemStats(&after)
	allocatedPerRequest := (after.TotalAlloc - before.TotalAlloc) / attempts
	require.Less(t, allocatedPerRequest, uint64(8<<10),
		"an invalid count must not allocate a copy proportional to caller input")
}

func TestSearchRejectsInvalidRequestBeforeCallbacks(t *testing.T) {
	root, _, _ := exportFixture(t)
	var callbacks int
	hook := func() { callbacks++ }
	authority := &authorityStub{normalizeHook: hook}
	client := newTestClient(t, staticQMDServer(t, `{"results":[]}`, "application/json", http.StatusOK), root, &authorizerStub{hook: hook}, authority, 1<<20)
	client.secrets = secretStub{hook: hook}
	_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchLexical, Query: " "}}, Limit: 1})
	require.Error(t, err)
	assert.Zero(t, callbacks)
}

func TestSearchEnforcesSharedQueryBudgetAndFiniteBoundsBeforeCallbacks(t *testing.T) {
	root, _, _ := exportFixture(t)
	var callbacks int
	authority := &authorityStub{normalizeHook: func() { callbacks++ }}
	client := newTestClient(t, staticQMDServer(t, `{"results":[]}`, "application/json", http.StatusOK), root, &authorizerStub{}, authority, 1<<20)
	client.profile.MaxQueryBytes = 5
	client.profile.MaxIntentBytes = 5
	for name, request := range map[string]Request{
		"aggregate bytes":   {Searches: []Search{{Type: SearchLexical, Query: "abc"}, {Type: SearchHyDE, Query: "def"}}, Limit: 1},
		"intent plus query": {Searches: []Search{{Type: SearchVector, Query: "abc"}}, Intent: "def", Limit: 1},
		"limit":             {Searches: []Search{{Type: SearchVector, Query: "a"}}, Limit: client.profile.MaxCandidates + 1},
		"score":             {Searches: []Search{{Type: SearchVector, Query: "a"}}, Limit: 1, MinScore: 2},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Search(t.Context(), request)
			require.Error(t, err)
		})
	}
	assert.Zero(t, callbacks)
}

func TestSearchRejectsInvalidSecretAndAuthorizationWithoutEgress(t *testing.T) {
	root, _, _ := exportFixture(t)
	for name, configure := range map[string]func(*Client){
		"empty secret":  func(c *Client) { c.secrets = secretStub{value: " "} },
		"secret error":  func(c *Client) { c.secrets = secretStub{err: errors.New("secret unavailable")} },
		"authorization": func(c *Client) { c.authorizer = &authorizerStub{err: errors.New("denied")} },
	} {
		t.Run(name, func(t *testing.T) {
			var requests int
			server := qmdServer(t, func(http.ResponseWriter, *http.Request) { requests++ })
			client := newTestClient(t, server, root, &authorizerStub{}, &authorityStub{}, 1<<20)
			configure(client)
			_, err := client.Search(t.Context(), validRequest())
			require.Error(t, err)
			assert.Zero(t, requests)
		})
	}
}

func TestSearchRetainsManifestAuthorityAcrossCandidateMutation(t *testing.T) {
	root, source, receipt := exportFixture(t)
	authority := &authorityStub{live: []store.QMDExportLiveCandidate{{NodeID: source.NodeID, NodeRevision: 5, ContentVersionID: source.ContentVersionID, Path: "/current.pdf"}}}
	authority.revalidateHook = func(values []store.QMDExportSource) { values[0].NodeID = 999; values[0].ContentVersionID = "mutated" }
	client := newTestClient(t, staticQMDServer(t, resultBody(receipt.Manifest.Entries[0].URI), "application/json", http.StatusOK), root, &authorizerStub{}, authority, 1<<20)
	results, err := client.Search(t.Context(), validRequest())
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, source.NodeID, results[0].Document.NodeID)
	assert.Equal(t, source.ContentVersionID, results[0].Document.ContentVersionID)
}

func TestSearchChecksCancellationAfterSuccessfulCallbacks(t *testing.T) {
	root, _, _ := exportFixture(t)
	for name, configure := range map[string]func(*Client, context.CancelFunc){
		"normalization": func(c *Client, cancel context.CancelFunc) { c.authority = &authorityStub{normalizeHook: cancel} },
		"secret":        func(c *Client, cancel context.CancelFunc) { c.secrets = secretStub{hook: cancel} },
		"authorization": func(c *Client, cancel context.CancelFunc) { c.authorizer = &authorizerStub{hook: cancel} },
		"live authority": func(c *Client, cancel context.CancelFunc) {
			c.authority = &authorityStub{revalidateHook: func([]store.QMDExportSource) { cancel() }}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			client := newTestClient(t, staticQMDServer(t, `{"results":[]}`, "application/json", http.StatusOK), root, &authorizerStub{}, &authorityStub{}, 1<<20)
			configure(client, cancel)
			_, err := client.Search(ctx, validRequest())
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestValidateLiveRequiresExactIdentityRevisionAndLogicalPath(t *testing.T) {
	entries := []store.QMDExportSource{{NodeID: 7, ContentVersionID: "version"}}
	valid := store.QMDExportLiveCandidate{NodeID: 7, ContentVersionID: "version", NodeRevision: 1, Path: "/folder/file.pdf"}
	require.NoError(t, validateLive(entries, []store.QMDExportLiveCandidate{valid}))
	for name, mutate := range map[string]func(*store.QMDExportLiveCandidate){
		"node":          func(value *store.QMDExportLiveCandidate) { value.NodeID++ },
		"version":       func(value *store.QMDExportLiveCandidate) { value.ContentVersionID = "other" },
		"revision zero": func(value *store.QMDExportLiveCandidate) { value.NodeRevision = 0 },
		"relative path": func(value *store.QMDExportLiveCandidate) { value.Path = "folder/file.pdf" },
		"host path":     func(value *store.QMDExportLiveCandidate) { value.Path = `C:\folder\file.pdf` },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			require.ErrorIs(t, validateLive(entries, []store.QMDExportLiveCandidate{candidate}), store.ErrQMDExportAuthorityStale)
		})
	}
	require.ErrorIs(t, validateLive(entries, nil), store.ErrQMDExportAuthorityStale)
	secondEntries := []store.QMDExportSource{entries[0], {NodeID: 8, ContentVersionID: "second"}}
	second := store.QMDExportLiveCandidate{NodeID: 8, ContentVersionID: "second", NodeRevision: 1, Path: "/second.pdf"}
	require.ErrorIs(t, validateLive(secondEntries, []store.QMDExportLiveCandidate{second, valid}), store.ErrQMDExportAuthorityStale)
}

func TestSearchDeadlineStartsBeforeScopeNormalization(t *testing.T) {
	root, _, _ := exportFixture(t)
	client := newTestClient(t, staticQMDServer(t, `{"results":[]}`, "application/json", http.StatusOK), root, &authorizerStub{}, blockingNormalizeAuthority{}, 1<<20)
	client.profile.RequestTimeout = 10 * time.Millisecond
	_, err := client.Search(t.Context(), validRequest())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestSearchChecksCancellationAfterSuccessfulResponseRead(t *testing.T) {
	root, _, _ := exportFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	client := newTestClient(t, staticQMDServer(t, `{"results":[]}`, "application/json", http.StatusOK), root, &authorizerStub{}, &authorityStub{}, 1<<20)
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: &cancelAtEOFReader{Reader: strings.NewReader(`{"results":[]}`), cancel: cancel}}, nil
	})
	_, err := client.Search(ctx, validRequest())
	require.ErrorIs(t, err, context.Canceled)
}

func TestSearchPreservesAuthorityErrorAndExpiredContext(t *testing.T) {
	root, _, _ := exportFixture(t)
	cause := errors.New("synthetic authority failure")
	ctx, cancel := context.WithCancel(t.Context())
	authority := &authorityStub{err: cause, revalidateHook: func([]store.QMDExportSource) { cancel() }}
	client := newTestClient(t, staticQMDServer(t, `{"results":[]}`, "application/json", http.StatusOK), root, &authorizerStub{}, authority, 1<<20)
	_, err := client.Search(ctx, validRequest())
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSearchConcurrentRequestsKeepOwnedQueriesIsolated(t *testing.T) {
	root, _, _ := exportFixture(t)
	const count = 20
	received := make(chan string, count)
	server := qmdServer(t, func(w http.ResponseWriter, r *http.Request) {
		var value wireRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &value)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !assert.Len(t, value.Searches, 1) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- value.Searches[0].Query
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	client := newTestClient(t, server, root, authorizerFunc(func(context.Context, Operation) error { return nil }), authorityFunc{}, 1<<20)
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for index := range count {
		wg.Go(func() {
			query := fmt.Sprintf("query-%d", index)
			_, err := client.Search(t.Context(), Request{Searches: []Search{{Type: SearchLexical, Query: query}}, Limit: 1})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	close(received)
	actual := make(map[string]int, count)
	for query := range received {
		actual[query]++
	}
	expected := make(map[string]int, count)
	for index := range count {
		expected[fmt.Sprintf("query-%d", index)] = 1
	}
	assert.Equal(t, expected, actual)
}

type authorizerFunc func(context.Context, Operation) error

func (f authorizerFunc) AuthorizeQMDQuery(ctx context.Context, op Operation) error { return f(ctx, op) }

type authorityFunc struct{}

func (authorityFunc) NormalizeQMDSearchScope(_ context.Context, value store.SearchOptions) (store.SearchOptions, error) {
	return value, nil
}
func (authorityFunc) RevalidateQMDExportCandidates(context.Context, []store.QMDExportSource, store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
	return []store.QMDExportLiveCandidate{}, nil
}

type blockingNormalizeAuthority struct{}

func (blockingNormalizeAuthority) NormalizeQMDSearchScope(ctx context.Context, _ store.SearchOptions) (store.SearchOptions, error) {
	<-ctx.Done()
	return store.SearchOptions{}, ctx.Err()
}
func (blockingNormalizeAuthority) RevalidateQMDExportCandidates(context.Context, []store.QMDExportSource, store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
	return nil, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type cancelAtEOFReader struct {
	io.Reader

	cancel context.CancelFunc
}

func (r *cancelAtEOFReader) Read(buffer []byte) (int, error) {
	count, err := r.Reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		r.cancel()
	}
	return count, err
}
func (*cancelAtEOFReader) Close() error { return nil }
