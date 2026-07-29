# Web screenshots

This Playwright harness captures the actual daemon-served Docbank interface
against a temporary synthetic vault. It does not use mocked API responses or a
developer's existing vault.

Install Chromium once:

```sh
cd frontend
npx playwright install chromium
```

Then run from the repository root:

```sh
make frontend-screenshots
```

The command builds the current frontend and Docbank binary, creates and seeds
an owner-private temporary vault, opens the daemon-issued browser session,
captures the requested state, stops the daemon, and removes the vault.
Generated images are written beneath `.superpowers/screenshots/` for visual
inspection and PR attachment; they are intentionally not committed.

Pass ordinary Playwright arguments after `--` to select a case:

```sh
npm --prefix frontend run screenshots -- --grep "trash confirmation"
```
