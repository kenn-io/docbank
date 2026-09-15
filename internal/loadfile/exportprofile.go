package loadfile

import "errors"

var (
	ErrPackageIncomplete = errors.New("package_incomplete: a required export role is missing")
	ErrUnrepresentable   = errors.New("package_unrepresentable: output cannot preserve the supplied value")
)

type ExportProfile struct {
	ID            string   `json:"id"`
	DAT           string   `json:"dat"`
	PageMap       string   `json:"page_map"`
	RequiredRoles []string `json:"required_roles"`
	OptionalRoles []string `json:"optional_roles"`
}

var exportProfiles = [...]ExportProfile{
	{
		ID: "export-dat-pdf-v1", DAT: "dat-concordance-v1",
		RequiredRoles: []string{"produced_pdf"},
		OptionalRoles: []string{"supplied_text", "rendition_text", "native"},
	},
	{
		ID: "export-dat-opt-images-v1", DAT: "dat-concordance-v1", PageMap: "opt-standard-v1",
		RequiredRoles: []string{"page_image"},
		OptionalRoles: []string{"rendition_text", "native"},
	},
	{
		ID: "export-csv-natives-v1", DAT: "csv-rfc4180-v1",
		RequiredRoles: []string{"native"},
		OptionalRoles: []string{"rendition_text"},
	},
}

func ReadExportProfile(id string) (ExportProfile, error) {
	for _, profile := range exportProfiles {
		if profile.ID == id {
			return cloneExportProfile(profile), nil
		}
	}
	return ExportProfile{}, ErrInvalidProfile
}

func ExportProfiles() []ExportProfile {
	profiles := make([]ExportProfile, len(exportProfiles))
	for index, profile := range exportProfiles {
		profiles[index] = cloneExportProfile(profile)
	}
	return profiles
}

func cloneExportProfile(profile ExportProfile) ExportProfile {
	profile.RequiredRoles = append([]string(nil), profile.RequiredRoles...)
	profile.OptionalRoles = append([]string(nil), profile.OptionalRoles...)
	return profile
}
