---
last_edited: 2026-09-11
---

# Web screenshots

This Playwright harness captures the actual daemon-served Docbank interface
against a temporary synthetic vault. It does not use mocked API responses or a
developer's existing vault.

Install Chromium once:

```sh
cd frontend
npm ci
node node_modules/@playwright/test/cli.js install-deps chromium
node node_modules/@playwright/test/cli.js install chromium
```

Then run from the repository root:

```sh
make docs-screenshots
```

The command builds the current frontend and Docbank binary, creates and seeds
an owner-private temporary vault, opens the daemon-issued browser session,
captures the requested state, stops the daemon, and removes the vault.
Generated images are atomically published beneath `.superpowers/screenshots/`
for visual inspection and orphan-branch publication; the current set captures
desktop and mobile page selection, move-to-trash and restore confirmations,
the tag-definition catalog, and a completed tag assignment, current vault browsing, extracted-text search, retained-version
selection, packed-storage status, and independently verified permanent-audit
evidence. The processing plan, partial semantic coverage, and sanitized Markdown
rendition are captured separately under `.superpowers/processing-screenshots/`
for pull request review. Generated images are intentionally not committed to the main branch.
The Playwright config sets `DOCBANK_TELEMETRY_ENABLED=0` for every spec, so harness daemons send no usage telemetry.

The command must produce the complete set listed in `scripts/docs-assets.txt`.
Documentation builds consume a reviewed set and never run this harness.

For focused harness development, invoke Playwright directly with a separate
temporary `DOCBANK_SCREENSHOT_DIR`; the root Make target intentionally rejects
partial generations:

```sh
cd frontend
DOCBANK_SCREENSHOT_DIR="$(mktemp -d)" node node_modules/@playwright/test/cli.js test \
  --config screenshots/playwright.config.ts --project chromium \
  --grep "trash confirmation"
```

The collection case runs separately until both images are included in a complete
published, pinned `docs-assets` set. `make docs-screenshots` excludes it until
then. It imports two synthetic documents and exercises label rename, a concurrent
label conflict, clearing, and document navigation:

```sh
make build
DOCBANK_SCREENSHOT_DIR="$PWD/.superpowers/collection-screenshots" \
  node frontend/node_modules/@playwright/test/cli.js test \
  --config frontend/screenshots/playwright.config.ts --project chromium \
  --grep "import collections"
```

It captures the member browser and the preserved draft after a label conflict
as `web-collections.png` and `web-collection-label-conflict.png`.

The PR-only snapshot case seeds 1,001 synthetic documents and exercises the
real daemon, browser IndexedDB, recovery checkpoint import, exact retry, and
stale revision fence. Its dedicated output stays outside the strict
documentation screenshot set:

```sh
make build
DOCBANK_SNAPSHOT_SCREENSHOT_DIR="$PWD/.superpowers/snapshot-integrated-final" \
  node frontend/node_modules/@playwright/test/cli.js test \
  snapshot-workspace.screenshot.ts \
  --config frontend/screenshots/playwright.config.ts --project chromium
```

The PR-only similar-documents case processes four synthetic files through a
loopback embedding stub, verifies that similarity sends no provider request,
and captures widths 1440, 1280, 768, and 400. The same run captures the actual
TUI through tmux, using WSL Ubuntu on Windows, or records the terminal
availability blocker. It accepts `DOCBANK_SCREENSHOT_BINARY` and its cleanup
stops the daemon and removes the temporary vault:

```sh
make build
DOCBANK_SIMILAR_SCREENSHOT_DIR="$PWD/.superpowers/similar-screenshots" \
  node frontend/node_modules/@playwright/test/cli.js test \
  similar-documents.screenshot.ts \
  --config frontend/screenshots/playwright.config.ts --project chromium
```

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
