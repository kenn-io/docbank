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

`screenshots/web-version-history/web-version-history.png` was captured on
2026-07-25 from Docbank commit `56432ef` at 1440 × 950 in dark mode. It shows
the actual daemon-served web application backed by a temporary synthetic
vault whose report was created, replaced, and reverted. The vault, document
name, path, contents, hashes, and identifiers are synthetic.

`screenshots/web-provenance/web-provenance.png` was captured on 2026-07-26
from Docbank commit `b646d56` at 1440 × 950 in dark mode. It shows the actual
daemon-served web application backed by a temporary synthetic vault containing
one immutable origin fact. The vault, document name, path, contents, hashes,
identifiers, source kind, and source reference are synthetic.

`screenshots/web-verified-download/web-verified-download.png` was captured on
2026-07-26 from Docbank commit `6896b86` at 1440 × 950 in dark mode. It shows
the actual daemon-served web application after a selected synthetic report was
terminally verified and handed to the browser's native download. The vault,
document name, path, contents, hash, and identifiers are synthetic.

`screenshots/web-tags/web-tags.png` was captured on 2026-07-27 from Docbank
commit `3c5ae56` at 1440 × 950 in dark mode. It shows the actual daemon-served
web application after a two-result text search was narrowed to the one
synthetic report carrying the selected stable tag. The vault, document names,
paths, contents, hashes, tags, and identifiers are synthetic.

`screenshots/web-retained-version-download/web-retained-version-download.png`
was captured on 2026-07-27 from Docbank commit `1dd146c` at 1440 × 950 in dark
mode. It shows the actual daemon-served web application with an older retained
version selected and ready for verified download. The vault, document name,
path, contents, hashes, and identifiers are synthetic.

`screenshots/web-storage-status/web-storage-status.png` was captured on
2026-07-27 from Docbank commit `c398337` at 1440 × 950 in dark mode. It shows
the actual daemon-served web application reporting loose content, live packed
content, and logically dead packed payload awaiting repack. The vault,
document names, paths, contents, hashes, identifiers, and storage history are
synthetic.
