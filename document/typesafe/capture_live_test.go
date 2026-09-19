//go:build typesafe_capture

package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
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
	directory := os.Getenv("DOCBANK_TYPESAFE_CAPTURE_DIR")
	if directory == "" {
		t.Fatal("DOCBANK_TYPESAFE_CAPTURE_DIR is required")
	}
	requestBody := []byte("{\"state\":{\"query\":\"synthetic question\",\"candidate\":\"synthetic candidate\"},\"model\":\"jev-1.13.0\",\"questions\":{\"matches\":{\"type\":\"noul\",\"instructions\":\"Could `candidate` be the best answer to `query`?\",\"criteria\":{\"true\":\"The candidate contains the specific information needed to answer the query.\",\"false\":\"The candidate is only topically similar or does not contain the needed evidence.\"}}}}")
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.typesafe.ai/v1/systemone", bytes.NewReader(requestBody))
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
	var body json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	record := struct {
		Request     json.RawMessage `json:"request"`
		Status      int             `json:"status"`
		ContentType string          `json:"content_type"`
		Response    json.RawMessage `json:"response"`
	}{requestBody, response.StatusCode, response.Header.Get("Content-Type"), body}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "capture.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}
