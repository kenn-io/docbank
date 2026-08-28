package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestRenditionCLIEmitsExactSelfDescribingMarkdown(t *testing.T) {
	body := []byte("# Synthetic report\n\nprivate fixture\n")
	evidenceHash := sha256Hex("evidence")
	rendering := document.RenditionV1{ContractVersion: document.RenditionContractV1,
		Completeness: document.EvidenceComplete, EvidenceChecksum: evidenceHash,
		Markdown: body, MarkdownChecksum: sha256HexBytes(body),
		Units: []document.NormalizedUnitV1{{EvidenceUnitID: "page:000000", Order: 0, Text: string(body),
			Locator: document.EvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginZero}}}}
	rendering.Checksum = sha256Hex("rendition")
	buildID := sha256Hex("build")
	enveloped, _, err := document.EnvelopeRenditionV1(rendering, document.RenditionEnvelopeV1{
		BuildID: buildID, SourceSHA256: sha256Hex("source"), SourceFormat: "pdf",
		SourceMediaType: "application/pdf", RenditionRequestFingerprint: sha256Hex("request"),
		EvidenceLexicalFingerprint: sha256Hex("lexical"),
		NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, UnitKind: document.EvidenceUnitPage,
	})
	require.NoError(t, err)
	artifact := enveloped.Markdown
	attachmentID := sha256Hex("attachment")
	artifactID := sha256Hex("artifact")
	artifactHash := sha256.Sum256(artifact)
	digest := "sha-256=:" + base64.StdEncoding.EncodeToString(artifactHash[:]) + ":"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/renditions/"+attachmentID, request.URL.Path)
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set(api.RenditionAttachmentHeader, attachmentID)
		w.Header().Set(api.RenditionBuildHeader, buildID)
		w.Header().Set(api.RenditionArtifactHeader, artifactID)
		w.Header().Set(api.BlobHashHeader, hex.EncodeToString(artifactHash[:]))
		w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(artifact)))
		w.Header().Set(api.ContentVersionHeader, processingTestVersionID)
		w.Header().Set("Trailer", "Content-Digest")
		_, writeErr := w.Write(artifact)
		assert.NoError(t, writeErr)
		w.Header().Set("Content-Digest", digest)
	}))
	t.Cleanup(server.Close)

	command, output := processingTestCommand()
	require.NoError(t, runRenditionGet(command, client.New(server.URL, "test-key"), attachmentID, 1<<20))
	assert.True(t, bytes.Equal(artifact, output.Bytes()))
	assert.True(t, bytes.HasPrefix(output.Bytes(), []byte("---\ndocbank:\n")))
}

func sha256Hex(value string) string { return sha256HexBytes([]byte(value)) }

func sha256HexBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
