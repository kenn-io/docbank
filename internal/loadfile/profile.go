package loadfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"go.kenn.io/docbank/internal/canonical"
)

var (
	ErrInvalidProfile = errors.New("invalid_package_profile: unknown or unusable load-file profile")
	ErrMalformedInput = errors.New("malformed_package_input: corrupt or out-of-bounds load-file content")
)

type Profile struct {
	ID               string   `json:"id"`
	Field            rune     `json:"field"`
	Qualifier        rune     `json:"qualifier"`
	NewlineInField   rune     `json:"newline_in_field"`
	HeaderRow        bool     `json:"header_row"`
	Columns          []string `json:"columns"`
	Encoding         string   `json:"encoding"`
	DateFormat       string   `json:"date_format"`
	DeclaredTimezone string   `json:"declared_timezone"`
}

var namedProfiles = map[string]Profile{
	"dat-concordance-v1": {
		ID: "dat-concordance-v1", Field: '\x14', Qualifier: 'þ',
		NewlineInField: '®', HeaderRow: true,
		Encoding: encodingNameUTF8,
	},
	"csv-rfc4180-v1": {
		ID: "csv-rfc4180-v1", Field: ',', Qualifier: '"', HeaderRow: true,
		Encoding: encodingNameUTF8,
	},
	"opt-standard-v1": {
		ID: "opt-standard-v1", Field: ',',
		Columns:  []string{"ImageKey", "VolumeName", "ImagePath", "DocumentBreak", "FolderBreak", "BoxBreak", "PageCount"},
		Encoding: encodingNameUTF8,
	},
	"opt-pagecount5-v1": {
		ID: "opt-pagecount5-v1", Field: ',',
		Columns:  []string{"ImageKey", "VolumeName", "ImagePath", "DocumentBreak", "PageCount", "FolderBreak", "BoxBreak"},
		Encoding: encodingNameUTF8,
	},
	"lfp-ipro-v1": {
		ID: "lfp-ipro-v1", Field: ',', Encoding: encodingNameUTF8,
	},
}

func ReadProfile(id string) (Profile, error) {
	profile, ok := namedProfiles[id]
	if !ok {
		return Profile{}, ErrInvalidProfile
	}
	profile.Columns = append([]string(nil), profile.Columns...)
	return profile, nil
}

func (p Profile) SHA256() (string, error) {
	encoded, err := canonical.Marshal(p)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
