package qmdbridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/store"
)

func TestSearchOwnsSourceFenceAcrossEveryCallback(t *testing.T) {
	// Mutations caught: aliasing the caller, normalizer result, authorization
	// operation, or final authority scope can replace the selected local source.
	root, source, receipt := exportFixture(t)
	original := source.ContentVersionID
	excluded := "00000000-0000-4000-8000-000000000008"
	callerIDs := []string{original}
	var returnedIDs []string
	authority := sourceFenceAuthority{
		normalize: func(scope store.SearchOptions) (store.SearchOptions, error) {
			callerIDs[0] = excluded
			require.Equal(t, []string{original}, scope.ContentVersionIDs)
			returnedIDs = []string{original}
			scope.ContentVersionIDs = returnedIDs
			return scope, nil
		},
		revalidate: func(candidates []store.QMDExportSource, scope store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
			require.Equal(t, []store.QMDExportSource{source}, candidates)
			require.Equal(t, []string{original}, scope.ContentVersionIDs)
			scope.ContentVersionIDs[0] = excluded
			return []store.QMDExportLiveCandidate{{
				NodeID: source.NodeID, NodeRevision: 4,
				ContentVersionID: original, Path: "/source.pdf",
			}}, nil
		},
	}
	authorizer := authorizerFunc(func(_ context.Context, operation Operation) error {
		require.Equal(t, []string{original}, operation.Scope.ContentVersionIDs)
		operation.Scope.ContentVersionIDs[0] = excluded
		return nil
	})
	payloads := make(chan string, 1)
	server := qmdServer(t, func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read source-fence request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		payloads <- string(body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(resultBody(receipt.Manifest.Entries[0].URI)))
	})
	client := newTestClient(t, server, root, authorizer, authority, 1<<20)
	client.secrets = secretResolverFunc(func(context.Context, string) (string, error) {
		returnedIDs[0] = excluded
		return "synthetic-secret", nil
	})

	results, err := client.Search(t.Context(), Request{
		Searches: []Search{{Type: SearchLexical, Query: "synthetic query"}}, Limit: 1,
		Scope: store.SearchOptions{ContentVersionIDs: callerIDs},
	})
	payload := <-payloads
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, original, results[0].Document.ContentVersionID)
	require.NotContains(t, payload, original)
	require.NotContains(t, payload, excluded)
}

func TestSearchBoundsSourceFenceBeforeCallbacksAndPreservesEmptyScope(t *testing.T) {
	// Mutations caught: cloning an untrusted oversized scope, confusing the
	// candidate bound with the source bound, or treating empty as restricted.
	root, _, _ := exportFixture(t)
	server := staticQMDServer(t, `{"results":[]}`, "application/json", http.StatusOK)
	validRequestWith := func(ids []string) Request {
		return Request{Searches: []Search{{Type: SearchLexical, Query: "synthetic query"}},
			Limit: 1, Scope: store.SearchOptions{ContentVersionIDs: ids}}
	}
	ids := make([]string, store.MaxSearchSourceFenceIDs)
	for index := range ids {
		ids[index] = sourceFenceID(index)
	}

	t.Run("input at bound", func(t *testing.T) {
		var normalized, revalidated int
		authority := sourceFenceAuthority{
			normalize: func(scope store.SearchOptions) (store.SearchOptions, error) {
				normalized++
				require.Len(t, scope.ContentVersionIDs, store.MaxSearchSourceFenceIDs)
				return scope, nil
			},
			revalidate: func(_ []store.QMDExportSource, scope store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
				revalidated++
				require.Len(t, scope.ContentVersionIDs, store.MaxSearchSourceFenceIDs)
				return []store.QMDExportLiveCandidate{}, nil
			},
		}
		client := newTestClient(t, server, root, &authorizerStub{}, authority, 1<<20)
		_, err := client.Search(t.Context(), validRequestWith(ids))
		require.NoError(t, err)
		require.Equal(t, 1, normalized)
		require.Equal(t, 1, revalidated)
	})

	t.Run("input above bound", func(t *testing.T) {
		callbacks := 0
		authority := sourceFenceAuthority{normalize: func(scope store.SearchOptions) (store.SearchOptions, error) {
			callbacks++
			return scope, nil
		}}
		client := newTestClient(t, server, root, &authorizerStub{hook: func() { callbacks++ }}, authority, 1<<20)
		client.secrets = secretStub{hook: func() { callbacks++ }}
		_, err := client.Search(t.Context(), validRequestWith(append(slices.Clone(ids), sourceFenceID(len(ids)))))
		require.EqualError(t, err, "QMD source fence exceeds 4096 content versions")
		require.Zero(t, callbacks)
	})

	t.Run("normalized above bound", func(t *testing.T) {
		callbacks := 0
		authority := sourceFenceAuthority{normalize: func(store.SearchOptions) (store.SearchOptions, error) {
			return store.SearchOptions{ContentVersionIDs: append(slices.Clone(ids), sourceFenceID(len(ids)))}, nil
		}}
		client := newTestClient(t, server, root, &authorizerStub{hook: func() { callbacks++ }}, authority, 1<<20)
		client.secrets = secretStub{hook: func() { callbacks++ }}
		_, err := client.Search(t.Context(), validRequestWith([]string{sourceFenceID(0)}))
		require.EqualError(t, err, "QMD source fence exceeds 4096 content versions")
		require.Zero(t, callbacks)
	})

	for name, sourceIDs := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			authority := sourceFenceAuthority{
				normalize: func(scope store.SearchOptions) (store.SearchOptions, error) {
					require.Equal(t, sourceIDs == nil, scope.ContentVersionIDs == nil)
					return scope, nil
				},
				revalidate: func(_ []store.QMDExportSource, scope store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
					require.Equal(t, sourceIDs == nil, scope.ContentVersionIDs == nil)
					return []store.QMDExportLiveCandidate{}, nil
				},
			}
			client := newTestClient(t, server, root, &authorizerStub{}, authority, 1<<20)
			_, err := client.Search(t.Context(), validRequestWith(sourceIDs))
			require.NoError(t, err)
		})
	}
}

func sourceFenceID(index int) string {
	return "00000000-0000-4000-8000-" + fmtNode(int64(index))
}

type sourceFenceAuthority struct {
	normalize  func(store.SearchOptions) (store.SearchOptions, error)
	revalidate func([]store.QMDExportSource, store.SearchOptions) ([]store.QMDExportLiveCandidate, error)
}

func (a sourceFenceAuthority) NormalizeQMDSearchScope(_ context.Context, scope store.SearchOptions) (store.SearchOptions, error) {
	if a.normalize == nil {
		return scope, nil
	}
	return a.normalize(scope)
}

func (a sourceFenceAuthority) RevalidateQMDExportCandidates(_ context.Context, candidates []store.QMDExportSource, scope store.SearchOptions) ([]store.QMDExportLiveCandidate, error) {
	if a.revalidate == nil {
		return nil, errors.New("unexpected revalidation")
	}
	return a.revalidate(candidates, scope)
}

type secretResolverFunc func(context.Context, string) (string, error)

func (f secretResolverFunc) ResolveSecret(ctx context.Context, binding string) (string, error) {
	return f(ctx, binding)
}
