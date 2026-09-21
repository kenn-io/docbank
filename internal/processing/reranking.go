package processing

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

// rerankingAuthorizer keeps reranking consent separate from embedding consent.
// A semantic search already owns the shared egress fence, so its reranking
// check reads the second grant without taking the same fence again.
type rerankingAuthorizer struct {
	catalog               *store.Store
	principal             string
	scope                 string
	profileFingerprint    string
	disclosureFingerprint string
	queryFenceHeld        bool
}

func (authorizer *rerankingAuthorizer) AuthorizeReranking(
	ctx context.Context, operation retrieval.ProviderOperation,
) (retrieval.ProviderEgressLease, error) {
	if authorizer == nil || authorizer.catalog == nil ||
		operation.InputClass != retrieval.ProviderInputQueryAndExcerpt {
		return nil, errors.New("reranking authorization is unavailable")
	}
	request := store.ProviderOperationAuthorizationRequest{
		Principal:             authorizer.principal,
		Scope:                 authorizer.scope,
		ProfileFingerprint:    authorizer.profileFingerprint,
		DisclosureFingerprint: authorizer.disclosureFingerprint,
		InputClasses:          []string{string(operation.InputClass)},
	}
	if authorizer.queryFenceHeld {
		if _, err := authorizer.catalog.AuthorizeProviderOperation(ctx, request); err != nil {
			return nil, err
		}
		return noOpProviderEgressLease{}, nil
	}
	_, fence, err := authorizer.catalog.BeginProviderEgress(ctx, request)
	if err != nil {
		return nil, err
	}
	return fence, nil
}

type noOpProviderEgressLease struct{}

func (noOpProviderEgressLease) Close() {}
