# Docs index rewrite: lead with human value

**Date:** 2026-07-19
**Scope:** `docs/index.md` only. The README keeps its current copy; other
docs pages are untouched.

## Problem

Feedback on the current index page: it is a very technical write-up that
does not sell to humans. The headline ("Keep the documents. Change the
filing system.") frames the product as a change to how filing works, when
the value is a filing system that is excellent for everything you need.
The hero paragraph leads with SHA-256, stable node IDs, conflict-safe
mutations, and lifecycle controls — proof points, not the pitch.

## Decisions (approved in brainstorming)

- **Scope:** rewrite the full index page, top to bottom.
- **Tone:** benefit-led plain language. Grounded but clear: every claim
  stays within what the product actually guarantees; no hype vocabulary
  (critical, comprehensive, robust, etc.). Value is stated plainly, not
  inflated.
- **Narrative:** "lifelong home" — one permanent place for the documents
  of your life, on your own machine, that you can always find, never
  quietly lose, and can prove is intact.
- **Agents:** strong second act. The hero sells the human value; a
  dedicated section further down presents the agent contract in plain
  terms and links to the agent docs. Agents keep a hero button.

## Approved hero (verbatim, minor typographic fixes allowed)

> **A permanent home for the documents of your life**
>
> Tax returns, contracts, medical records, scans of things you can't
> replace — Docbank keeps them all in one place on your own machine. File
> anything, find it in seconds, reorganize without breaking anything, and
> keep every version of every document you've ever saved. It's built so
> that decades from now, everything is still there — and you can prove it.

Note the deliberate hedge "It's built so that" — a design-goal statement,
not a warranty, consistent with alpha status.

## Page structure

1. **Hero.** Plain-language eyebrow (e.g. "EVERY DOCUMENT YOU KEEP FOR
   LIFE, ON YOUR MACHINE" — not "LOCAL-FIRST DOCUMENT SYSTEM OF RECORD").
   Approved headline and paragraph. Buttons: *Start your archive* /
   *Ten-minute tour* (quickstart) / *Build agent workflows*. "Embed in Go"
   drops to a link lower down.
2. **"What you get" grid.** Replaces the agent-contract grid. Four human
   benefits in plain words: *Find anything in seconds* · *Reorganize
   without fear* · *Every version kept* · *Backups you can actually
   trust*. Followed by the install one-liner and the existing CLI taste
   block (kept, with friendlier inline comments).
3. **"Why not just folders?"** Merges "Why docbank exists" and "Own your
   documents": folders forget, cloud drives couple your archive to an
   account, docbank keeps everything local, inspectable, and provable.
   The honest "not a sync-and-share tool" boundary stays.
4. **"Ready for your agents, too."** One tight section: anything you can
   do, an agent can do through the same authenticated API — stable IDs
   that survive renames, verified bytes, safe concurrent edits, dry runs
   before anything destructive. Links to Docbank for Agents and the
   integration guide. Deep contract detail (If-Match, OpenAPI, content
   authority) stays on those pages.
5. **The guarantees, briefly.** The four commitments survive as the short
   grid only; the duplicated long bullet list is removed. The
   audited-history "newer than v0.5.0, build from source" caveat is folded
   into one line. Technical terms (SHA-256, fsync) may appear here as
   supporting proof.
6. **Two ways to run it / Status / Where to go next.** Kept, lightly
   tightened, no factual changes. Alpha status stays honest.

## Constraints

- **Grounded claims.** These facts must survive intact wherever their
  topic appears: alpha software, current release v0.5.0; audit workflow
  newer than v0.5.0 (source build); import copies and never touches
  sources; `rm` is recoverable trash; `verify` re-proves bytes on demand;
  backups restore into a separate vault and are verified before trusted;
  docbank is not a sync-and-share tool.
- **Demote, don't delete.** SHA-256, immutable/deduplicated content,
  virtual tree in SQLite, If-Match, OpenAPI, "system of record" leave the
  hero and top sections; they remain down-page or on linked pages. No
  technical content is removed from the docs overall.
- **Front matter.** `title` and `description` are rewritten to match the
  new hero in the same plain register.
- **Existing links and anchors.** All pages currently linked from the
  index remain linked from somewhere on the page.

## Success criteria

- A non-technical reader of only the hero and "What you get" can say what
  Docbank does for them and why they'd want it.
- No claim on the page overstates what the product guarantees.
- The agent story is still prominent enough that an agent-builder landing
  on the index finds their path within one scroll.
