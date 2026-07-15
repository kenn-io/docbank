# docbank documentation

This directory holds the [zensical](https://zensical.org) documentation site
plus internal design material.

## Layout

- `*.md`, `usage/`, `architecture/` — published site content
- `stylesheets/` — published visual theme
- `scripts/` — source and built-site validation (never published)
- `internal/` — living agent/developer design documentation (never published)
- `zensical.toml` — site configuration
- `zensical-docs.sh` — build wrapper; validates sources, copies publishable
  content into a temporary tree, and checks the generated site's links,
  metadata, assets, and publishing boundary; all Python tools run through
  the locked `uv` environment

Every rendered directory route also publishes its exact Markdown source at
the sibling `.md` path: `/setup/` has `/setup.md`, and
`/usage/importing/` has `/usage/importing.md`. This gives agents and other
text-first clients a stable representation without scraping rendered HTML.
Section landing pages use a sibling source such as `usage.md`, not
`usage/index.md`, so relative links keep the same base at `/usage.md`.

## Building

```bash
cd docs
uv sync --frozen          # one-time: installs zensical into docs/.venv
./zensical-docs.sh serve  # live-reload preview
./zensical-docs.sh build  # strict production build into docs/site/
```

Or from the repository root: `make docs-install`, `make docs-serve`,
`make docs-build`.

## Documentation boundary

- User- and agent-facing pages explain shipped capabilities, exact contracts,
  and current limitations. They do not inventory future commands or endpoints.
- Public Architecture pages explain product behavior and durable boundaries.
  A future contract belongs there only when it materially explains design
  intent, and is always isolated under an explicit `!!! info "Planned"`
  admonition.
- `internal/` is the definitive developer description of how the system works
  and why. Update it in place with implementation changes; revise the matching
  public Architecture page when user-visible behavior or boundaries change.
- `roadmap.md` is the one high-level public view of product direction and
  status. It is not an execution ledger.
- Kata is the sole source of truth for actionable work, sequencing, ownership,
  blockers, and completion state. Do not copy that state into documentation.
