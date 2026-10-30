package processing

import (
	"bytes"
	"fmt"
	"strconv"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
)

func recipientProfiles() (loadfile.Profile, loadfile.Profile) {
	dat, _ := loadfile.ReadProfile("dat-concordance-v1")
	dat.Columns = []string{"DOCID", "LABELSET", "BEGBATES", "ENDBATES", "PDF", "TEXT", "PAGECOUNT"}
	opt, _ := loadfile.ReadProfile("opt-standard-v1")
	return dat, opt
}

func recipientMapping(columns []string) loadfile.Mapping {
	targets := []string{"loadfile.document.id", "loadfile.label.set", "loadfile.label.begin", "loadfile.label.end", "loadfile.file.produced_pdf", "loadfile.file.supplied_text"}
	mapping := loadfile.Mapping{Contract: loadfile.MappingContractV1, Columns: make([]loadfile.MappingColumn, len(targets))}
	for i, target := range targets {
		mapping.Columns[i] = loadfile.MappingColumn{Source: columns[i], SourceOrdinal: new(i), Canonical: new(target)}
	}
	return mapping
}

func recipientValues(d RecipientPackageDocument) []string {
	return []string{d.DocumentID, d.NamespaceID, d.Labels[0], d.Labels[len(d.Labels)-1], d.PDF.Path, d.Text.Path, strconv.Itoa(d.PageCount)}
}

func recipientImages(documents []RecipientPackageDocument) []loadfile.ImageRef {
	var images []loadfile.ImageRef
	for _, d := range documents {
		for j, label := range d.Labels {
			count := 0
			if j == 0 {
				count = d.PageCount
			}
			images = append(images, loadfile.ImageRef{ImageKey: label, Volume: "VOL001", RelPath: fmt.Sprintf("PDF/%06d.pdf", d.Ordinal), DocumentBreak: j == 0, PageOrdinal: j + 1, DeclaredPageCount: count})
		}
	}
	return images
}

func recipientMetadata(documents []RecipientPackageDocument) ([][]byte, error) {
	dat, opt := recipientProfiles()
	records := make([]loadfile.Record, len(documents))
	for i, d := range documents {
		records[i] = loadfile.Record{DocID: d.DocumentID, RowOrdinal: i + 1, ColumnOrder: dat.Columns}
		for j, v := range recipientValues(d) {
			records[i].Fields = append(records[i].Fields, loadfile.Field{Column: dat.Columns[j], Ordinal: j, Raw: v})
		}
	}
	var rows, pages bytes.Buffer
	if err := loadfile.WriteDAT(&rows, records, dat); err != nil {
		return nil, err
	}
	if err := loadfile.WriteOPT(&pages, recipientImages(documents), opt); err != nil {
		return nil, err
	}
	mapping, err := canonical.Marshal(recipientMapping(dat.Columns))
	return [][]byte{rows.Bytes(), pages.Bytes(), mapping}, err
}
