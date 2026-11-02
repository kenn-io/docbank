package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func TestPDFFactSearchMatchesRecordedEvidence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	nodes := make([]Node, 4)
	for i, name := range []string{"unknown.pdf", "plain.pdf", "changed.pdf", "script.pdf"} {
		var err error
		nodes[i], err = s.CreateFile(t.Context(), s.RootID(), name, fmt.Sprintf("%064x", i+1), 12, "application/pdf")
		require.NoError(t, err)
		if i == 0 {
			continue
		}
		fields := []document.SourceMetadataFieldV1{}
		for _, fact := range []struct {
			key   string
			value document.SourceMetadataValueV1
		}{
			{"encrypted", document.SourceMetadataValueV1{Kind: document.SourceMetadataBoolean, Boolean: new(i == 2)}},
			{"last_signature_covers_file", document.SourceMetadataValueV1{Kind: document.SourceMetadataBoolean, Boolean: new(i != 2)}},
			{"has_javascript", document.SourceMetadataValueV1{Kind: document.SourceMetadataBoolean, Boolean: new(i == 3)}},
			{"has_launch_actions", document.SourceMetadataValueV1{Kind: document.SourceMetadataBoolean, Boolean: new(i == 2)}},
			{"signature_field_count", document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(int64(i - 1))}},
			{"embedded_file_count", document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(int64(i - 1))}},
			{"permissions", document.SourceMetadataValueV1{Kind: document.SourceMetadataStringList, Strings: []string{"Print", "High-quality print flag"}}},
		} {
			if fact.key == "last_signature_covers_file" && i == 1 || fact.key == "permissions" && i != 2 {
				continue
			}
			sourceField := fact.key
			if fact.key == "permissions" {
				sourceField = "Permissions"
			}
			fields = append(fields, document.SourceMetadataFieldV1{Key: "pdf.info." + fact.key, Namespace: "pdf.info", SourceField: sourceField, Value: fact.value})
		}
		canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: fields})
		require.NoError(t, err)
		_, err = s.PublishSourceMetadata(t.Context(), nodes[i].BlobHash, fakeHash("f1"), canonical)
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		filter string
		want   []int64
	}{
		{"pdf_encrypted:true", []int64{nodes[2].ID}},
		{"pdf_encrypted:false", []int64{nodes[1].ID, nodes[3].ID}},
		{"pdf_embedded_file_count_min:1", []int64{nodes[2].ID, nodes[3].ID}},
		{"pdf_last_signature_covers_file:false", []int64{nodes[2].ID}},
		{"pdf_last_signature_covers_file:true", []int64{nodes[3].ID}},
		{"pdf_has_javascript:true", []int64{nodes[3].ID}},
		{"pdf_has_launch_actions:true", []int64{nodes[2].ID}},
		{"pdf_signature_field_count:0", []int64{nodes[1].ID}},
		{"pdf_signature_field_count_max:1", []int64{nodes[1].ID, nodes[2].ID}},
		{"pdf_embedded_file_count:0", []int64{nodes[1].ID}},
		{`pdf_permissions:"Print"`, []int64{nodes[2].ID}},
		{`pdf_permissions:"print"`, nil},
		{`pdf_permissions:"High-quality"`, nil},
		{"pdf_embedded_file_count_min:1 OR pdf_has_javascript:true", []int64{nodes[2].ID, nodes[3].ID}},
		{"pdf_encrypted:(true OR false) AND NOT pdf_has_javascript:true", []int64{nodes[1].ID, nodes[2].ID}},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			compiled, err := compileQuery(t.Context(), compilerQuery(t, tc.filter), nil)
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.want, compiledFixtureIDs(t, s.db, compiled, ""))
		})
	}

	_, _, err := s.ReplaceContent(t.Context(), nodes[3].ID, nodes[3].Revision, fakeHash("a9"), 12, "application/pdf")
	require.NoError(t, err)
	hits, _, err := s.SearchPageWithOptions(t.Context(), "", 50, SearchOptions{PDFFilter: "pdf_has_javascript:true", MIMEType: "application/pdf"})
	require.NoError(t, err)
	assert.Empty(t, hits, "historical bytes must not match the current PDF")
}

func TestPDFFilterRejectsInvalidOperands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		want string
	}{
		{"tag:urgent", "references are not supported here"},
		{"name:report", "PDF filters require PDF fact fields"},
		{"report", "PDF filters require PDF fact fields"},
		{"pdf_encrypted:yes", "invalid pdf_encrypted operand"},
		{"pdf_encrypted:TRUE", "invalid pdf_encrypted operand"},
		{"pdf_embedded_file_count:>=1", "size operand must be a decimal nonnegative safe integer"},
		{"pdf_embedded_file_count:-1", "size operand must be a decimal nonnegative safe integer"},
		{"pdf_embedded_file_count:9007199254740992", "size operand must be a decimal nonnegative safe integer"},
		{"pdf_has_javascript:true*", "scalar operands cannot use prefix matching"},
		{"pdf_encrypted:(mime:application/pdf)", "nested field overrides are not supported"},
		{"pdf_encrypted:true NEAR/5 pdf_has_javascript:false", "NEAR is supported only for text and name operands"},
	} {
		_, err := compilePDFFilter(t.Context(), tc.text)
		expressionErr, ok := errors.AsType[*query.ExpressionError](err)
		require.True(t, ok, "%s: %v", tc.text, err)
		assert.Equal(t, tc.want, expressionErr.Message, tc.text)
	}
}

func TestPDFFilterNegationMatchesUnknownEvidenceWithText(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	file, err := s.CreateFile(t.Context(), s.RootID(), "review.pdf", fakeHash("a1"), 12, "application/pdf")
	require.NoError(t, err)
	for _, text := range []string{"", "review"} {
		hits, _, err := s.SearchPageWithOptions(t.Context(), text, 50, SearchOptions{PDFFilter: "NOT pdf_has_javascript:true"})
		require.NoError(t, err)
		require.Len(t, hits, 1)
		assert.Equal(t, file.ID, hits[0].Node.ID)
	}
}

func TestPDFFactSearchRejectsInvalidGenerations(t *testing.T) {
	t.Parallel()
	for _, adapter := range []struct {
		name   string
		driver docsqlite.Driver
	}{
		{"default", DefaultSQLiteDriver()}, {"pure Go", modernc.Driver{}},
	} {
		for _, damage := range []string{"checksum", "unknown contract", "malformed JSON"} {
			t.Run(adapter.name+"/"+damage, func(t *testing.T) {
				s, err := Open(t.Context(), filepath.Join(t.TempDir(), "evidence.db"), adapter.driver)
				require.NoError(t, err)
				defer func() { require.NoError(t, s.Close()) }()
				ctx := t.Context()
				node, err := s.CreateFile(ctx, s.RootID(), "evidence.pdf", fakeHash("a1"), 12, "application/pdf")
				require.NoError(t, err)
				canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
					Fields: []document.SourceMetadataFieldV1{{Key: "pdf.info.has_javascript", Namespace: "pdf.info", SourceField: "has_javascript", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataBoolean, Boolean: new(true)}}}})
				require.NoError(t, err)
				_, err = s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("f1"), canonical)
				require.NoError(t, err)
				hits, _, err := s.SearchPageWithOptions(ctx, "", 50, SearchOptions{PDFFilter: "pdf_has_javascript:true"})
				require.NoError(t, err)
				require.Len(t, hits, 1)
				_, err = s.db.ExecContext(ctx, `DROP TRIGGER source_metadata_generations_immutable_update`)
				require.NoError(t, err)
				switch damage {
				case "checksum":
					_, err = s.db.ExecContext(ctx, `UPDATE source_metadata_generations SET checksum=?`, fakeHash("bad"))
				case "unknown contract":
					_, err = s.db.ExecContext(ctx, `UPDATE source_metadata_generations SET contract_version='source-metadata/v2'`)
				case "malformed JSON":
					malformed := append(append([]byte(nil), canonical...), '}')
					checksum := fmt.Sprintf("%x", sha256.Sum256(malformed))
					_, err = s.db.ExecContext(ctx, `UPDATE source_metadata_generations SET canonical_json=?,checksum=?`, malformed, checksum)
				}
				require.NoError(t, err)
				hits, _, err = s.SearchPageWithOptions(ctx, "", 50, SearchOptions{PDFFilter: "pdf_has_javascript:true"})
				require.NoError(t, err)
				assert.Empty(t, hits)
			})
		}
	}
}
