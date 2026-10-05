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

The command builds the frontend and Docbank binary, seeds owner-private
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
