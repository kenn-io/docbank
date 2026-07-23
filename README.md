# Docbank documentation assets

Binary documentation assets live on this orphan branch so the main source
history stays small.

## v0.11.0 web application

The screenshots in `screenshots/v0.11.0/` were captured on 2026-07-23 from
Docbank v0.11.0 (`2203108`) with Playwright 1.58.2 at 1440 × 900 in dark mode.
The frontend and pure-Go backend were built and run in Docker. The vault,
document names, paths, contents, hashes, and identifiers are synthetic.

- `web-vault-browser.png` shows the root tree and the selected document's
  stable authority.
- `web-search-results.png` shows verified extracted-text search results across
  the synthetic vault.

## Pull request screenshots

`screenshots/pr-136/web-background-jobs.png` was captured on 2026-07-23 from
Docbank commit `64f72c3` at 1600 × 1000 in dark mode. It shows the actual web
application backed by a temporary synthetic vault with extraction, automatic
packing, and watched-inbox jobs running. The vault, document names, paths,
contents, hashes, and identifiers are synthetic.
