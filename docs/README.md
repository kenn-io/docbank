# docbank documentation

This directory holds the [zensical](https://zensical.org) documentation site
plus internal design material.

## Where does each page belong?

| Source | Reader question | Published route |
| --- | --- | --- |
| `website/index.html` and `website/index.md` | What is Docbank, and is it useful to me? | `/` and `/index.md` |
| `website/guide/index.html` and `website/guide.md` | How does a document move through the system? | `/guide/` and `/guide.md` |
| `docs/*.md`, `usage/`, `agents/`, and `architecture/` | How do I use, integrate, or maintain it? | `/docs/` and its child routes |
| `docs/internal/` | How is the implementation organized, and why? | Never published |
| `superpowers/specs/` (beneath this directory) | What did an earlier design propose or approve? | Never published or included in normal navigation |

Give each fact one owning guide or reference. Link to that section elsewhere.
The product page introduces the value; the guide explains the document model;
the docs index routes readers to tasks. These pages should not duplicate the
capability guide or roadmap.

Every rendered directory route has a Markdown companion. For example,
`/docs/setup/` has `/docs/setup.md`. The build copies documentation Markdown
exactly. For the hand-written website pages, update HTML and Markdown together
so they explain the same capabilities and limitations.

`zensical.toml` defines navigation and `stylesheets/` defines the docs theme.
`zensical-docs.sh` stages the explicit public file allowlist in a temporary tree.
Checks under `scripts/` validate sources and generated links, metadata, and
assets. Those scripts and configuration files are never published. Python tools
use the locked `uv` environment.

## How should I write or revise a page?

Follow the [Documentation rules in AGENTS.md](../AGENTS.md#documentation).
Start with the outcome and name the reader's next action. Explain a technical
term where the reader first needs it. Keep exact commands, fields, limits,
authorization checks, and failure behavior when simplifying prose.

Update the section that owns the fact. Use numbered steps for a sequence and
paragraphs for the reason behind a rule. Keep limitations apart from available
capabilities. Preserve design rationale, approvals, and active exceptions,
including the conditions for removing an exception. Mark superseded designs
as historical and keep them outside normal navigation.

## How do I check a change locally?

Run commands from the repository root:

1. Install the locked documentation environment once:

   ```bash
   make docs-install
   ```

2. Preview the product page, guide, and documentation together:

   ```bash
   make docs-serve
   ```

   Open `http://127.0.0.1:8000`. The server rebuilds when website, docs, or
   pinned screenshot sources change. A failed rebuild leaves the last
   successful `site/` available to inspect.

3. After the final source or asset-pin edit, run the strict build:

   ```bash
   make docs-build
   ```

   Fix every warning. Check the rendered pages, including narrow screens when
   website copy changes. Confirm that their Markdown companions give the same
   guidance and that internal files stay outside `site/`.

## Screenshots

Screenshot generation is a separate reviewed workflow. It never runs during a
documentation build or deployment.

1. Run `make docs-screenshots`. The harness starts a real daemon with a
   temporary synthetic vault and writes the complete capture set beneath
   `.superpowers/screenshots/`.
2. Inspect every generated image and its metadata.
3. Publish the complete reviewed set as one orphan `docs-assets` commit.
4. Put that exact commit in `scripts/docs-assets.ref` and run
   `make docs-assets-sync`.
5. Run `make docs-build` after the final source or asset-pin edit.

Never capture a developer vault, publish a partial set, or point the build at a
mutable branch head.

## Which source can I publish?

Published documentation describes the selected software release. Sources on
`main` are candidate documentation for the next release. Describe merged
capabilities in present tense there; do not add per-feature release-timing
notes. Before release, verify that each documented capability is in the tag.
Defer documentation for anything that will not ship.

Do not publish a candidate feature preview before its binary is tagged.
A software release makes its documentation eligible for deployment; it does
not publish it. The selected source is normally the documentation-only follow-up
after the tag and release notes exist. That follow-up adds the final changelog
entry and may correct wording, but must not advertise behavior absent from the
release. It does not require another software tag.

A maintainer must still authorize every deployment.

## How do I deploy an approved source?

Link the Vercel project once from the repository root with `make docs-link`.
Deploy an exact eligible source from a clean checkout with:

```bash
make docs-deploy DOCS_SOURCE=$(git rev-parse HEAD)
```

The command checks that the source is on `origin/main`, descends from the latest
software release, and contains only approved documentation changes. It uploads
an unpromoted production build, waits for Vercel to verify it, repeats the
release check, and only then promotes the build. Deployment does not generate
screenshots, build the product, run Docker, or install frontend dependencies.

The protected `Deploy documentation` workflow provides the same path for an
explicitly supplied source SHA. A credential-free job checks that SHA against
the release policy from `main` before the protected production job receives
the validated SHA. It has no automatic push, pull-request, tag, or release
trigger.

Pull-request documentation checks never receive Vercel credentials. The
authenticated upload dry run runs only after a trusted push to `main` and
requires the repository `VERCEL_TOKEN` secret. Production deployment requires
the `VERCEL_TOKEN`, `VERCEL_ORG_ID`, and `VERCEL_PROJECT_ID` secrets on the
protected `production` environment.

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
