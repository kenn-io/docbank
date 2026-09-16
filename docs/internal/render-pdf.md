# Render documents to PDF

`document/renderpdf` converts byte-verified DOCX and XLSX sources to PDFs with
an operator-pinned LibreOffice installation. The package owns conversion
provenance. It does not open a vault, contact a daemon, or authorize upload.

## Conversion stages

The converter reads the source once, checks its declared size and SHA-256, and
detects its real format. It accepts DOCX and XLSX profiles only. Each profile
names the flat ODF format LibreOffice must produce.

LibreOffice first imports the original inside the Linux sandbox and writes a
bounded flat ODF file. The converter scans that exact file with `encoding/xml`.
The scan rejects external and relative links, DDE and database sources,
scripts, event handlers, linked sections, external formulas, and opaque active
objects such as OLE, plugins, applets, and nested documents. It permits
internal fragment links, local formulas, and embedded raster image data.

Only admitted flat ODF bytes reach the second sandboxed LibreOffice process.
That process writes the PDF. The converter verifies its stage attestation,
bounds the bytes, counts pages with `media.CountPDFPages`, and returns no
result after any failure.

## Isolation

`document/internal/providerutil/sandbox` owns the Linux enforcement used by
Trafilatura and this package. It launches a sealed, SHA-256-verified
executable, uses user, network, PID, and mount namespaces, remounts inherited
host files read-only, and provides bounded private temporary storage. Landlock
limits runtime reads. Seccomp limits network operations. The PID namespace
owner reaps descendants when the context is canceled.

Both LibreOffice stages use the same executable digest, fixed arguments,
private profile, clean environment, and read-only runtime roots. The private
profile disables macros and active content. A fresh profile is created for each
stage. The sandbox's process tree and file transport are independent of the
scanner's admission decision.

Native enforcement is available on Linux when the required kernel controls
exist. Other platforms need an injected, audited runner. An injected runner is
trusted deployment code. The package checks its identity and each stage's
attestation, but a dishonest runner can still report false controls.

## Receipt and source

`Receipt` binds the detected source format, source extension, original size and
SHA-256, normalized ODF size and SHA-256, PDF size and SHA-256, page count,
policy fingerprint, converter version, runtime identity, and runner identity.
`Result.PDF` and `Result.Receipt` return copies. `Result.Source` returns a
fresh `application/pdf` `ocr.Source` over the exact counted bytes.

The receipt records provenance. It grants no upload permission. A later caller
must perform its own capability check, consent check, and upload authorization
for the returned PDF.
