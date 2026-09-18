# Render documents to PDF

`document/renderpdf` converts byte-verified DOCX sources to bounded
PDFs through an operator-pinned LibreOffice installation. It owns conversion
provenance and grants no upload authority.

See [renderer setup](../document-understanding.md#set-up-the-renderer) for
Linux and AppArmor requirements, installation, executable pinning, runtime
discovery, and a complete policy example. The
[conversion guide](../document-understanding.md#convert-a-source) describes
format restrictions and the XML token limit, including embedded image data.

## Conversion stages

The converter reads the source once, checks its declared size and SHA-256, and
detects its real format. The initial profile accepts DOCX only and normalizes
it to flat ODF text.

LibreOffice imports the original inside a Linux private-root sandbox and writes
a bounded FODT file. The converter scans those exact bytes with
`encoding/xml`. The scan rejects external and relative links, DDE and
database sources, scripts, event handlers, linked sections, external formulas,
and opaque active objects such as OLE, plugins, applets, and nested documents.
It also rejects every `xml:base` attribute, so internal fragment links cannot
inherit an external base URI. Internal fragment links and embedded raster image
data remain accepted when no XML Base is present.

Only admitted FODT reaches the second private-root LibreOffice process. That
process writes the PDF. The converter verifies each stage attestation, binds
source, normalized, and PDF identities in the receipt, bounds the bytes, counts
pages with `media.CountPDFPages`, and publishes no result after a
failure.

## Isolation

`document/internal/providerutil/sandbox` owns two separate Linux policies.
Trafilatura uses strict exec mode with its baseline narrow Landlock paths and
complete socket deny list. Render stages use private-root-v1.

Private-root-v1 discovers the declared runtime during operator setup. The
parent creates a unique temporary root and removes it after the launcher
exits. Inside its mount namespace, the launcher mounts tmpfs at that root,
copies each regular runtime file once to its declared guest path, and checks
the copied bytes against the manifest. Special files and runtime identity
mismatches fail before the renderer starts. The old root is detached with
`pivot_root` before the renderer can create an AF_UNIX socket. LibreOffice
mode permits AF_UNIX as a fixed launcher policy. All other socket families
are denied.

The root contains only generated configuration, declared runtime files, a
bounded `/work` tmpfs, private `/tmp`, and proc and device entries.
Runtime data mounts are noexec. ELF executables and shared objects, including
mode-0644 `libmergedlo.so`, keep executable mappings. The private
profile disables macros and active content and prevents link updates.

The launcher verifies the sealed executable and bounds runtime entries and
bytes. Before caller bytes enter `/work`, each stage converts a fixed,
digest-pinned trusted fixture with the exact stage arguments. A trusted
fixture conversion may retry exit 81 once in the same profile. The launcher
then removes every non-profile warm-up artifact, restores and verifies the
exact fixed security settings, writes the caller input, and runs that conversion
once. Caller failures publish no bytes. Non-Linux callers need an injected
audited runner.

## Receipt and source

`Receipt` binds detected format, source extension, original size and
SHA-256, normalized FODT size and SHA-256, PDF size and SHA-256, page count,
policy fingerprint, converter version, runtime identity, and runner identity.
`Result.PDF` and `Result.Receipt` return copies.
`Result.Source` returns a fresh `application/pdf` source over
the exact counted bytes.

The receipt records provenance. It grants no upload permission. A later caller
must perform its own capability, consent, and upload authorization checks.

The Mistral adapter is one such caller. When its `PolicyConfig.RenderPDF` is
configured, a DOCX authorization uses the manifest's PDF request authority,
then `renderpdf.Convert` produces one counted PDF before provider egress.
Mistral checks the generated PDF against its own byte and page limits, keeps
that PDF for retries, and returns the original DOCX family and source hash
alongside the generated PDF hash. The render policy fingerprint becomes part
of the Mistral policy identity. A native DOCX capability-probe observation
still records `UnitBoundNone` and does not authorize this route.
