---
last_edited: 2026-10-05
---

# Documentation screenshots

This Playwright harness captures the actual daemon-served Docbank interface
against temporary synthetic vaults. The documentation images use real daemon
responses. Configured embedding providers are loopback test services; they
show the interface, not the quality of a particular model's results.

Install the locked frontend dependencies and matching Chromium once:

```sh
cd frontend
npm ci
node node_modules/@playwright/test/cli.js install chromium
```

Linux hosts also need Chromium's system dependencies. Terminal captures require
`tmux`. Run the complete generation from the repository root:

```sh
make docs-screenshots
```

The command builds the frontend and Docbank binary in `bin/`, seeds owner-private
synthetic vaults, captures the real interfaces, stops its daemons, and removes
the vaults. `DOCBANK_TELEMETRY_ENABLED=0` disables telemetry for every spec.
Do not run another build while a capture is using the binary or embedded assets.

## Review and publish the complete set

The public set is listed in `scripts/docs-assets.txt`. It covers the sidebar
and mobile navigation, selection and keyboard shortcuts, tags, saved and frozen
queries, collection quality, processing and retained text, semantic search and
similar documents, email and attachments, reports, ZIP and Bates exports,
load-file imports, photo-import jobs, storage, recovery, and the TUI.

The runner gathers screenshots and proof receipts in
`.superpowers/.screenshots.capture/`. After every case succeeds, it copies only
the image manifest to a staging directory and validates that complete set
before replacing `.superpowers/screenshots/`. Missing captures leave the prior
set intact. Extra proof receipts, downloaded bundles, and viewport variants
never enter the published set.

1. Run `make docs-screenshots` and inspect **every** final image at its original
   resolution, including its filename and embedded metadata.
2. Publish the complete reviewed set as one orphan `docs-assets` commit. Its
   tree must contain exactly the PNG files in the manifest.
3. Pin that exact commit in `scripts/docs-assets.ref`.
4. Run `make docs-assets-sync` and `make docs-build` after the final edit.

Generated images do not belong on a source branch. Documentation builds consume
the immutable pin and never generate screenshots or follow a branch head.

## Work on one capture

Use a separate ignored output directory. The root Make target rejects partial
sets. For example:

```sh
mkdir -p .superpowers/collection-screenshots
DOCBANK_SCREENSHOT_DIR="$PWD/.superpowers/collection-screenshots" \
  node frontend/node_modules/@playwright/test/cli.js test \
  collections.screenshot.ts \
  --config frontend/screenshots/playwright.config.ts --project chromium
```

The query, report, Bates, export, semantic-search, similar-document, and snapshot
specs accept dedicated `DOCBANK_*_SCREENSHOT_DIR` variables. The complete runner
sets them all to its capture directory. Focused runs must set the variable used
by that spec.

Separate opt-in proofs cover Linux page rendering, retained email PDFs from a
real synthetic Linux backup, and attachment navigation through Go-owned daemon
fixtures. Their setup is defined in the corresponding spec or Go test. They are
not substitutes for the complete public image set.

The 10,000-photo grid proof uses `DOCBANK_PHOTOS_SCREENSHOT_DIR`. It creates a synthetic vault and checks paging, selection, retained previews, recovery, and bounded image URLs. It runs separately from ordinary screenshot publication.

## Report and original-export qualification

Run the combined browser workflow from the repository root:

```sh
make report-export-browser-test
```

It builds the frontend once, builds separate CGO and pure-Go Docbank binaries
with `fts5`, and executes one Chromium case per binary. It requires the usual
Go and Node build tools, a C compiler for CGO, Python 3, and the pinned Chromium
installation described above. A skipped case fails the command.

The case selects three synthetic documents and leaves a matching fourth file
out. It checks strict missing-text refusal, reviews an ambiguous date, compares
report counts and coverage with literal expectations, and independently checks
the report packet, CSV, and original ZIP. Replacement preserves the old packet
but prevents an unchanged selected request from running again.

`DOCBANK_SCREENSHOT_BINARY` must name an absolute branch binary when
`DOCBANK_REPORT_EXPORT_SCREENSHOT_DIR` enables this case. The runner sets both;
it never uses an installed binary. Each mode owns a temporary vault. After
shutdown, it verifies the saved files offline and checks that no rendition or
embedding jobs were created. Scratch data is removed; failed shutdown retains
the vault and reports its path and process.

Review captures remain under `.superpowers/report-export-browser/cgo/` and
`purego/`. They are separate from the published documentation image set. The
command prints the source commit, tree state, and executed-case result for each
mode. These results qualify Linux Chromium, not native macOS or Windows browsers.
The dedicated CI job takes effect after merge because PRs use `ci.yml` on main;
local runs provide the evidence for the PR introducing that job.
