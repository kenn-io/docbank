# Web screenshots

This Playwright harness captures the actual daemon-served Docbank interface
against a temporary synthetic vault. It does not use mocked API responses or a
developer's existing vault.

Install Chromium once:

```sh
cd frontend
npm ci
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
both move-to-trash and
restore confirmations, the tag-definition catalog, and a completed tag
assignment, current vault browsing, extracted-text search, retained-version
selection, packed-storage status, and independently verified permanent-audit
evidence. Generated images are intentionally not committed to the main branch.

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
