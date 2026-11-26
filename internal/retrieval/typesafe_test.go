package retrieval

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"go.kenn.io/docbank/document/providerhttp"
	wire "go.kenn.io/docbank/document/typesafe"
)

func typesafeIdentity(number int64) DocumentIdentity {
	return DocumentIdentity{VaultID: "vault", NodeID: number, ContentVersionID: fmt.Sprintf("version-%d", number)}
}

func TestRerankSendsOnlyExcerptsAndMapsEveryIdentity(t *testing.T) {
	profile := wire.Profile{SecretBinding: "test", EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.typesafe.ai", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}
	fake := newTypesafeFake(profile, func(_, candidate string) (float64, error) {
		if candidate == "excerpt-0" {
			return 0.2, nil
		}
		return 0.8, nil
	})
	client, err := NewTypesafeClient(fake)
	if err != nil {
		t.Fatal(err)
	}
	request := RerankingRequest{Query: "query", Candidates: []RerankingCandidate{
		{Document: typesafeIdentity(1), Excerpt: "excerpt-0", Evidence: []EvidenceReference{{VaultID: "vault-private"}}},
		{Document: typesafeIdentity(2), Excerpt: "excerpt-1", Evidence: []EvidenceReference{{BuildID: "build-private"}}},
	}}
	scores, err := client.Rerank(context.Background(), request)
	if err != nil || len(scores) != 2 || scores[0].Document != request.Candidates[0].Document || scores[1].Score != 0.8 {
		t.Fatalf("scores=%+v err=%v", scores, err)
	}
	requests := fake.Requests()
	if len(requests) != 1 || requests[0].Query != "query" || !slices.Equal(requests[0].Candidates, []string{"excerpt-0", "excerpt-1"}) {
		t.Fatalf("provider request = %+v", requests)
	}
	if got := fmt.Sprintf("%#v", requests); got == "" || containsAny(got, "vault-private", "build-private", "segment-private") {
		t.Fatalf("identity leaked into provider request: %s", got)
	}
}

func TestRerankRejectsIncompleteOrDuplicateIdentitiesBeforeProvider(t *testing.T) {
	profile := wire.Profile{SecretBinding: "test", EgressPolicy: providerhttp.EgressPolicy{Scheme: "https", Host: "api.typesafe.ai", Port: 443, AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}
	fake := newTypesafeFake(profile, func(_, _ string) (float64, error) { return 0.5, nil })
	client, err := NewTypesafeClient(fake)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RerankingRequest{
		{Query: "q", Candidates: []RerankingCandidate{{Document: DocumentIdentity{VaultID: "vault"}}}},
		{Query: "q", Candidates: []RerankingCandidate{{Document: typesafeIdentity(1)}, {Document: typesafeIdentity(1)}}},
	} {
		if _, err := client.Rerank(context.Background(), bad); err == nil || len(fake.Requests()) != 0 {
			t.Fatalf("err=%v requests=%v", err, fake.Requests())
		}
	}
}

func containsAny(value string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
