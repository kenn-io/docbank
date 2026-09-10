# CSV to PDF conversion

`document/csvpdf.Convert` turns a verified `text/csv` source into independently
identified PDF bytes. It has no dependency on vault storage or a provider
client. Applications can pass `Result.Source` to the existing PDF OCR path.

The conversion receipt records how the PDF came from the CSV. It does not
authorize upload. Applications retain it separately from provider evidence and
upload consent.

## What authorizes conversion?

An immutable `Policy` supplies six positive limits, each bounded by a fixed
hard ceiling. A zero policy cannot authorize conversion.

The policy's canonical JSON fingerprint includes:

- the six limits;
- the converter version;
- the fixed layout identity; and
- the embedded font hash.

Changes to parsing, layout, the font, or the pinned PDF writer require a new
converter version.

## How does the converter read and validate CSV?

1. Read at most the declared source length plus one byte, bounded by the source
   ceiling.
2. Recompute SHA-256 and verify the source before parsing.
3. Parse CSV while enforcing record-count, cell-count, and per-cell byte limits.
4. Close the input. A close failure discards a would-be result.

Go's CSV reader handles quoting, escaped quotes, blank lines, and CRLF
normalization. The converter accepts variable-width records. The source byte
limit bounds parser allocations before the finer record and cell limits can
be checked.

The converter closes its input on every path. Cancellation stops work between
reads and during parsing and layout. The caller remains responsible for an
arbitrary reader that blocks inside a read.

## What does the generated page contain?

The layout uses embedded Go Mono at 10 points on A4 pages. Each content line
holds 80 characters, and each page holds 48 lines. A fixed heading identifies
generated pages.

Each cell has a record/cell label followed by its value. Empty values and
explicit empty lines remain visible. Labels count toward the line limit.
Page spans record every page that contains a cell's label or value, including
continuation pages.

The renderer accepts only font-supported basic multilingual plane characters
in the Latin, Greek, Cyrillic, or Common scripts. It rejects marks, formatting
characters, and control characters except newlines. This prevents silent glyph
replacement or incorrect shaping.

## How is the PDF bounded and checked?

The pinned fpdf writer uses a private embedded-font copy, explicit compression,
fixed timestamps, sorted catalogs, and disabled automatic page breaks.

1. Layout checks the page limit before creating the PDF.
2. The output writer enforces the PDF byte limit.
3. `media.CountPDFPages` independently checks the generated object graph and
   compares its page count with the planned count.

The PDF library builds an internal buffer before writing. Source and page hard
ceilings bound that work; the PDF byte limit bounds retained output.
Applications must also limit simultaneous conversions to control process memory.

## What does a successful result prove?

Only a complete, counted PDF yields a `Result`. Its receipt binds the original
and generated SHA-256 hashes, both byte counts, generated page count, policy
fingerprint, converter version, and page/record/cell spans.

`PDF`, `Receipt`, and `Source` return independent copies. Callers cannot mutate
the authoritative bytes or mapping through those results. The PDF page count
does not imply an original CSV record count.

The receipt supplies provenance. The existing PDF manifest and the application's
consent supply upload authority.
