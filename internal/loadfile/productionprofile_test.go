package loadfile

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanProductionExportBindsOnlyRedactedRoles(t *testing.T) {
	tests := []struct {
		name      string
		profileID string
		roles     []string
		bates     bool
		want      ProductionExportPlan
	}{
		{
			name:      "DAT with PDF",
			profileID: "export-dat-pdf-v1",
			roles:     []string{"redacted_text", "redacted_pdf"},
			want: ProductionExportPlan{
				ProfileID: "export-dat-pdf-v1",
				DAT:       "dat-concordance-v1",
				PageMap:   "opt-standard-v1",
				Bindings: []ProductionRoleBinding{
					{ProductionRole: "redacted_pdf", ExportRole: "produced_pdf"},
					{ProductionRole: "redacted_text", ExportRole: "supplied_text"},
				},
			},
		},
		{
			name:      "DAT with Bates PDF",
			profileID: "export-dat-pdf-v1",
			roles:     []string{"redacted_text", "redacted_pdf"},
			bates:     true,
			want: ProductionExportPlan{
				ProfileID: "export-dat-pdf-v1",
				DAT:       "dat-concordance-v1",
				PageMap:   "opt-standard-v1",
				Bindings: []ProductionRoleBinding{
					{ProductionRole: "redacted_pdf", ExportRole: "stamped_pdf"},
					{ProductionRole: "redacted_text", ExportRole: "supplied_text"},
				},
			},
		},
		{
			name:      "DAT with OPT images",
			profileID: "export-dat-opt-images-v1",
			roles:     []string{"redacted_text", "redacted_page"},
			want: ProductionExportPlan{
				ProfileID: "export-dat-opt-images-v1",
				DAT:       "dat-concordance-v1",
				PageMap:   "opt-standard-v1",
				Bindings: []ProductionRoleBinding{
					{ProductionRole: "redacted_page", ExportRole: "page_image"},
					{ProductionRole: "redacted_text", ExportRole: "rendition_text"},
				},
			},
		},
		{
			name:      "DAT with LFP images",
			profileID: "export-dat-lfp-images-v1",
			roles:     []string{"redacted_text", "redacted_page"},
			want: ProductionExportPlan{
				ProfileID: "export-dat-lfp-images-v1",
				DAT:       "dat-concordance-v1",
				PageMap:   "lfp-ipro-v1",
				Bindings: []ProductionRoleBinding{
					{ProductionRole: "redacted_page", ExportRole: "page_image"},
					{ProductionRole: "redacted_text", ExportRole: "supplied_text"},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := PlanProductionExport(test.profileID, test.roles, test.bates)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)

			profile, err := ReadExportProfile(test.profileID)
			require.NoError(t, err)
			available := make([]string, 0, len(got.Bindings))
			for _, binding := range got.Bindings {
				available = append(available, binding.ExportRole)
				declaredRole := binding.ExportRole
				if test.bates && declaredRole == "stamped_pdf" {
					declaredRole = "produced_pdf"
				}
				assert.True(t, slices.Contains(profile.RequiredRoles, declaredRole) ||
					slices.Contains(profile.OptionalRoles, declaredRole), "undeclared export role %q", binding.ExportRole)
			}
			_, err = CheckExportRoles(test.profileID, available, test.bates)
			require.NoError(t, err)
		})
	}
}

func TestPlanProductionExportRejectsIncompleteOrUnsafeRoleSets(t *testing.T) {
	_, err := PlanProductionExport("export-dat-pdf-v1", []string{"redacted_pdf"}, false)
	require.ErrorIs(t, err, ErrPackageIncomplete)

	for _, roles := range [][]string{
		{"redacted_pdf", "redacted_text", "native"},
		{"redacted_pdf", "redacted_text", "supplied_text"},
		{"redacted_pdf", "redacted_text", "rendition_text"},
		{"redacted_pdf", "redacted_text", "produced_pdf"},
		{"redacted_pdf", "redacted_text", "page_image"},
		{"redacted_pdf", "redacted_text", "stamped_pdf"},
		{"redacted_pdf", "redacted_text", "redacted_text"},
		{"redacted_pdf", "redacted_text", "unknown"},
	} {
		_, err = PlanProductionExport("export-dat-pdf-v1", roles, false)
		require.ErrorIs(t, err, ErrProductionRoleNotAllowed, "roles: %v", roles)
	}

	_, err = PlanProductionExport("export-csv-natives-v1", []string{"native"}, false)
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = PlanProductionExport("export-dat-opt-images-v1", []string{"redacted_page", "redacted_text"}, true)
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = PlanProductionExport("export-dat-pdf-v2", []string{"redacted_pdf", "redacted_text"}, false)
	require.ErrorIs(t, err, ErrInvalidProfile)
}

func TestProductionExportPlanResultsCannotMutateBindings(t *testing.T) {
	plan, err := PlanProductionExport("export-dat-pdf-v1", []string{"redacted_pdf", "redacted_text"}, false)
	require.NoError(t, err)
	plan.Bindings[0].ExportRole = "native"

	unchanged, err := PlanProductionExport("export-dat-pdf-v1", []string{"redacted_pdf", "redacted_text"}, false)
	require.NoError(t, err)
	assert.Equal(t, "produced_pdf", unchanged.Bindings[0].ExportRole)
}
