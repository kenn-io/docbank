package pdfproduction

import (
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

// PagePresentation supplies independent output expectations. A nil presentation
// means the source-sized, unnumbered layout.
type PagePresentation struct {
	Layout       redaction.PageLayout    `json:"layout"`
	Endorsements []redaction.Endorsement `json:"endorsements"`
}

type NumberedFooter struct {
	StripHeight         int64  `json:"strip_height"`
	InsetX              int64  `json:"inset_x"`
	InsetTop            int64  `json:"inset_top"`
	InsetBottom         int64  `json:"inset_bottom"`
	FontSHA256          string `json:"font_sha256"`
	FontSizeMilliPoints int64  `json:"font_size_milli_points"`
}

func QualifiedNumberedFooter(recipe redaction.Recipe) NumberedFooter {
	return NumberedFooter{5000, 2000, 1000, 1000, recipe.FontSHA256, 10000}
}

// NumberedPresentation derives recorded output expectations without loading a
// local font or rasterizer. Admission must separately preflight the presentation.
func NumberedPresentation(pages []redaction.Page, labels []string, footer NumberedFooter) ([]PagePresentation, error) {
	if len(pages) == 0 || len(pages) != len(labels) || len(pages) > 16 {
		return nil, endorsementConflict()
	}
	result := make([]PagePresentation, len(pages))
	for i, source := range pages {
		data, err := canonical.Marshal(struct {
			Contract    string         `json:"contract"`
			Source      redaction.Page `json:"source"`
			StripHeight int64          `json:"strip_height"`
		}{"native-pdf-numbered-frame/v1", source, footer.StripHeight})
		if err != nil {
			return nil, err
		}
		output := source
		output.Height += footer.StripHeight
		output.FrameSHA256 = hashBytes(data)
		p := PagePresentation{Layout: redaction.PageLayout{Source: source, Output: output, StripHeight: footer.StripHeight},
			Endorsements: []redaction.Endorsement{{Kind: "number", Text: labels[i],
				Color: "#000000", FontSHA256: footer.FontSHA256, FontSizeMilliPoints: footer.FontSizeMilliPoints,
				Box: redaction.Box{Page: source.Number, FrameSHA256: output.FrameSHA256,
					X0: footer.InsetX, X1: source.Width - footer.InsetX, Y0: source.Height + footer.InsetTop, Y1: output.Height - footer.InsetBottom}}}}
		result[i] = p
	}
	return result, nil
}

func presentationAt(page redaction.Page, index int, presentation []PagePresentation) (PagePresentation, error) {
	if presentation == nil {
		return PagePresentation{Layout: redaction.PageLayout{Source: page, Output: page},
			Endorsements: []redaction.Endorsement{}}, nil
	}
	if index >= len(presentation) || presentation[index].Layout.Source != page {
		return PagePresentation{}, pdfIntegrity("presentation source differs from review")
	}
	return presentation[index], nil
}

// PreflightPresentation checks the current font's label fit before retention.
func PreflightPresentation(presentation []PagePresentation, recipe redaction.Recipe) error {
	for _, p := range presentation {
		if err := paintEndorsements(nil, p.Layout, p.Endorsements, recipe); err != nil {
			return err
		}
	}
	return nil
}
