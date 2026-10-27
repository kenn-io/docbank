package daemonconn

import (
	"reflect"
	"testing"

	"go.kenn.io/docbank/internal/api"
)

func TestAutomaticSearchCandidates(t *testing.T) {
	profiles := []api.ProcessingProfileSummary{{Name: "z", QueryEmbeddingBindings: []string{"b", "a"}}, {Name: "a", QueryEmbeddingBindings: []string{"x"}}}
	got := AutomaticSearchCandidates(profiles)
	want := []SearchCandidate{{"a", "x"}, {"z", "a"}, {"z", "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if profiles[0].QueryEmbeddingBindings[0] != "b" {
		t.Fatal("mutated input")
	}
	for _, code := range []string{"not_found", "processing_failed", "processing_consent_expired"} {
		if !BindingSearchFailure(code) {
			t.Fatalf("%s must retry", code)
		}
	}
	for _, code := range []string{"unauthorized", "stale_version", "reranking_unavailable"} {
		if BindingSearchFailure(code) {
			t.Fatalf("%s must not retry", code)
		}
	}
}
