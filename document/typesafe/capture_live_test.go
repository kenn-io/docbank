//go:build typesafe_capture

package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/docbank/document/providerhttp"
)

func TestLiveCaptureSystemOne(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Skip("TYPESAFE_API_KEY is not set")
	}
	for _, test := range []struct {
		name       string
		shape      RequestShape
		candidates []string
	}{{"per_candidate", RequestShapePerCandidate, []string{"synthetic candidate"}},
		{"batched", RequestShapeBatched, []string{"synthetic first", "synthetic second"}}} {
		t.Run(test.name, func(t *testing.T) {
			profile := testProfile()
			profile.RequestShape = test.shape
			calls, err := encodeCalls(profile, RerankRequest{Query: "synthetic question", Candidates: test.candidates})
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 {
				t.Fatalf("encoded calls = %d", len(calls))
			}
			captureCall(t, key, calls[0].payload, capturePath(test.name))
		})
	}
}

func captureCall(t *testing.T, key string, requestBody []byte, path string) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://api.typesafe.ai/v1/systemone", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	transport, err := providerhttp.NewTransport(providerhttp.EgressPolicy{
		Scheme: "https", Host: "api.typesafe.ai", Port: 443,
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport, CheckRedirect: providerhttp.RefuseRedirects}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	record := struct {
		Request     json.RawMessage `json:"request"`
		Status      int             `json:"status"`
		ContentType string          `json:"content_type"`
		Response    json.RawMessage `json:"response"`
	}{requestBody, response.StatusCode, response.Header.Get("Content-Type"), responseBody}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func capturePath(shape string) string {
	return filepath.Join("testdata", "capture_"+shape+".json")
}
