package retrieval

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestProviderStagesHoldConsentThroughExecution(t *testing.T) {
	for _, stage := range []ProviderStage{ProviderStageExpansion, ProviderStageReranking} {
		for _, outcome := range []string{"success", "failure", "canceled"} {
			t.Run(string(stage)+"/"+outcome, func(t *testing.T) {
				catalog, err := store.Open(filepath.Join(t.TempDir(), "consent.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, catalog.Close()) })
				_, err = catalog.CreateFile(t.Context(), catalog.RootID(), "needle.txt", strings.Repeat("a", 64), 1, "text/plain")
				require.NoError(t, err)
				request := store.ProviderOperationAuthorizationRequest{
					Principal: "search-user", Scope: "search", ProfileFingerprint: strings.Repeat("b", 64),
					DisclosureFingerprint: strings.Repeat("c", 64), InputClasses: []string{"query_text"},
				}
				if stage == ProviderStageReranking {
					request.InputClasses = []string{"query_text_and_excerpt"}
				}
				_, err = catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
					Principal: request.Principal, Scope: request.Scope, ProfileFingerprint: request.ProfileFingerprint,
					DisclosureFingerprint: request.DisclosureFingerprint, InputClasses: request.InputClasses,
				})
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				authorized, proceed := make(chan struct{}), make(chan struct{})
				started, finish := make(chan struct{}), make(chan struct{})
				authorizer := &consentStageAuthorizer{catalog: catalog, request: request,
					afterAuthorize: func() {
						close(authorized)
						select {
						case <-proceed:
						case <-ctx.Done():
						}
					}}
				provider := &blockingStageProvider{started: started, finish: finish}
				if outcome == "failure" {
					provider.err = errors.New("provider failed")
				}
				config := SearcherConfig{Backend: catalog, Owner: "search-test", LeaseDuration: time.Minute}
				if stage == ProviderStageExpansion {
					config.Expansion = ExpansionConfig{Enabled: true, Profile: ExpansionProfile{ID: "expansion", MaxVariants: 1},
						Provider: provider, Authorizer: authorizer, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
				} else {
					config.Reranking = RerankingConfig{Enabled: true, Profile: RerankingProfile{ID: "reranking", MaxCandidates: 1},
						Provider: provider, Authorizer: authorizer, Deadline: time.Second, FailurePolicy: ProviderFailureFailClosed}
				}
				searcher, err := NewSearcher(config)
				require.NoError(t, err)
				done := make(chan error, 1)
				go func() { _, err := searcher.Search(ctx, Query{Text: "needle", Mode: ModeLexical}); done <- err }()
				<-authorized
				revoked := make(chan error, 1)
				go func() {
					_, err := catalog.RevokeConsent(t.Context(), store.ProcessingConsentRevocationRequest{Principal: request.Principal, Scope: request.Scope})
					revoked <- err
				}()
				early := false
				select {
				case err := <-revoked:
					t.Errorf("revocation completed between authorization and provider execution: %v", err)
					early = true
				case <-time.After(20 * time.Millisecond):
				}
				close(proceed)
				<-started
				if !early {
					select {
					case err := <-revoked:
						t.Errorf("revocation completed during provider execution: %v", err)
						early = true
					case <-time.After(20 * time.Millisecond):
					}
				}
				if outcome == "canceled" {
					cancel()
				} else {
					close(finish)
				}
				searchErr := <-done
				switch outcome {
				case "success":
					require.NoError(t, searchErr)
				case "canceled":
					require.ErrorIs(t, searchErr, context.Canceled)
				case "failure":
					require.Error(t, searchErr)
				}
				if !early {
					require.NoError(t, <-revoked)
				}
				authorizer.afterAuthorize = nil
				_, err = searcher.Search(t.Context(), Query{Text: "needle", Mode: ModeLexical})
				require.Error(t, err, "revoked consent must reject the next provider call")
			})
		}
	}
}

type consentStageAuthorizer struct {
	catalog        *store.Store
	request        store.ProviderOperationAuthorizationRequest
	afterAuthorize func()
}

func (authorizer *consentStageAuthorizer) AuthorizeExpansion(ctx context.Context, _ ProviderOperation) (ProviderEgressLease, error) {
	_, fence, err := authorizer.catalog.BeginProviderEgress(ctx, authorizer.request)
	if err == nil && authorizer.afterAuthorize != nil {
		authorizer.afterAuthorize()
	}
	if err != nil {
		return nil, err
	}
	return fence, nil
}

func (authorizer *consentStageAuthorizer) AuthorizeReranking(ctx context.Context, operation ProviderOperation) (ProviderEgressLease, error) {
	return authorizer.AuthorizeExpansion(ctx, operation)
}

type blockingStageProvider struct {
	started, finish chan struct{}
	err             error
}

func (provider *blockingStageProvider) Expand(ctx context.Context, _ ExpansionRequest) ([]string, error) {
	close(provider.started)
	select {
	case <-provider.finish:
		return nil, provider.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (provider *blockingStageProvider) Rerank(ctx context.Context, request RerankingRequest) ([]RerankScore, error) {
	close(provider.started)
	select {
	case <-provider.finish:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if provider.err != nil {
		return nil, provider.err
	}
	return []RerankScore{{Document: request.Candidates[0].Document, Score: 1}}, nil
}
