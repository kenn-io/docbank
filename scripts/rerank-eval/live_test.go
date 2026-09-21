//go:build rerank_eval

package rerankeval_test

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/typesafe"
	"go.kenn.io/docbank/internal/retrieval/cohere"
)

func TestLiveRerankComparison(t *testing.T) {
	if !liveKeysPresent() {
		t.Skip("COHERE_API_KEY and TYPESAFE_API_KEY are absent; live clients were not constructed")
	}
	pricing, err := pricingFromEnvironment()
	require.NoError(t, err)
	corpus := loadCorpus(t)
	fixture := newComparisonFixture(t, corpus)
	adapters := make(map[string]comparisonReranker, 3)
	fingerprints := make(map[string]string, 3)
	for _, shape := range []typesafe.RequestShape{typesafe.RequestShapePerCandidate, typesafe.RequestShapeBatched} {
		client, err := newTypeSafeClient(shape)
		require.NoError(t, err)
		arm := "jev-batched"
		if shape == typesafe.RequestShapePerCandidate {
			arm = "jev-per-candidate"
		}
		adapters[arm] = &typeSafeAdapter{client: client, pricing: pricing}
		fingerprints[arm] = client.PolicyFingerprint()
	}
	client, err := newCohereClient()
	require.NoError(t, err)
	adapters["cohere"] = &cohereProviderAdapter{client: client, pricing: pricing}
	fingerprints["cohere"] = client.PolicyFingerprint()
	runner := &providerComparisonRunner{comparisonRunner: &comparisonRunner{fixture: fixture}, adapters: adapters}
	systems := comparisonSystems(fixture.vectorSpace, fingerprints)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	report, err := embeddingeval.Evaluate(ctx, corpus, systems, 1, runner)
	require.NoError(t, err)
	require.Len(t, report.Systems, len(systems))
	require.Equal(t, 18, runner.rerankCalls)
	require.NotEmpty(t, runner.providerSeen)
	require.NotEmpty(t, runner.providerQueries)
	reason := "semantic and hybrid query vectors are synthetic fixtures; live rows record provider measurements and support no quality recommendation"
	if pricing == nil {
		reason += "; no caller-supplied dated pricing was provided, so cost cells remain unavailable"
	}
	t.Log("REAL_LIVE_COMPARISON=Evaluate ran all 12 retrieval/reranker systems and rendered Markdown and JSON")
	t.Log("OWNER_REACHABILITY=TypeSafe per_candidate, TypeSafe batched, and Cohere adapters completed through their clients")
	t.Log(renderComparisonTableForStatus(report, "measured", reason))
	t.Log(renderComparisonJSONForStatus(report, "measured", reason))
}

func TestPricingFromEnvironmentRejectsInvalidValues(t *testing.T) {
	const validDate = "2026-09-21"
	tests := []struct {
		name     string
		date     string
		typesafe string
		cohere   string
	}{
		{name: "missing date", typesafe: "2", cohere: "3"},
		{name: "missing TypeSafe rate", date: validDate, cohere: "3"},
		{name: "missing Cohere rate", date: validDate, typesafe: "2"},
		{name: "invalid date", date: "2026-02-30", typesafe: "2", cohere: "3"},
		{name: "malformed TypeSafe rate", date: validDate, typesafe: "two", cohere: "3"},
		{name: "malformed Cohere rate", date: validDate, typesafe: "2", cohere: "three"},
		{name: "negative TypeSafe rate", date: validDate, typesafe: "-1", cohere: "3"},
		{name: "negative Cohere rate", date: validDate, typesafe: "2", cohere: "-1"},
		{name: "NaN TypeSafe rate", date: validDate, typesafe: "NaN", cohere: "3"},
		{name: "NaN Cohere rate", date: validDate, typesafe: "2", cohere: "NaN"},
		{name: "+Inf TypeSafe rate", date: validDate, typesafe: "+Inf", cohere: "3"},
		{name: "+Inf Cohere rate", date: validDate, typesafe: "2", cohere: "+Inf"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("RERANK_PRICING_DATE", test.date)
			t.Setenv("TYPESAFE_MICROS_PER_TOKEN", test.typesafe)
			t.Setenv("COHERE_MICROS_PER_SEARCH_UNIT", test.cohere)
			pricing, err := pricingFromEnvironment()
			require.Error(t, err)
			require.Nil(t, pricing)
		})
	}
}

type envSecrets struct{}

func (envSecrets) ResolveSecret(ctx context.Context, binding string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	var name string
	switch binding {
	case "synthetic:cohere":
		name = "COHERE_API_KEY"
	case "synthetic:typesafe":
		name = "TYPESAFE_API_KEY"
	default:
		return "", errors.New("unknown synthetic provider secret binding")
	}
	value := os.Getenv(name)
	if value == "" {
		return "", errors.New("synthetic provider secret is absent")
	}
	return value, nil
}

func typesafeProfile(shape typesafe.RequestShape) typesafe.Profile {
	return typesafe.Profile{
		SecretBinding: "synthetic:typesafe", RequestShape: shape,
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "https", Host: "api.typesafe.ai", Port: 443,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
		},
	}
}

func cohereProfile() cohere.Profile {
	return cohere.Profile{
		ID: "synthetic-cohere-rerank", Model: cohere.ModelFast,
		CompatibilityEpoch: "synthetic-v1", ModelRevision: "synthetic-v1",
		SecretBinding: "synthetic:cohere",
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "https", Host: "api.cohere.com", Port: 443,
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
		},
	}
}

func newTypeSafeClient(shape typesafe.RequestShape) (*typesafe.Client, error) {
	return typesafe.New(typesafeProfile(shape), envSecrets{}, nil, http.DefaultClient)
}

func newCohereClient() (*cohere.Client, error) {
	return cohere.New(cohereProfile(), envSecrets{}, nil, http.DefaultClient)
}

func pricingFromEnvironment() (*pricingInput, error) {
	basisDate := strings.TrimSpace(os.Getenv("RERANK_PRICING_DATE"))
	typeSafeValue := strings.TrimSpace(os.Getenv("TYPESAFE_MICROS_PER_TOKEN"))
	cohereValue := strings.TrimSpace(os.Getenv("COHERE_MICROS_PER_SEARCH_UNIT"))
	if basisDate == "" && typeSafeValue == "" && cohereValue == "" {
		return nil, nil
	}
	if basisDate == "" || typeSafeValue == "" || cohereValue == "" {
		return nil, errors.New("all rerank pricing environment values are required together")
	}
	if _, err := time.Parse("2006-01-02", basisDate); err != nil {
		return nil, errors.New("RERANK_PRICING_DATE is invalid")
	}
	typeSafeRate, err := strconv.ParseFloat(typeSafeValue, 64)
	if err != nil {
		return nil, errors.New("TYPESAFE_MICROS_PER_TOKEN is invalid")
	}
	cohereRate, err := strconv.ParseFloat(cohereValue, 64)
	if err != nil {
		return nil, errors.New("COHERE_MICROS_PER_SEARCH_UNIT is invalid")
	}
	pricing := &pricingInput{
		TypeSafeTokens:    &datedRate{Basis: basisDate + ":typesafe-token", MicrosPerUnit: typeSafeRate},
		CohereSearchUnits: &datedRate{Basis: basisDate + ":cohere-search-unit", MicrosPerUnit: cohereRate},
	}
	if _, err := pricing.TypeSafeTokens.cost(0); err != nil {
		return nil, err
	}
	if _, err := pricing.CohereSearchUnits.cost(0); err != nil {
		return nil, err
	}
	return pricing, nil
}
