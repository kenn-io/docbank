package loadfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportProfilesAreClosedAndUseDeclaredDialects(t *testing.T) {
	expected := []ExportProfile{
		{ID: "export-dat-pdf-v1", DAT: "dat-concordance-v1", PageMap: "opt-standard-v1", RequiredRoles: []string{"produced_pdf"}, OptionalRoles: []string{"supplied_text", "rendition_text", "native"}},
		{ID: "export-dat-opt-images-v1", DAT: "dat-concordance-v1", PageMap: "opt-standard-v1", RequiredRoles: []string{"page_image"}, OptionalRoles: []string{"rendition_text", "native"}},
		{ID: "export-csv-natives-v1", DAT: "csv-rfc4180-v1", RequiredRoles: []string{"native"}, OptionalRoles: []string{"rendition_text"}},
		{ID: "export-dat-lfp-images-v1", DAT: "dat-concordance-v1", PageMap: "lfp-ipro-v1", RequiredRoles: []string{"page_image"}, OptionalRoles: []string{"supplied_text", "native"}},
	}
	assert.Equal(t, expected, ExportProfiles())
	for _, want := range expected {
		got, err := ReadExportProfile(want.ID)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		_, err = ReadProfile(got.DAT)
		require.NoError(t, err)
		if got.PageMap != "" {
			_, err = ReadProfile(got.PageMap)
			require.NoError(t, err)
		}
	}
	for _, id := range []string{"", "export-dat-opt-tiff-v1", "export-dat-pdf-v2", "dat-concordance-v1"} {
		_, err := ReadExportProfile(id)
		require.ErrorIs(t, err, ErrInvalidProfile)
	}
}

func TestExportProfileResultsCannotMutateRegistry(t *testing.T) {
	profile, err := ReadExportProfile("export-dat-pdf-v1")
	require.NoError(t, err)
	profile.RequiredRoles[0] = "native"
	profile.OptionalRoles[0] = "native"
	profiles := ExportProfiles()
	profiles[0].RequiredRoles[0] = "native"
	profiles[0].OptionalRoles[0] = "native"

	unchanged, err := ReadExportProfile("export-dat-pdf-v1")
	require.NoError(t, err)
	assert.Equal(t, []string{"produced_pdf"}, unchanged.RequiredRoles)
	assert.Equal(t, []string{"supplied_text", "rendition_text", "native"}, unchanged.OptionalRoles)
}

func TestCheckExportRolesRejectsMissingRequiredAndReportsOptionalOmissions(t *testing.T) {
	omitted, err := CheckExportRoles("export-dat-pdf-v1", []string{"produced_pdf", "native"}, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"supplied_text", "rendition_text"}, omitted)

	_, err = CheckExportRoles("export-dat-pdf-v1", []string{"native"}, false)
	require.ErrorIs(t, err, ErrPackageIncomplete)
	_, err = CheckExportRoles("export-dat-opt-images-v1", []string{"produced_pdf"}, false)
	require.ErrorIs(t, err, ErrPackageIncomplete)

	omitted, err = CheckExportRoles("export-dat-pdf-v1", []string{"stamped_pdf", "supplied_text"}, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"rendition_text", "native"}, omitted)
	_, err = CheckExportRoles("export-dat-pdf-v1", []string{"produced_pdf"}, true)
	require.ErrorIs(t, err, ErrPackageIncomplete)

	_, err = CheckExportRoles("export-csv-natives-v1", []string{"native"}, true)
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = CheckExportRoles("export-dat-opt-tiff-v1", []string{"page_image"}, false)
	require.ErrorIs(t, err, ErrInvalidProfile)
}
