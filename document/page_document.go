package document

import (
	"errors"

	"go.kenn.io/docbank/internal/canonical"
)

// PageDocumentV1 is a complete physical inventory, independent of how many
// page images have been requested or successfully published.
type PageDocumentV1 struct {
	Contract  string        `json:"contract"`
	Source    PageSource    `json:"source"`
	PageCount int           `json:"page_count"`
	Frames    []PageFrameV1 `json:"frames"`
}

func ValidatePageDocumentV1(d PageDocumentV1) error {
	if err := d.Source.Validate(); err != nil {
		return err
	}
	if d.Contract != PageFrameContractV1 || d.PageCount < 1 || d.PageCount > MaxDocumentPages || len(d.Frames) != d.PageCount {
		return errors.New("incomplete physical page inventory")
	}
	for index, frame := range d.Frames {
		if err := ValidatePageFrameV1(frame); err != nil {
			return err
		}
		if frame.Source != d.Source || frame.Page != index+1 || frame.InputUnits != d.Frames[0].InputUnits || frame.InputUnits == "pixel" && d.PageCount != 1 {
			return errors.New("physical page inventory source or sequence mismatch")
		}
	}
	return nil
}
func MarshalPageDocumentV1(d PageDocumentV1) ([]byte, string, error) {
	if err := ValidatePageDocumentV1(d); err != nil {
		return nil, "", err
	}
	b, err := canonical.Marshal(d)
	return b, sha256Hex(b), err
}
func DecodePageDocumentV1(b []byte) (PageDocumentV1, string, error) {
	d, err := canonical.Decode[PageDocumentV1](b)
	if err != nil {
		return d, "", err
	}
	_, hash, err := MarshalPageDocumentV1(d)
	return d, hash, err
}
