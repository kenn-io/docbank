# DOCX to PDF conversion

`document/docxpdf` converts a verified DOCX source to a PDF through an
operator configured LibreOffice executable. The converter counts the PDF and
returns those same bytes, so an application can apply one page limit before it
sends the document to Mistral.

## Configure the renderer

Create a `Renderer` with an absolute, clean executable path, its lowercase
SHA-256 digest, and a short printable ASCII runtime identity. The identity
should name the LibreOffice build and the font installation that determine
layout. `NewPolicy` verifies the executable immediately and includes every
renderer value, conversion argument, and limit in the policy fingerprint.

The renderer is optional. Importing this package and calling `NewPolicy` never
starts LibreOffice. A policy with the zero value or invalid limits cannot run a
conversion.

`DefaultLimits` allows a 50 MiB DOCX, a 50 MiB generated PDF, 1,000 pages, and
a ten minute renderer timeout. Applications may tighten each value. The byte
limits bound retained input and output. They do not cap LibreOffice's native
memory or temporary disk use while it renders. Limit concurrent conversions
when the host needs a stricter resource budget.

## Verify the source and conversion

`Convert` takes ownership of `ocr.Source.Content` and closes it once on every
path. It checks the caller's metadata, reads only the declared source length
plus one byte, verifies the SHA-256, and detects a genuine OOXML DOCX package.
The caller's media type is a hint. A ZIP with `word/document.xml` but no Word
main content type is rejected.

The converter writes the source to a private temporary directory with mode
0600. It gives LibreOffice a separate profile, home directory, and temporary
directory for each call. The child gets only fixed arguments and a
credential-free environment. On Windows, `providerutil.ManagedCommand` owns a
job object so cancellation ends the renderer's descendants. On Linux and
macOS, the configured executable must be the non-daemonizing renderer child,
such as `soffice.bin`, because the process owner can terminate only that exact
child. The renderer still runs with the calling user's operating system
permissions. This package does not provide a network or filesystem sandbox.

After LibreOffice exits, the converter requires a regular output file at the
fixed path, reads at most `MaxPDFBytes`, validates the PDF object graph, and
counts its pages. It removes the work directory on every path. A cleanup
failure discards a successful result. A renderer can still consume temporary
disk before its timeout; use a sandbox or filesystem quota when that limit
matters.

On Unix, a fresh LibreOffice profile may exit with its documented normal
restart status 81 while it initializes. The converter starts the same pinned
child once more with the same profile and overall deadline. A second status 81
or any other nonzero exit fails the conversion.

## Keep both identities

`Result.Receipt` records the original DOCX SHA-256 and byte count, the
generated PDF SHA-256 and byte count, the counted page total, the policy
fingerprint, and `ConverterVersion`. `PDF`, `Receipt`, and `Source` return
copies. The receipt records provenance. It does not authorize an upload.

The application owns consent and provider policy. A typical flow is:

```go
policy, err := docxpdf.NewPolicy(renderer, docxpdf.DefaultLimits())
if err != nil {
    return err
}
converted, err := docxpdf.Convert(ctx, source, policy)
if err != nil {
    return err
}
if converted.Receipt().Pages > mistralPolicy.Values().MaxUnits {
    return errors.New("DOCX PDF exceeds the Mistral page limit")
}
pdfSource, err := converted.Source()
if err != nil {
    return err
}
// Pass pdfSource to the existing PDF Prepare, Authorize, and Process flow.
```

The existing Mistral PDF policy remains the upload authority. The application
must prepare and authorize `pdfSource` with its generated SHA-256 and byte
count. `document/mistral` does not gain DOCX authorization or a DOCX unit
counter. This package is a staged library for callers that choose to adopt
the generated PDF. The original DOCX processing authority remains unchanged.

## Layout and trust

LibreOffice decides how Word fields, hyperlinks, fonts, and page breaks render.
Trusted ordinary DOCX files are accepted, including automatic fields and
external links. The operator controls the installation, fonts, permissions,
and confinement. The runtime identity belongs in the policy so a change to
that installation creates a different conversion identity, but it does not
claim that two installations produce bit identical PDFs.
