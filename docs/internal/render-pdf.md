# Render documents to PDF

`document/renderpdf` converts byte-verified DOCX sources to bounded
PDFs through an operator-pinned LibreOffice installation. It owns conversion
provenance and grants no upload authority.

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

Private-root-v1 discovers the declared runtime during operator setup. Each
regular runtime file is opened, hashed, copied to a sealed memfd, and attached
at its declared guest path. Special files and runtime identity mismatches fail
before launch. The old root is detached with `pivot_root` before the
renderer can create an AF_UNIX socket. Supervised private-root mode permits
AF_UNIX as a fixed launcher policy. IP and netlink socket creation remains
denied.

The root contains only generated configuration, declared runtime files, a
bounded `/work` tmpfs, private `/tmp`, and proc and device entries.
Runtime data mounts are noexec. ELF executables and shared objects, including
mode-0644 `libmergedlo.so`, keep executable mappings. The private
profile disables macros and active content and prevents link updates.

The launcher verifies the sealed executable and bounds runtime descriptors and
bytes. Before caller bytes enter `/work`, each stage converts a fixed,
digest-pinned trusted fixture with the exact stage arguments. A trusted
fixture conversion may retry exit 81 once in the same profile. The launcher
then removes every non-profile warm-up artifact, verifies the security
settings, writes the caller input, and runs that conversion once. Caller
failures publish no bytes. Non-Linux callers need an injected audited runner.

## Receipt and source

`Receipt` binds detected format, source extension, original size and
SHA-256, normalized FODT size and SHA-256, PDF size and SHA-256, page count,
policy fingerprint, converter version, runtime identity, and runner identity.
`Result.PDF` and `Result.Receipt` return copies.
`Result.Source` returns a fresh `application/pdf` source over
the exact counted bytes.

The receipt records provenance. It grants no upload permission. A later caller
must perform its own capability, consent, and upload authorization checks.
