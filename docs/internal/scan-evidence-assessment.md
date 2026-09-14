---
last_edited: 2026-09-13
---

# Scan evidence assessment

This assessment records what Docbank's existing PDF inspection APIs establish
on synthetic inputs. It does not implement scan detection, OCR, routing, or a
classification policy.

## Reproduce the measurements

Use the Go and pdfcpu versions pinned in [go.mod](../../go.mod). From the
repository root, run the corpus and measurement checks in both SQLite modes:

```sh
go test -tags fts5 ./document ./document/internal/scanfixture -run 'TestCommittedScanCorpusMatchesGenerator|TestScanEvidenceBaseline' -count=1
CGO_ENABLED=0 go test -tags fts5 ./document ./document/internal/scanfixture -run 'TestCommittedScanCorpusMatchesGenerator|TestScanEvidenceBaseline' -count=1
```

SQLite is not called by these measurements. The two runs check that both
supported build selections retain the same evidence.

The [baseline golden file](../../document/internal/scanfixture/testdata/measurements.golden.json)
contains the measured results. `Skipped` marks image and audio inputs as
`not_pdf` and the oversized input as `generated_input`. Their zero-valued fields
are placeholders, not observations about those inputs.

The baseline selects a 128 MiB source-size bound. Following
[the processing service's inspection policy](../../internal/processing/service.go),
`InspectPDF` uses that same bound for expanded bytes and individual entries,
with at most 100,000 entries. This is the low-level API assessment; it does not
run the processing service or claim that a document passes its other gates.

## Regenerate the corpus and baseline

After changing the generator or a parser dependency, run:

```sh
UPDATE_GOLDEN=1 go test -tags fts5 ./document ./document/internal/scanfixture -run 'TestCommittedScanCorpusMatchesGenerator|TestScanEvidenceBaseline' -count=1
```

This replaces the generated `document/testdata/scanassessment` directory,
including obsolete files, and updates the separate measurement golden file.
Review and commit those changes with the generator or dependency change.
Ordinary test runs compare against the committed files without rewriting them.

The encrypted PDF and lossless WebP seeds under
`document/internal/scanfixture/testdata` remain frozen. Real parsers validate
these seeds; regenerating the corpus does not replace them.

## Fixture identity

All inputs are synthetic. The [corpus manifest](../../document/testdata/scanassessment/manifest.json)
records each small file's media type, byte length, and SHA-256. It contains no
detector predictions or proposed reason codes. Git preserves the exact bytes
on every platform and treats PDF streams as binary in diffs.

The oversized two-page PDF is generated in memory and never committed. It is
134,217,729 bytes, one byte over 128 MiB. Its SHA-256 is
`743b87c1c48432b9877a2a8895b8a79307c4878f98507a011c4f939c3a8edace`.
Padding precedes the first object so readers find the trailer near the end.
`TestCorpusIsCompleteStableAndPageAccurate` checks its size and page count and
prints its digest with `go test -v`.

## What the APIs establish

- `CountPDFPages` validates the object graph and counts pages without decoding
  unrelated content streams.
- `InspectPDF` counts stream entries and encoded and decoded bytes when every
  stream is locally bounded. DCT image streams stop that measurement with
  `ErrPDFUnbounded`. Unknown filters return their decode error.
- `ReadPDFMetadata` attempts to resolve the Info dictionary and catalog XMP
  stream. This assessment retains only its top-level error. An empty
  `MetadataError` does not establish that metadata was absent, every field
  decoded, or `PDFMetadata.Issues` was empty.

The image-only and mixed inputs show the central limitation: Docbank can
count their pages, but `InspectPDF` cannot finish stream accounting.
`ErrPDFUnbounded` does not establish that a stream is an image or a document
is scanned. The undecodable-font input shows the opposite limitation: finite
stream bytes do not establish usable Unicode text.

## Limits of this assessment

These measurements do not establish searchable Unicode, decoded text quality,
visible image invocation, hidden text, page-level search coverage, authorship,
dates, or whether OCR would improve a document. `InspectPDF` totals whole
streams; it does not count PDF showing-string operands. `ReadPDFMetadata` does
not read page text.

No PyMuPDF corroboration, OCR evaluation, or fuzz campaign was run for this
assessment. It makes no classification-accuracy or parser-safety claim.
