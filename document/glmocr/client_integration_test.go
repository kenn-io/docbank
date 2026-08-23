package glmocr_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/glmocr"
	"go.kenn.io/docbank/document/ocr"
)

func TestClientAgainstLocalService(t *testing.T) {
	endpoint := os.Getenv("GLMOCR_INTEGRATION_ENDPOINT")
	fixture := os.Getenv("GLMOCR_INTEGRATION_FIXTURE")
	if endpoint == "" || fixture == "" {
		t.Skip("set GLMOCR_INTEGRATION_ENDPOINT and GLMOCR_INTEGRATION_FIXTURE")
	}
	content, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	mediaType := "image/png"
	if filepath.Ext(fixture) == ".pdf" {
		mediaType = "application/pdf"
	}
	stream, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	source, err := ocr.NewSource(stream, mediaType, int64(len(content)), hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	normalize, err := document.NewNormalizePolicy(1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := glmocr.NewPolicy(glmocr.PolicyConfig{
		Endpoint: endpoint, MaxDocumentBytes: glmocr.MaxDocumentBytes,
		MaxResponseBytes: glmocr.MaxResponseBytes, MaxUnits: glmocr.MaxUnits, NormalizePolicy: normalize,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Process(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Document.Units) == 0 || result.Document.Units[0].Text == "" {
		t.Fatalf("empty normalized result: units=%d", len(result.Document.Units))
	}
	if result.Identity.Provider != glmocr.Provider || result.Identity.Model != glmocr.DefaultModel {
		t.Fatalf("unexpected identity: %+v", result.Identity)
	}
}
