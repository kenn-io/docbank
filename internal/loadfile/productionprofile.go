package loadfile

import (
	"errors"
	"fmt"
	"slices"
)

// ErrProductionRoleNotAllowed reports a role that could expose an original or
// another representation outside the redacted production allowlist.
var ErrProductionRoleNotAllowed = errors.New("production_role_not_allowed: role is outside the redacted export allowlist")

// ProductionRoleBinding translates one immutable redacted artifact role into
// the representation role consumed by an existing load-file export profile.
type ProductionRoleBinding struct {
	ProductionRole string
	ExportRole     string
}

// ProductionExportPlan is the narrow compatibility boundary between redacted
// production artifacts and an accepted load-file export profile. Bindings are
// exhaustive: callers must not add the profile's optional native or source
// representations.
type ProductionExportPlan struct {
	ProfileID string
	DAT       string
	PageMap   string
	Bindings  []ProductionRoleBinding
}

// PlanProductionExport accepts only the exact redacted artifact set required
// by a supported load-file profile and returns deterministic role bindings.
// A Bates PDF plan binds the redacted PDF to the accepted stamped output role.
func PlanProductionExport(profileID string, availableRoles []string, bates bool) (ProductionExportPlan, error) {
	profile, err := ReadExportProfile(profileID)
	if err != nil {
		return ProductionExportPlan{}, err
	}
	if bates && profileID != "export-dat-pdf-v1" {
		return ProductionExportPlan{}, fmt.Errorf("%w: Bates output requires the PDF export profile", ErrInvalidProfile)
	}

	bindings, ok := productionRoleBindings(profileID, bates)
	if !ok {
		return ProductionExportPlan{}, fmt.Errorf("%w: export profile %q is not safe for redacted production", ErrInvalidProfile, profileID)
	}
	for _, binding := range bindings {
		declaredRole := binding.ExportRole
		if bates && declaredRole == "stamped_pdf" {
			declaredRole = "produced_pdf"
		}
		if !slices.Contains(profile.RequiredRoles, declaredRole) && !slices.Contains(profile.OptionalRoles, declaredRole) {
			return ProductionExportPlan{}, fmt.Errorf("%w: export profile %q does not declare role %q", ErrInvalidProfile, profileID, binding.ExportRole)
		}
	}

	allowed := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		allowed[binding.ProductionRole] = struct{}{}
	}
	seen := make(map[string]struct{}, len(availableRoles))
	for _, role := range availableRoles {
		if _, ok := allowed[role]; !ok {
			return ProductionExportPlan{}, fmt.Errorf("%w: %q", ErrProductionRoleNotAllowed, role)
		}
		if _, duplicate := seen[role]; duplicate {
			return ProductionExportPlan{}, fmt.Errorf("%w: duplicate %q", ErrProductionRoleNotAllowed, role)
		}
		seen[role] = struct{}{}
	}
	for _, binding := range bindings {
		if _, ok := seen[binding.ProductionRole]; !ok {
			return ProductionExportPlan{}, fmt.Errorf("%w: %s", ErrPackageIncomplete, binding.ProductionRole)
		}
	}

	return ProductionExportPlan{
		ProfileID: profile.ID,
		DAT:       profile.DAT,
		PageMap:   profile.PageMap,
		Bindings:  bindings,
	}, nil
}

func productionRoleBindings(profileID string, bates bool) ([]ProductionRoleBinding, bool) {
	switch profileID {
	case "export-dat-pdf-v1":
		pdfRole := "produced_pdf"
		if bates {
			pdfRole = "stamped_pdf"
		}
		return []ProductionRoleBinding{
			{ProductionRole: "redacted_pdf", ExportRole: pdfRole},
			{ProductionRole: "redacted_text", ExportRole: "supplied_text"},
		}, true
	case "export-dat-opt-images-v1":
		return []ProductionRoleBinding{
			{ProductionRole: "redacted_page", ExportRole: "page_image"},
			{ProductionRole: "redacted_text", ExportRole: "rendition_text"},
		}, true
	case "export-dat-lfp-images-v1":
		return []ProductionRoleBinding{
			{ProductionRole: "redacted_page", ExportRole: "page_image"},
			{ProductionRole: "redacted_text", ExportRole: "supplied_text"},
		}, true
	default:
		return nil, false
	}
}
