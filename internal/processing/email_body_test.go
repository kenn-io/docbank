package processing

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/emailmime"
)

func TestEmailBodyPreservesFullSupportedText(t *testing.T) {
	text := strings.Repeat("x", (16<<20)-12) + "final-marker"
	profile, err := document.EmailBodyProfileV1(emailmime.Recipe())
	require.NoError(t, err)
	ep, rp, err := document.RenditionExecutionPoliciesForProfileV1(profile)
	require.NoError(t, err)
	units, err := bodyUnits(t.Context(), text)
	require.NoError(t, err)
	require.LessOrEqual(t, len(units), 17)
	source := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceDegradedProvenance, Family: "text", UnitKind: document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "Selected body converted to derived generic text blocks; exact MIME authority retained separately."}},
		Units:     units,
	}
	evidence, err := document.NormalizeEvidenceV1(source, ep)
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(evidence, rp)
	require.NoError(t, err)
	for _, warning := range rendition.Warnings {
		require.NotEqual(t, "truncated", warning.Code)
	}
	require.Contains(t, rendition.Units[len(rendition.Units)-1].Text, "final-marker")
}

func TestEmailBodyLiteralUnicodeAndLimits(t *testing.T) {
	for _, text := range []string{"<script>literalanglemarker</script> ``` & [x](javascript:alert(1))", strings.Repeat("`", 1<<20), strings.Repeat("a", (1<<20)-1) + "🌍尾"} {
		units, err := bodyUnits(t.Context(), text)
		require.NoError(t, err)
		var recovered strings.Builder
		for _, u := range units {
			require.True(t, utf8.ValidString(u.Text))
			require.LessOrEqual(t, len(u.Text), 3*(1<<20)+4)
			first := strings.IndexByte(u.Text, '\n')
			last := strings.LastIndexByte(u.Text, '\n')
			recovered.WriteString(u.Text[first+1 : last])
		}
		require.Equal(t, text, recovered.String())
		profile, err := document.EmailBodyProfileV1(emailmime.Recipe())
		require.NoError(t, err)
		ep, rp, err := document.RenditionExecutionPoliciesForProfileV1(profile)
		require.NoError(t, err)
		evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceDegradedProvenance, Family: "text", UnitKind: document.EvidenceUnitGeneric, Omissions: []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "Selected body converted to derived generic text blocks; exact MIME authority retained separately."}}, Units: units}, ep)
		require.NoError(t, err)
		rendition, err := document.BuildRenditionV1(evidence, rp)
		require.NoError(t, err)
		require.NoError(t, checkEmailBodyRendition(rendition))
		for index, unit := range rendition.Units {
			input := units[index].Text
			first, last := strings.IndexByte(input, '\n'), strings.LastIndexByte(input, '\n')
			require.Contains(t, unit.Text, input[first+1:last])
		}
	}
	_, err := bodyUnits(t.Context(), string([]byte{0xff}))
	require.Error(t, err)
	_, err = bodyUnits(t.Context(), strings.Repeat("x", (16<<20)+1))
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = bodyUnits(ctx, "body")
	require.ErrorIs(t, err, context.Canceled)
}
func TestEmailBodyStructuralText(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"<p>A &amp; B</p><div>C<br>D</div>", "A & B\n\nC\nD"},
		{"<script>scriptsecret</script><style>stylesecret</style><noscript>nosecret</noscript><template>hidden<template>inner</template>stillhidden</template><p>visible</p>", "visible"},
		{"<script/>scriptsecret<p>stillsecret</p>", ""},
		{"<!-- commentsecret --><p>visible &lt;literal&gt;</p>", "visible <literal>"},
	} {
		got, err := emailBodyText(t.Context(), "html", []byte(tc.input))
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	_, err := emailBodyText(t.Context(), "html", []byte("≂̸"))
	require.NoError(t, err)
	// Each five-byte entity expands to six UTF-8 bytes, crossing the text cap.
	_, err = emailBodyText(t.Context(), "html", []byte(strings.Repeat("&nGt;", (16<<20)/5)))
	require.ErrorIs(t, err, emailBodyUnavailableError("body_text_limit"))
}

func TestEmailBodyRenderTruncationRefused(t *testing.T) {
	ep, err := document.NewEvidencePolicy(1000)
	require.NoError(t, err)
	units, err := bodyUnits(t.Context(), strings.Repeat("before after tail ", 10))
	require.NoError(t, err)
	units[0].Order = 1
	units = append([]document.SourceEvidenceUnitV1{{Order: 0, Text: "prefix", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone}}}, units...)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceDegradedProvenance, Family: "text", UnitKind: document.EvidenceUnitGeneric, Omissions: []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "Selected body converted to derived generic text blocks; exact MIME authority retained separately."}}, Units: units}, ep)
	require.NoError(t, err)
	rp, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 100, MaxUnitRunes: 1000, MaxSegmentRunes: 12})
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(evidence, rp)
	require.NoError(t, err)
	require.ErrorIs(t, checkEmailBodyRendition(rendition), emailBodyUnavailableError("body_render_limit"))
}

func TestEmailBodyFullUnicodeNeedsSeventeenUnits(t *testing.T) {
	text := strings.Repeat("€", (16<<20)/3) + "x"
	units, err := bodyUnits(t.Context(), text)
	require.NoError(t, err)
	require.Len(t, units, 17)
	var recovered strings.Builder
	for _, unit := range units {
		require.True(t, utf8.ValidString(unit.Text))
		first, last := strings.IndexByte(unit.Text, '\n'), strings.LastIndexByte(unit.Text, '\n')
		recovered.WriteString(unit.Text[first+1 : last])
	}
	require.Equal(t, text, recovered.String())
}
