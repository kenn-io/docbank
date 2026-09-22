package loadfile

import (
	"errors"
	"fmt"
	"slices"
)

var ErrPackageIncomplete = errors.New("package_incomplete: a required export role is missing")

// ExportProfile binds a load-file dialect to its page-map format and the
// representation roles that each selected document must carry.
type ExportProfile struct {
	ID            string   `json:"id"`
	DAT           string   `json:"dat"`
	PageMap       string   `json:"page_map"`
	RequiredRoles []string `json:"required_roles"`
	OptionalRoles []string `json:"optional_roles"`
}

var namedExportProfiles = []ExportProfile{
	{
		ID: "export-dat-pdf-v1", DAT: "dat-concordance-v1", PageMap: "opt-standard-v1",
		RequiredRoles: []string{"produced_pdf"},
		OptionalRoles: []string{"supplied_text", "rendition_text", "native"},
	},
	{
		ID: "export-dat-opt-images-v1", DAT: "dat-concordance-v1", PageMap: "opt-standard-v1",
		RequiredRoles: []string{"page_image"}, OptionalRoles: []string{"rendition_text", "native"},
	},
	{
		ID: "export-csv-natives-v1", DAT: "csv-rfc4180-v1",
		RequiredRoles: []string{"native"}, OptionalRoles: []string{"rendition_text"},
	},
	{
		ID: "export-dat-lfp-images-v1", DAT: "dat-concordance-v1", PageMap: "lfp-ipro-v1",
		RequiredRoles: []string{"page_image"}, OptionalRoles: []string{"supplied_text", "native"},
	},
}

// ReadExportProfile accepts only the declared export contracts.
func ReadExportProfile(id string) (ExportProfile, error) {
	for _, profile := range namedExportProfiles {
		if profile.ID == id {
			return cloneExportProfile(profile), nil
		}
	}
	return ExportProfile{}, fmt.Errorf("%w: unknown export profile %q", ErrInvalidProfile, id)
}

// ExportProfiles returns the closed table in its declared order.
func ExportProfiles() []ExportProfile {
	profiles := make([]ExportProfile, len(namedExportProfiles))
	for index, profile := range namedExportProfiles {
		profiles[index] = cloneExportProfile(profile)
	}
	return profiles
}

func cloneExportProfile(profile ExportProfile) ExportProfile {
	profile.RequiredRoles = slices.Clone(profile.RequiredRoles)
	profile.OptionalRoles = slices.Clone(profile.OptionalRoles)
	return profile
}

// CheckExportRoles validates one selected member and returns absent optional
// roles for the package manifest. Bates PDF exports require the stamped output
// in place of an unstamped produced PDF.
func CheckExportRoles(profileID string, available []string, bates bool) ([]string, error) {
	profile, err := ReadExportProfile(profileID)
	if err != nil {
		return nil, err
	}
	if bates && profileID != "export-dat-pdf-v1" {
		return nil, fmt.Errorf("%w: Bates output requires the PDF export profile", ErrInvalidProfile)
	}
	for _, role := range profile.RequiredRoles {
		if bates && role == "produced_pdf" {
			role = "stamped_pdf"
		}
		if !slices.Contains(available, role) {
			return nil, fmt.Errorf("%w: %s", ErrPackageIncomplete, role)
		}
	}
	omitted := make([]string, 0, len(profile.OptionalRoles))
	for _, role := range profile.OptionalRoles {
		if !slices.Contains(available, role) {
			omitted = append(omitted, role)
		}
	}
	return omitted, nil
}
