// Package redactiontest provides synthetic contract fixtures for redaction tests.
package redactiontest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

func PDF(tb testing.TB, lines []string, subject string) []byte {
	tb.Helper()
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCompression(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetSubject(subject, true)
	pdf.SetAutoPageBreak(false, 0)
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	for index, line := range lines {
		pdf.Text(72, 72+float64(index)*16, line)
	}
	var output bytes.Buffer
	require.NoError(tb, pdf.Output(&output))
	return bytes.Clone(output.Bytes())
}

func Map(text string) redaction.TextMap {
	for index := range len(text) {
		if text[index] >= utf8RuneSelf {
			panic("redactiontest.Map accepts ASCII text only")
		}
	}
	frameSHA256 := syntheticSHA256("redactiontest page frame v1")
	width := int64(max(1, len(text)) * 1000)
	value := redaction.TextMap{
		Contract:       "aligned-text/v1",
		PDFSHA256:      syntheticSHA256("redactiontest PDF identity v1"),
		EvidenceSHA256: syntheticSHA256("redactiontest evidence identity v1"),
		Text:           text,
		Pages: []redaction.Page{{
			Number: 1, FrameSHA256: frameSHA256, Width: width, Height: 10_000,
			Span: redaction.Span{Start: 0, End: int64(len(text))},
		}},
		Atoms: make([]redaction.Atom, 0, len(text)),
		Units: []redaction.Unit{},
		Gaps:  []redaction.Gap{},
	}
	for index := range len(text) {
		value.Atoms = append(value.Atoms, redaction.Atom{
			Span: redaction.Span{Start: int64(index), End: int64(index + 1)},
			Boxes: []redaction.Box{{
				Page: 1, FrameSHA256: frameSHA256, X0: int64(index * 1000), Y0: 0,
				X1: int64(index*1000 + 100), Y1: 100,
			}},
		})
	}
	value.SHA256 = mapSHA256(value)
	return value
}

const utf8RuneSelf = 0x80

func mapSHA256(value redaction.TextMap) string {
	full, err := canonical.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("serialize synthetic redaction map: %v", err))
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(full, &members); err != nil {
		panic(fmt.Sprintf("decode synthetic redaction map: %v", err))
	}
	if _, present := members["sha256"]; !present {
		panic("synthetic redaction map has no sha256 member")
	}
	delete(members, "sha256")
	encoded, err := canonical.Marshal(members)
	if err != nil {
		panic(fmt.Sprintf("canonicalize synthetic redaction map: %v", err))
	}
	return sha256Bytes(encoded)
}

func syntheticSHA256(value string) string { return sha256Bytes([]byte(value)) }

func sha256Bytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
