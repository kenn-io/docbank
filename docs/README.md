# docbank documentation

This directory holds the [zensical](https://zensical.org) documentation site
plus internal design material.

## Layout

- `*.md`, `usage/`, `architecture/` — published site content
- `superpowers/` — transient working specs and implementation plans for
  in-flight development (never published; exists only while a project
  is being executed)
- `zensical.toml` — site configuration
- `zensical-docs.sh` — build wrapper; copies publishable content into a
  temporary tree so internal material can't leak into the site

## Building

```bash
cd docs
uv sync --frozen          # one-time: installs zensical into docs/.venv
./zensical-docs.sh serve  # live-reload preview
./zensical-docs.sh build  # strict production build into docs/site/
```

Or from the repository root: `make docs-install`, `make docs-serve`,
`make docs-build`.

## Conventions

- User-facing pages document only what the current binary does. Planned
  behavior lives in the Design section and is labeled with a
  `!!! info "Planned — Phase N"` admonition. Do not document flags or
  endpoints that do not exist yet outside those admonitions.
- The Design pages are the digested, maintained form of the superpowers
  specs. When a design decision changes, update the Design page in the
  same PR. Once a project ships and its design content is digested into
  the site, delete its spec and plan — git history keeps the
  point-in-time record; the working tree carries only maintained docs.
