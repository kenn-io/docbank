---
last_edited: 2026-10-31
title: Document processing
description: Preview, consent to, and run document processing. The original version is never changed.
---

# Document processing

Document processing derives searchable renditions, such as Markdown or a
transcript, from one exact document version. When a profile has embedding
bindings, processing also derives vector sets from that version. Your original
version stays unchanged and authoritative. A processing profile is a named,
non-secret policy that sets the provider descriptors, trust boundaries,
retention choices, and retrieval limits for that work.

To see the profiles the daemon can run, list them:

```bash
docbank processing profiles
```

## Set up local text search

To turn on local text search from the CLI, run:

```bash
docbank processing local --server local
```

The command previews the profile, model, server, and provider disclosures, then
enables indexing without a prompt. Running the command grants approval. Setup
keeps an approved configured recipe or picks the first indexed compatible recipe
in name order, then moves it to the shared server with its extractor, chunks,
comparison metric, normalization setting, and existing vectors. Only new or
changed content needs embedding.

If another model, text format, or formatted input limit above 2,046 rune tokens
requires a separate built-in index, the preview tells you before downloading or
starting work, and your original index stays intact. Vaults without compatible
indexed recipes use the built-in `local-text` profile with Nomic and the
plaintext extractor. When no configured recipe moves, an existing saved built-in
index continues adding missing content, while a moved recipe replaces that
built-in selection for new imports. Mixed recipes keep image and other bindings
on their configured providers. Setup moves one profile; other configured
profiles still need their providers. Restarting the daemon restores the saved
profile, including existing watches.

To use an existing Kit embedding server, supply its URL, model, and vector
width together:

```bash
docbank processing local --embedding-server https://embedding.example.com/api/v1/servers/example --embedding-model gemini-embedding-2 --embedding-dimensions 3072
```

The preview lists each affected profile and the providers that stay. Setup
rebuilds the affected indexes, saves the choice, and restarts the daemon.
New imports use the same selection. The URL must accept requests with `/embed` appended.
Requests are anonymous by default. For a protected server, add `--embedding-api-key-env NAME` and make that variable available to the daemon. Docbank saves the variable name, not its value.
Use `--server local` to switch back to the local server.

Voyage contextual text stays with Voyage. For models whose name starts with
`gemini-embedding-2`, supported Voyage media moves to the server: PNG, JPEG,
and WebP images, PDFs up to six pages, and MP4 videos up to two minutes, each
up to 20 MiB. Other media stays with Voyage. A text-only model leaves media
with its existing provider. Retained Voyage media remains export-only for semantic search. The preview explains these retained providers.
If the rebuild fails, the saved selection stays unchanged.

To return to your configured providers, delete `local-text.json` beside the
vault's `config.toml` and run `docbank daemon restart`. Any documents added or
changed while local indexing was selected need processing under the configured
profile.

Moving a recipe processes the previewed document count before setup returns, and
restarting resumes background indexing for the saved profile. Revoking
processing consent stops provider calls.

Both `--server auto` and `--server local` use the shared per-user `kennembed`
server, which Docbank discovers through its per-user runtime record. When the
server or model is missing, the command prints the download size first and
downloads them from Kit's shared installation. An already installed server
downloads nothing, and the daemon's changing loopback port keeps the same
consent grant.

If you cancel before apply or the server fails to start, your saved choice stays
unchanged. New imports keep the same ongoing approval. The CLI installs and
starts the shared server before apply, whereas API apply installs a missing
server after consent and plan verification and then starts it. Docbank grants
consent before saving the selection. If a failure happens before file
publication, the previous selection stays in place and Docbank reports that
consent was saved. A later durability failure reports that the new selection was
published. The CLI restarts and reconnects the daemon before reporting that
indexing is enabled. If restart fails, the choice stays saved, and you can run
`docbank daemon start` to activate it.

To set up local text search in the web app:

1. Open **System → Search by meaning** and review the download size.
2. Choose **Turn on** to approve setup. Setup starts the server and saves the
   indexing choice for this vault and new imports.
3. Run `docbank daemon restart`, then reopen the web app to start indexing and
   search by meaning. The tab keeps the restart instruction until the daemon
   restarts.

A successful built-in setup in the web app or CLI records the choice for other
tools on this computer that share the built-in server. If another tool recorded
that choice, Docbank installs any missing shared files and starts indexing at
its next launch without saving a vault setup. If setup fails, choose **Turn on**
to try again. Existing configured providers keep their settings.

For other processing, configure a profile along with its referenced rendition,
embedding, and retrieval profiles, then pick a name listed by
`processing profiles`.

Before sending a document or query to any provider, preview the current version:

```bash
docbank processing plan /inbox/notes.txt --profile <configured-profile>
```

The plan lists the stable node and content-version IDs, profile fingerprint,
every provider flow and trust boundary, input classes disclosed to each flow,
retained derivative classes, estimated source bytes and provider calls, any work
beyond the selected source that consent starts, whether consent is currently
required, and the backup consequence. For each flow, it also names the immediate
and ultimate processor, endpoint, deployment, model and revision, vector space
where applicable, provider-visible metadata, and retained artifact roles. These
runtime disclosures are part of the plan fingerprint. Re-run the preview
whenever the source, profile, or provider destination changes. The consent state
shown in a plan is advisory and does not change the fingerprint; execution
checks consent again.

If local inspection admits a file but the profile's provider doesn't accept its
format, the plan reports `unsupported_media_type`, and starting that plan fails
with `rendition_source_ineligible`. The web app doesn't offer that profile for
the file.

Run only the plan you reviewed:

```bash
docbank processing build /inbox/notes.txt \
  --profile <configured-profile> \
  --plan-fingerprint <fingerprint-from-plan> \
  --consent
```

Pass the lowercase SHA-256 fingerprint from the preview in `--plan-fingerprint`.
The CLI requires `--consent`, which grants ongoing permission for this profile
configuration to the vault's daemon operator across documents and searches, with
no expiry. That grant is not limited to this job, and workers check consent
again before provider egress and before publication. The daemon returns a
durable processing job ID before provider work finishes. That receipt confirms
acceptance only. The daemon reports progress or failure in later status. Pass
the job ID to inspect aggregate state and any failure code:

```bash
docbank processing status <job-id>
```

For supplied recordings, `media submit` stores the original bytes and caller
occurrence before processing starts. Review the ASR plan, grant its exact
disclosure, and track the attempt with `media status`. After a transient or
credential failure, `media retry` queues a later attempt. It still requires the
same consent and returns before the provider finishes, and `coverage_state`
keeps the last successful transcript separate from the newest attempt.

For automation, `processing profiles`, `plan`, and `status` accept `--json`.
`processing build --ndjson` writes the job event right away, followed by one
terminal status or error event. If the stream ends early, keep the first job ID
and query its status. The authenticated HTTP API exposes the same preview,
consent, run, status, rendition, and coverage contracts, plus search limited to
an authorized source set, known as a source fence. See the
[HTTP API](../architecture/http-api.md).

## Prepare a native PDF

Before you design redaction decisions, inspect the current PDF version:

```bash
docbank redaction prepare /inbox/source.pdf
docbank redaction prepare id:42 --version <retained-version-id> --json
```

This command requires the configured
[Linux amd64 page runtime](../architecture/page-images.md). The original must
belong to a live file, though you can pass an explicit retained historical
version. The command checks the node revision and exact source identity; if the
selection changed, inspect it again before retrying.

Human output shows source, evidence, and map digests, plus each page's byte
span, accepted atom count, and ordered and unordered gap counts, without
printing original text. `--json` returns the complete
`native-pdf-preparation/v1` envelope, **including unredacted text**. Treat
any saved JSON like the original document. Agents can use the same JSON command
or authenticated `POST /api/v1/redaction/prepare`. The browser uses the same
preparation in its [PDF review editor](#review-a-pdf-in-the-browser). Local MCP agents can
[save a preparation for the same resolver](mcp.md#prepare-a-retained-pdf-for-review).

Preparation returns coverage gaps for you to inspect. Every ordered gap must be
fully masked, and an unordered gap requires masking its whole page, before the
resolver can proceed. A zero-gap result does not approve redaction or verify an
output PDF. Keep the complete envelope with its evidence and frames; the map
alone is not an execution authorization. This command applies no masks and
creates no retained job or output PDF.

The entire PDF must fit within 64 MiB and 16 pages. Retained evidence is capped
at 8 MiB and the full result at 15 MiB. Preparation work has a 60-second request
deadline. Limits or failures never return a partial preparation. Prepare, resolve, and
render share one slot with no queue. Busy or memory refusals exit with code 5;
wait for active work to finish, or retry when daemon memory use is lower.
Because the renderer's 1 GiB RSS ceiling counts the whole daemon, even a small
PDF can be refused on a busy process.

Exit codes distinguish failure types: malformed selections exit 2, unavailable
documents exit 3, changed selections exit 4, and failed integrity checks exit 6.
Missing runtime, unsupported PDFs, work limits, request cancellation, and worker
failures exit 1. Native text inspection does not add OCR, and the
[font and coverage limitations](../architecture/page-images.md#remaining-refusals)
still apply.

## Describe photos

To generate photo descriptions, configure a
[vision profile](../configuration.md#photo-descriptions), review
`processing plan` for a photo, and grant consent with `processing build`.
Eligible photos then queue automatically, a few at a time. The provider receives
the cached grid JPEG, sized to at most 512 pixels on its longest edge and
re-encoded without metadata; filenames and original bytes stay in the vault.
Docbank skips hidden, excluded, trashed, and superseded photos before the call
and again at publication, and a hidden photo retries once it becomes eligible
again.

[Automatic face detection](../configuration.md#automatic-face-detection)
requires separate profile consent and sends the same stripped grid preview.

The first request to a provider checks it with a synthetic image, and a failed
check retries no sooner than a minute later. If the check fails when you grant
consent, the error tells you that consent was saved. Caption, tags, and visible
text arrive together as one [suggestion](photos.md#suggested-metadata). Docbank
drops an invalid caption or an oversized tag without discarding the rest of the
suggestion; a non-JSON reply publishes nothing.
If a connection drops, times out, or the daemon restarts
during a description call, Docbank retries it with backoff, since a description
has no side effects.

Derivative purge and consent revocation leave pending suggestion text in place.
Dismissing the suggestion removes it, and purging the photo version removes it
too.

## Turn on face suggestions

To enable local face suggestions, run `docbank processing faces`. The command
reviews the local processing plan and prints the missing download size before
installing anything. The face model runs locally on photo previews and on
detected or manually drawn face boxes. Turning it on grants ongoing consent for
this vault, including new photos, and the daemon can start using that consent
right away.

The face model uses its own profile and download, and setting up text search
doesn't enable face suggestions. At startup, the daemon reuses installed face
assets; if assets are missing, run setup again. Faces that the model can't align
stay available for manual naming. You can review and name suggested groups in
[People](people.md#face-suggestions).

Profiles that include face embeddings use this setup and background worker.
Per-file plans, starts, consent grants, coverage, search fences, and aggregate
job status refuse these profiles, including custom and mixed profiles supplied
through the Go API.

## Embed photos for meaning search

Image bindings power Photos search, while Documents search uses text bindings. A
hybrid search with only image bindings uses filename and retained-text matches.

An image embedding binding uses `input_kind = "image"` with the
`docbank-standard-embedding-bridge/v2` adapter, and its descriptor must support
both image documents and text queries in the same vector space. The bridge
receives a verified, stripped grid JPEG through derived-upload authority with an
empty filename. Original image bytes and embedded metadata stay in the vault.

After you grant consent, existing embedding reconciliation discovers eligible
photo versions in bounded pages. Jobs follow the existing retry, fencing,
vector-generation activation, and retirement paths. Docbank checks visibility,
current content, preview identity, purge state, and consent before upload and
again before publication. Export and backup retain image authority alongside the
existing embedding catalog. Configure the bridge's limits to match your
provider; image inference runs through the configured bridge.

## Review and render a native PDF

Save the preparation, supply your explicit masks, and inspect the review before rendering:

```bash
docbank redaction prepare /inbox/source.pdf --json > preparation.json
docbank redaction resolve preparation.json masks.json --json > review.json
docbank redaction render preparation.json masks.json review.json redacted.zip
```

The resulting ZIP contains exactly `redacted.pdf`, `redacted.txt`, and
`receipt.json`. Extract `redacted.pdf` to open it in a PDF viewer.
`redacted.txt` holds the sanitized text with a form feed after each page, and
`receipt.json` binds the source, review, recipe, and file hashes. Passing
`--json` to render also reports the local archive path, size, hash, and
publication state. Pass `--overwrite` to replace an existing output file;
without it, Docbank keeps any existing or concurrently created file. Choose an
output path outside the Docbank data directory.

`masks.json` is a JSON array where each mask has a distinct canonical lowercase
UUIDv4 `id` and a `selector`. For example, this entry removes the first six
UTF-8 **bytes**:

```json
[
  {
    "id": "11111111-1111-4111-8111-111111111111",
    "selector": {
      "kind": "text",
      "map_sha256": "<map.sha256 from preparation.json>",
      "span": {"start": 0, "end": 6}
    }
  }
]
```

Text spans must end on UTF-8 boundaries. Rectangle selectors use `boxes`, each
with `page`, `frame_sha256`, `x0`, `y0`, `x1`, and `y1` in the map's integer
page frame. Page selectors use `pages`, a list of one-based page numbers. Every
selector kind carries `map_sha256`. Within the 1 MiB mask limit, you can include
up to 256 masks, 1,024 boxes, and 1,024 page references.

Run resolve without `--json` to inspect effective masks, removed byte spans,
and quoted retained text for each page. If a partial atom needs a larger mask,
resolve returns `selection_expansion_required` and writes the complete proposal
to stderr. Review its `expanded` selector, explicitly replace the selector in
your masks file, and resolve again; nothing silently accepts an expansion. Every
gap in the document must be covered: ordered gaps need covering masks, and
unordered gaps need a whole-page mask. `mapping_incomplete` produces no review
or output PDF.

Both resolve and render inspect the selected original again. Tags, names, moves,
and a newer current version do not invalidate a review of an unchanged retained
version. The selected version must still belong to the live file, as trash or
pruning makes it unavailable. Revisions in the review and receipt are
informational. Keep the complete preparation envelope, masks, and review files
together; a bare map is not enough, and these files contain unredacted evidence.

Rendering can hold the shared slot for up to ten minutes, including delivery,
while prepare and resolve each have a 60-second request deadline. If the slot is
busy, the error names the active operation. Busy or memory refusals exit 5; wait
for that operation to finish or lower daemon memory use before retrying. Changed
evidence or review exits 4 and requires a new review. Invalid local inputs exit
2, unavailable versions exit 3, integrity failures exit 6, and other refusals or
I/O failures exit 1. Docbank never retries a write automatically. If you see an
output or cleanup error after a reported save, inspect the named archive before
retrying.

The daemon verifies the fresh PDF before building the ZIP, and the CLI verifies
the transferred archive, its inventory, file hashes, receipt, and reviewed text
before publishing a single file. Client checks do not independently verify the
source pixels. Plain rendering retains no job, attachment, or output for later
download. A killed daemon may leave private files in its spool; see the
[staging bounds](../architecture/page-images.md#native-pdf-mask-execution).
Agents can use these CLI commands or the authenticated
`POST /api/v1/redaction/resolve` and `/api/v1/redaction/render` routes. Browser
sessions use resolve along with the temporary-preview and numbered-publication
routes described below. Direct ZIP rendering requires an API key. MCP agents
can [save a review or an expansion proposal](mcp.md#resolve-explicit-pdf-masks),
[save an independently checked unnumbered ZIP](mcp.md#deliver-a-reviewed-pdf-without-bates-numbers),
and [publish an operator-reserved numbered PDF](#deliver-one-reviewed-pdf-with-bates-numbers).

### Review a PDF in the browser

To review and redact a PDF in the web app, open a live PDF in **Documents** and
choose **Redact PDF**; the inspector offers the same entry point. Reading and
marking share the same page navigation, search, and zoom controls. You need the
configured Linux amd64 page runtime, and the editor supports one original PDF up
to 64 MiB and 16 pages. Empty files, saved-result sources, and related previews
cannot start a review. **Fit page** shows the whole page at first; switch to
**Fit width** or a percentage to inspect details more closely.

1. Wait for the original page to render. With **Draw rectangle** off, select a
   passage on one page and choose **Mask selected text**. One Remove action
   removes the whole selection. You can also draw rectangles or choose **Mask
   whole page**. To draw with the keyboard, focus the PDF, move with the arrow
   keys, and press Enter at each corner. Shift moves 0.1 inch, and Escape
   cancels the unfinished rectangle. The reader announces the page and cursor
   position in inches as you move it with the keyboard. Arrows follow screen
   directions after viewing rotation, and the crosshair stays the same size on
   screen at every zoom. Page and zoom changes cancel an unfinished rectangle
   too. Drawing stays on after each rectangle.
2. Cover every coverage gap. An unordered gap needs a whole-page mask. A safety
   margin can reach nearby text even when you selected complete words. Review
   any proposed expansion and explicitly choose **Use expanded mask**, or keep
   editing.
3. Choose **Preview redaction**, then inspect **Redacted** and **Text** on each
   page. The displayed page images are the exact images verified against this
   PDF.
4. Choose **Download unnumbered ZIP**, then **Download ZIP**. Extract
   `redacted.pdf` to open it. The ZIP also contains the sanitized text and
   receipt. Keep the tab open until the transfer finishes.

**Original** is an unchecked browser rendering of verified source bytes and may
differ from the server renderer. Server coverage gaps and the verified
**Redacted** view remain authoritative. Drawing is disabled while the displayed
page is loading or has failed; choose **Retry page** to retry a failed image.
Cropped pages may use fractional point coordinates, but unsupported precision or
a browser/server geometry disagreement still disables drawing.

Text selection must match the server's prepared text uniquely. If a full context
repeats, extraction differs, or the passage is oversized, use rectangle or
whole-page marking instead. Ligatures and some non-Latin text may already have
server coverage gaps. A selection can span several lines, and matching selected
line breaks are removed with the passage. Different extraction order can produce
several text masks shown as one removable group. Text masks count toward the
existing 256-mask limit without adding a rectangle box for each glyph. If you
approve an expansion covering separate text ranges, it becomes a rectangle mask;
it uses rectangle capacity and keeps whitespace between the selected glyphs.

Choosing **Back to reading** discards your masks and preview after confirmation
and restores your reading position, while the original stays loaded. Original
print and export are disabled during review. Download the reviewed ZIP, or keep
a numbered PDF in Productions.

Your original stays unchanged. If the source version changes before you preview,
reopen the intended PDF; tags, names, and moves preserve your masks. Once ready,
the preview is frozen even if the original is replaced or trashed. Choosing
**Back to masks** discards that output before you edit again. Repeated downloads
return identical bytes. Verification checks the masks and text, though it does
not prove that unmasked pixels match the original or authenticate the producer.

Canceling a preview keeps your masks. If creation loses its response, the editor
releases any server preview before enabling another attempt. If cleanup fails,
choose **Release preview** to retry it.

Only one temporary preview can be open across the daemon. It expires after 30
minutes and is removed on discard, sign-out, or daemon restart. Closing or
reloading the tab requests cancellation of this preview and its transfers,
without ending the browser session or canceling other exports and imports.
Returning to a cached page leaves the editor closed. If a crash or lost cleanup
request prevents removal, wait for expiry. There is no saved draft or restart
recovery. A ready preview releases the rendering slot, though it can retain up
to 1.53 GiB of private spool files. See
[temporary-preview storage](../architecture/page-images.md#temporary-browser-redaction-previews).

For later offline verification, expand **Private review files** and save the
preparation, masks, and review files. Their names contain `PRIVATE`, and
preparation contains unredacted evidence. Keep all three separate from the
delivery ZIP, and use them with
[`redaction verify`](#check-and-retain-a-saved-archive).

### Deliver one reviewed PDF with Bates numbers

To number and keep a reviewed PDF in the browser, start from a ready redaction
preview and choose **Number and keep PDF**. Select an existing namespace, or
create one with a prefix, suffix, and digit count. The first-number field starts
at that namespace's next sequence. You can change it or choose the **Refresh
next number** icon beside the field before confirming. Starts below the known
next number are refused. Treat the suggested number as advisory: other
reservations can advance the cursor, and label formats can collide. Any conflict
requires another explicit choice and confirmation. A namespace that cannot fit
all pages stays unavailable even if you enter an earlier first number.

Check the displayed range, opt in to keeping the unredacted original
indefinitely, and choose **Review numbering**, then **Reserve and keep PDF**.
Each page gets a half-inch footer below the reviewed content; a Letter page
becomes 612 by 828 points. The server verifies both the content and the footer
before publication. Each kept PDF shows its label range. Under **Download**,
choose **PDF** to download the retained PDF, or use the open icon beside the
range to open Productions. If you edit masks later, create another preview to
number the new review with a separate range. Earlier published PDFs stay
downloadable until you close the review. Downloads check the retained file
identity, without rechecking masks and approved text. The plain preview and ZIP
stay available until their usual expiry or discard. Choose **Download unnumbered
ZIP** to save that reviewed preview; download it before expiry if you need its
sanitized-text sidecar. This action produces no numbered ZIP.

A failed or canceled request cannot release reserved numbers. Choose **Retry
reservation** to replay the same frozen range when its reply was uncertain.
Choose **Retry publication** to use the existing allocation, even if the live
source was trashed and purged. A lost successful reply returns the first
retained PDF without another render. Preview expiry does not erase an
in-progress numbering request or its private review files, though closing the
tab loses that draft. Save all three private files before reserving; finishing an unpublished range
after a reload requires them through the CLI. You can recover completed PDFs by
number without those files.

You can run the same workflow through the CLI.

Reserve a range only when you intend to keep the original indefinitely. Doing so
creates a real collection snapshot that retains the unredacted original through
trash, garbage collection, and backups. There is no release or void command for
this snapshot or range, and `--retain-original` is required on every reservation
request, including retries.

Prepare and review the PDF as shown above, then choose a namespace and an
explicit start number:

```sh
docbank bates namespaces --create --prefix PROD --padding 6 --json
docbank redaction reserve preparation.json masks.json review.json \
  --namespace <namespace-uuid> --start 1 --retain-original --json > reservation.json
docbank redaction render preparation.json masks.json review.json numbered.zip \
  --allocation <allocation_id-from-reservation.json> --json
```

Each source page gets a half-inch footer with its reserved label. If a label
cannot fit, Docbank refuses it before retaining the original or reserving
numbers; labels are never clipped or shrunk. Masks cover the source area only.
The ZIP still contains `redacted.pdf`, `redacted.txt`, and `receipt.json`, and
you can extract `redacted.pdf` to view it. The PDF text layer includes the
numbers, while `redacted.txt` contains only the reviewed sanitized text without
footer labels. The receipt records the reservation, labels, and indefinite
original retention.

The vault retains the numbered **PDF**, its recipe, and page receipts, but it
does not retain the ZIP, sidecar, or the preparation, masks, and review files.
Keep those three review inputs separately for full verification and ZIP
reconstruction. Each new PDF embeds about 6 MB of compressed font data, even for
a small document.

Reservation and snapshot creation are atomic. After an interrupted reservation,
repeat the exact same command with the same review, namespace, start, and
retention consent to return the existing allocation. Changing those inputs
requests a different allocation or produces a conflict; it is not a recovery
step. Failed renders leave the range reserved, and numbers are never silently
reused. Retry render with the same `--allocation`, even if the source was
subsequently trashed: the sealed snapshot supplies the exact original. A
completed retry uses the first retained PDF and rebuilds the same ZIP without
the page runtime. It still takes the shared native-PDF slot; a running render
can hold that slot for ten minutes, during which other attempts fail as busy.
Reservation-receipt replay and retained PDF downloads need no slot.

You can recover the PDF by allocation ID, or find the allocation by its number
using `find_bates_exports` or
`GET /api/v1/bates/exports/candidates?bates_label=PROD000001`:

```sh
docbank bates export download <allocation-id> recovered.pdf --json
docbank bates export download <allocation-id> checked.pdf --json \
  --preparation preparation.json --masks masks.json --review review.json
```

| Inputs and artifact | `verification` result | What is checked |
|---|---|---|
| Numbered PDF without review files | `recorded_bytes_and_numbers` | Size and SHA-256 against the retained artifact, plus recipe, allocation and label metadata. Masks, approved text and rendered numbers are not rechecked. |
| Numbered PDF with all three review files | `masks_text_and_numbers` | Recorded identity plus PDF structure, reviewed masks and text, footer layout and rendered numbers. |
| Ordinary stamped Bates PDF with the installed engine identity | `stamped_labels` | Recorded identity and the existing stamp verifier. |
| Ordinary stamped Bates PDF with a different recorded engine identity | `recorded_bytes_and_numbers` | Recorded identity and numbering metadata. Rendered stamps are not rechecked. |

If you pass any review-file flag, you must pass all three. Docbank selects the
verifier from the checked recipe before download, and a failed full check never
retries a narrower one. Recovery without review files works after restart and
backup/restore without a configured page runtime or the original renderer,
writer, or font version; its assurance depends on the vault's retained record.
Full verification requires a supported engine recipe. Both the original v2
writer and the compressed-font v3 writer are supported for verification, and
unfinished v2 reservations still render with their original review and numbers.
To start a new reservation or plain render from an old v2 review, resolve again;
otherwise the command exits 4. New reviews use v3, saving about 3.62 MB per PDF
without changing its font or character coverage. Other unqualified engine
identities report "verifier version differs" and exit 1, rather than reporting
an integrity failure. Footer text and placement must match exactly. Docbank
compares visible numbers against independently rendered expected labels: each
grayscale sample inside the label box may differ by at most 16 on the 0 to 255
scale, positions must match, and pixels outside the box must be white.
Render-time verification still compares the complete raster exactly. Full
verification still does not prove that unmasked pixels match the source or
authenticate the producer.

Agents can reserve and render through the CLI or with API-key authentication.
Browser sessions and API-key callers can reserve through
`POST /api/v1/redaction/reservations` and publish a retained PDF through
`POST /api/v1/redaction/publications`, while the ZIP-streaming
`/api/v1/redaction/render` route remains API-key-only. MCP's `export_bates_file`
can fully check a retained numbered PDF when given `preparation_path`,
`masks_path`, and `review_path` together, reporting `masks_text_and_numbers`; a
failed full check never falls back to recovery. Without review files, it checks
recorded bytes and numbering metadata and reports `recorded_bytes_and_numbers`.

With `--allow-package-writes`, MCP's `publish_numbered_pdf` can publish into an
allocation the operator already reserved through the browser or CLI. It requires
the three private review files and their review digest, and cannot grant
retention consent or reserve numbers. For inputs and exact retry advice, see
[MCP publication](mcp.md#publish-a-reserved-numbered-pdf).

Load-file exports accept ordinary stamped allocations only. To deliver numbered
redactions to a recipient, use `redaction package`.

### Package completed numbered PDFs for a recipient

In the browser review, choose **Download** › **Recipient ZIP** beside a
completed numbered PDF. Each completed delivery keeps its own action, even after
you edit masks or publish another range. The ZIP contains that retained PDF, its
reviewed text without footer labels, and DAT/OPT load files for recipient
import. Private preparation, masks, and review files stay outside the ZIP.

Before offering the download, the server rechecks masks, text, and numbers. It
does not render again or require the original to remain live, and the page
runtime is not required, though the recorded renderer and writer must still be
supported for full verification. No weaker check is substituted. The package
holds no rendering slot while waiting for download, but only one package can
wait or transfer at a time, and an unused download link expires after two
minutes. Keep the review open until the transfer finishes; closing it cancels the
package and any transfer in progress. If cleanup fails, choose
**Release recipient ZIP** before retrying. The completed PDF stays in
Productions, while preparing a new recipient ZIP needs its open private review.

From the CLI, `redaction package` bundles completed numbered redactions into one
ZIP for recipient import. It preserves each retained PDF and its numbers,
verifies masks, text, and visible labels against your saved reviews, and
includes label-free text plus DAT, OPT, and mapping files. It creates no new
reservation or rendering.

Create `selection.json` in the order you want the documents to appear:

```json
{
  "contract": "native-pdf-recipient-selection/v1",
  "documents": [
    {
      "allocation_id": "11111111-1111-4111-8111-111111111111",
      "preparation": "first/preparation.json",
      "masks": "first/masks.json",
      "review": "first/review.json"
    },
    {
      "allocation_id": "22222222-2222-4222-8222-222222222222",
      "preparation": "second/preparation.json",
      "masks": "second/masks.json",
      "review": "second/review.json"
    }
  ]
}
```

Paths are relative to the selection file, and absolute paths work too. Every
allocation must be a completed numbered native redaction; ordinary stamped
exports are refused. Allocations and labels must be distinct. Labels must fit
the recipient importer's 256-byte limit, and commas and line breaks cannot be
represented in OPT.

```sh
docbank redaction package selection.json recipient.zip --json
```

Before downloading any PDF, the command checks all local inputs and pins each
reviewed text file's size and SHA-256. It then reads each document's inputs
again and requires the same review digest; a changed file is refused and never
silently adopted. The command reads each input twice and keeps only one full
review and one downloaded PDF in memory or staging at a time. You can mix
supported v2 and v3 deliveries. The daemon must hold the completed artifacts,
though a live source or configured page runtime is not needed. Embedded PDFium
still performs full verification with no reduced-check fallback.

The ZIP contains one `VOL001` directory where each document gets
`PDF/000001.pdf` and `TEXT/000001.txt` paths assigned in selection order.
`DATA.DAT` has one row per document, and `DATA.OPT` has one row per page
pointing into that whole PDF. The first label is the document ID, and existing
ranges and gaps stay unchanged. `mapping.json` and `receipt.json` sit at the ZIP
root. The receipt records file hashes, allocation and namespace IDs, labels, and
review digests, with no original text, source identities, masks, or review
payloads. Keep the input files privately; they are not sent to the recipient.

JSON output reports `state`, `destination`, `size`, `sha256`, `receipt_sha256`,
`verification: "masks_text_and_numbers"`, and the receipt. Save the whole-ZIP
hash separately. Verification does not prove that unmasked pixels match the
original or authenticate the producer.

Agents can save the same package through
[`export_recipient_package`](mcp.md#export-a-reviewed-recipient-zip) using
`--allow-package-writes` and absolute selection and destination paths. It uses
the same preflight, full checks, and package format. Prefer stdio: a package can
hold MCP's shared full-PDF check slot for 30 minutes, while HTTP has a
two-minute request deadline.

Limits are 16 documents, 16 pages per document, 64 pages total, and 512 MiB for
the complete ZIP. The selection and each metadata entry are limited to 256 KiB,
and each text file is limited to 16 MiB. The command has a 30-minute deadline,
with 10 minutes per PDF and the existing renderer memory and page limits. A
private directory beside the destination holds the growing ZIP and at most one
PDF, up to 1 GiB combined plus metadata and filesystem overhead. Files on a
memory-backed filesystem consume RAM beyond the renderer's RSS accounting.

Choose a destination outside the Docbank data directory. Publication never
overwrites an existing destination, and a failure removes staging without
leaving a partial destination. If reporting fails after publication, the error
names the saved file. A hard kill may leave a `docbank-recipient-*` directory
beside the destination; after confirming that the process has stopped, remove
that exact directory and retry. Packaging does not retain its ZIP automatically; use `add` if you want to store
it. Existing allocations continue to retain their unredacted originals
indefinitely.

Exit codes distinguish failure types: malformed or oversized inputs exit 2,
changed reviews exit 4, resource refusals exit 5, and PDF or package mismatches
exit 6. Missing retained artifacts exit 3, while unsupported verifier versions,
I/O failures, and cancellation exit 1.

To import the delivered ZIP, extract it first, then run:

```sh
docbank package preflight ./recipient --profile dat-concordance-v1 \
  --page-map-profile opt-standard-v1 --encoding utf-8 \
  --map ./recipient/mapping.json --json
docbank document mkdir /received
docbank package import <preflight-id> --name "Received delivery" \
  --into /received --index-supplied-text --json
docbank package import status <operation-id> --json
```

Use the returned preflight ID, and do not add `--accept-partial` to bypass an
error. `package preflight` accepts a directory; direct ZIP import is available
through the existing sealed mailbox-container API path. Recipient import
preserves the sender's PDFs, text, and labels, without checking the sender's
private review or establishing provenance. Range endpoints are recorded under
the sender's namespace UUID, and every OPT page label is recorded under
`received` with received provenance. Import creates no recipient Bates namespace
or allocation.

### Check and retain a saved archive

To verify a ZIP after copying or recovering it, run `redaction verify` with your
saved preparation, masks, and review files:

```sh
docbank redaction verify preparation.json masks.json review.json redacted.zip
docbank redaction verify preparation.json masks.json review.json redacted.zip --json
```

This command runs offline without a daemon, source PDF, or configured page
runtime. It checks the ZIP inventory, entry checksums, receipt bindings, and
sanitized text, then uses embedded PDFium to check the PDF against the review.
Every reviewed mask must be opaque black, the searchable text and its placement
must match the reviewed runs, and the file must have the fresh writer's
structure. For numbered output, the receipt also binds the reservation and
expected footer labels, and the verifier checks their layout, text, and pixels.
The command prints the complete archive's SHA-256, byte count, and receipt;
`--json` outputs `{ "receipt": ..., "size": ..., "sha256": ... }`.

The archive must be a regular file of at most 544 MiB, and you must leave it
unchanged while the command captures it. All checks run against one private
temporary ZIP snapshot that Docbank removes before printing success. Allow up to
544 MiB of temporary storage; a memory-backed temporary directory consumes RAM
outside the process RSS limit. Verification has a ten-minute deadline and a
60-second limit per page, and it accepts up to 16 pages at 300 DPI within the
render recipe's image and text limits. The verifier uses the current preparation
and recipe contracts and does not promise compatibility with every future
format.

A passing check confirms that the PDF implements the reviewed masks and text. It
does **not** prove that unmasked image content matches the source or
authenticate the producer, and changes outside the masks can pass if the receipt
hashes are updated too. Compare the reported whole-archive SHA-256 against the
value you saved from `redaction render`, or against a trusted vault version's
`blob_hash`. The archive's own receipt is not that independent record.

To retain the ZIP in a vault, store it as an ordinary file:

```sh
docbank document mkdir /produced
docbank document add redacted.zip --dest /produced
docbank document get /produced/redacted.zip recovered.zip
docbank redaction verify preparation.json masks.json review.json recovered.zip
```

Packing, garbage collection, and backup treat the ZIP as an ordinary file. Keep
the preparation, masks, and review files separately; they contain
unredacted evidence and are not included in the ZIP. Backing up the ZIP alone
does not recover those inputs, and deleting the source PDF does not prevent
offline checks.

Exit codes distinguish failure types: malformed local JSON inputs exit 2,
changed review bindings exit 4, and archive or PDF integrity failures exit 6. A
renderer RSS refusal exits 5 and can be retried when memory use is lower.
Opening, reading, staging, or cleaning up files, cancellation, timeouts, engine
failures, and output failures exit 1. Docbank prints no success result on
validation failure; check the exit status even when output was partially
written.

## OCR

Pages without a text layer may be scans or blank pages, and OCR can read scanned
text. Configure a
[Docling profile](../configuration.md#document-conversion-with-docling) or a
[Mistral OCR profile](../configuration.md#document-ocr-with-mistral), then run
`processing plan` and `processing build` with consent. Neither profile runs
automatically.

Processing coverage for `docbank-pdf-text` lists the source PDF pages that have
no text layer.

During local inspection, both profiles refuse PDFs containing DCT, JBIG2, or JPX
streams. Those refused PDFs include many scans.

## Convert documents with Docling

To convert documents through your Docling Serve deployment, configure a
[Docling document rendition profile](../configuration.md#document-conversion-with-docling)
with the `docbank-docling-document/v1` adapter and select it from a processing
profile. The daemon can convert locally admitted PDFs, PPTX/XLSX files, plain
text/Markdown, and PNG/JPEG images. Local inspection still blocks DOCX, and this
profile does not admit HTML, TIFF, legacy Office, audio, or video. When local
inspection refuses a file, its plan prints the reason on an `ineligible:` line
and `processing build` fails with `rendition_source_ineligible`. A locked PDF
instead ends with `pdf_password_required` when no configured password opens it
or no password file is configured.

Import the original, inspect its plan, and consent to the exact destination:

```bash
docbank document add /path/to/report.pdf --dest /inbox
docbank processing plan /inbox/report.pdf --profile <configured-profile>
docbank processing build /inbox/report.pdf \
  --profile <configured-profile> \
  --plan-fingerprint <fingerprint-from-plan> \
  --consent
```

The plan discloses the endpoint, deployment, trust boundary, filename policy,
and retained artifacts; neither import nor preview sends the document to
Docling. Track the returned job with `processing status <job-id>`. Once it
completes, the retained Markdown is available through the rendition read API and
lexical search, while the original bytes stay unchanged. PDF output can retain
exact page locations; other supported formats report degraded provenance.

## Convert PDFs with Mistral OCR

To retain searchable Markdown from PDFs with Mistral, configure a
[Mistral OCR rendition profile](../configuration.md#document-ocr-with-mistral)
with an authenticated capability manifest and select it from a processing
profile. `docbank processing profiles` lists the executable profile. Use the
same `processing plan` and consented `processing build` workflow as Docling. The
plan discloses Mistral's EU endpoint and the pinned model before upload. Local
refusals end builds with `rendition_source_ineligible`, except locked PDFs,
which end with `pdf_password_required` when no configured password opens them or
no password file is configured.

## Password-protected PDFs

Docling and Mistral processing can open encrypted PDFs using
`[media].pdf_passwords_file` from [configuration](../configuration.md). Put one
password per line in that owner-only file. Both LF and CRLF line endings work;
Docbank ignores blank lines, and spaces belong to the password. The file must be
UTF-8 without a byte-order mark and can hold up to 16 KiB and 128 passwords.

Docbank decrypts a copy in memory before sending it to the provider, while the
archived original stays encrypted and unchanged. If no password opens the PDF or
no password file is configured, the browser dialog says the PDF is locked and
disables its run button. CLI and API builds create a job that ends `failed` with
`pdf_password_required`, and the CLI reports "no configured PDF password opens
this PDF" and points to `media.pdf_passwords_file`. Inspect the job with
`docbank processing status <job-id>`.

To unlock a failed PDF, add a working password to the file and build again. The
daemon reads the file for each locked-PDF inspection, so editing passwords needs
no restart; restart once when you first configure the file path. If the daemon
can't read the file, planning or building a locked PDF returns
`rendition_operator_required` with "PDF passwords file can't be read; check
media.pdf_passwords_file", without revealing the file's path. Queued jobs retry
until you fix the file, while plain PDFs and audio keep processing.

### Design choices

Docbank uses pdfcpu v0.16.4 to open AES-256 PDFs, including passwords containing
spaces and punctuation such as curly quotes or `£`. AES-256 password handling
applies Unicode normalization to decomposed accents and fullwidth letters. Older
RC4 and AES-128 ciphers use PDFDocEncoding, so Docbank also tries each password
converted from UTF-8 to that encoding. A rejected password candidate does not
stop Docbank from trying the remaining entries. When a password file is
configured, Docbank tries an empty password first, even if the file is empty or
contains only blank lines, so owner-only PDFs open. Without a configured file,
Docbank tries no passwords, and every encrypted PDF reports
`pdf_password_required`.

## Find similar documents

In the web app, open a file's **Find similar** row action to search for similar
files in the **Similar documents** drawer. The drawer searches the file versions
loaded in your current view using the selected file's stored embeddings locally.
If you have several profiles that store embeddings, pick one. A file without
embeddings for that profile shows no results; process it and retry. If your view
contains more than 4,096 file versions, narrow it first.

In the TUI, press uppercase `S` on a file to search the current listing;
lowercase `s` still sorts. The selected file is excluded, though another file
with identical bytes can appear. Results group identical content and show the
number of other eligible copies, and coverage describes this bounded scope. For
the equivalent `--similar-to` command, see the
[CLI reference](../cli-reference.md#find-similar-files).


![Similar documents found locally from stored embeddings in the TUI](https://docbank.ai/assets/generated/tui-similar.png)

## Processing flows and boundaries

Every profile makes its disclosure visible before execution. Three shapes are
useful:

- **Private rendition.** Configure a rendition profile with
  `docbank-plaintext-rendition/v1` and select it from a processing profile to
  run in process with no network connection. It accepts supported UTF-8
  text-like media types up to 16 MiB and produces one generic evidence unit, so
  Docbank reports its provenance as degraded. The `docbank-epub-rendition/v1`
  adapter also runs locally, extracting ordered XHTML spine text from supported
  EPUB packages while keeping source locations. Its plan reports `local_process`
  and the `in-process` destination before consent; see
  [local EPUB extraction](../document-understanding.md#extract-epub-locally) for
  limits and counting rules. The `docbank-pdftext-rendition/v1` adapter reads
  existing PDF text locally and records exact page omissions for pages with no
  text layer. An image-only PDF completes with those omissions and an empty text
  body. See
  [local PDF text extraction](../document-understanding.md#extract-pdf-text-locally).
- **Hosted provider.** A configured Mistral OCR or embedding runtime, or an
  embedded caller's rendition or embedding provider, may cross a hosted-provider
  trust boundary. The reviewed plan names the provider and the input class it
  receives: the original file, a rendition chunk, an original-file embedding
  input, or query text. Docbank does not treat a hosted profile as private. A
  configured Docling document runtime can receive an original file and retain
  searchable Markdown, and a configured Docling ASR profile can receive an
  original supplied WAV or MP3 file and publish generated transcript evidence.
  The daemon reports the provider destination before the operator grants
  consent.
- **Direct embedding.** A profile can embed an original file without a rendition
  provider. That flow has no readable rendition or lexical body index, so a
  relevant direct-file result arrives without a text excerpt.

All flows use the same source identity, profile fingerprint, authorization,
bounded upload, and catalog publication path. An embedded Go application
supplies provider implementations through `ProcessingOptions`, and it cannot
bypass those checks. Embedded network providers need an endpoint disclosure when
the vault opens. See
[Configure document processing in Go](../embedding.md#configure-document-processing).

## Check a private processing deployment

From a repository checkout, you can run
[`scripts/test-private-processing.sh`](https://github.com/kenn-io/docbank/blob/main/scripts/test-private-processing.sh)
against your Docling Serve and OpenAI-compatible embedding endpoints. The runner
creates two synthetic text documents in a disposable embedded vault, stores
their renditions and vectors, and verifies lexical, semantic, and hybrid search
limited to each document's source fence. It does not read an existing vault or
personal corpus.

You need Bash, `setsid`, the repository's Go toolchain, and cached Go modules.
Populate the module cache with `go mod download` first, because the runner
builds with `GOPROXY=off`. Your embedding service must accept the Nomic
document/query input format. Set these variables to match your services; the
loopback URLs and model values below are examples:

```bash
export DOCBANK_PRIVATE_PROCESSING_DOCLING_URL='http://127.0.0.1:5001'
export DOCBANK_PRIVATE_PROCESSING_DOCLING_ALLOWED_CIDRS='127.0.0.1/32'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_URL='http://127.0.0.1:8080'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_ALLOWED_CIDRS='127.0.0.1/32'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_MODEL='nomic-embed-text'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_REVISION='local-v1'
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_DIMENSIONS='768'
read -rsp 'Embedding API key: ' DOCBANK_PRIVATE_PROCESSING_EMBEDDING_API_KEY
export DOCBANK_PRIVATE_PROCESSING_EMBEDDING_API_KEY
scripts/test-private-processing.sh
```

If Docling requires an API key, set
`DOCBANK_PRIVATE_PROCESSING_DOCLING_API_KEY` too. Endpoint URLs must be HTTP(S)
origins with a literal private or loopback IP, with no credentials, query
parameters, fragments, or path beyond `/`. Each endpoint must fall inside its
provider's allowed CIDRs, with multiple ranges separated by commas or
whitespace. Hostnames and public address ranges are rejected.

The output reports aggregate provider-request and connected-address counts,
along with the rendition and search results. It checks only the connections made
by Docbank's provider transports and does **not** attest to onward traffic from
those endpoints: `endpoint_onward_egress=not_attested` means the operator must
check that separately. On exit, including after failure, the runner stops its
processes and removes its temporary vault, binary, logs, and build cache.

## Inspect evidence before reporting

To inspect a file's processing coverage and retained text without starting new
processing, first fetch its `current_version_id` with `stat --json`:

```bash
docbank document stat /inbox/notes.txt --json
docbank processing coverage <version-uuid> --profile <name> --json
docbank rendition window /inbox/notes.txt --version <version-uuid> --profile <name>
```

For an indexed PDF with one readable profile, you can omit both the version and
the profile:

```bash
docbank rendition window /inbox/report.pdf
```

Coverage requires a profile listed by `docbank processing profiles --json`.
Window reads can also use a configured profile whose runtime is unavailable, and
reports can use retained evidence under such a profile.

Coverage reports rendition and embedding counters for the single requested
version. JSON output includes `content_version_id` and the complete `coverage`
object, while human output names the profile, fingerprint, state, and each
class's counters. Unavailable, partial, rebuilding, and stale observations still
exit successfully. Unknown, replaced, or trashed versions count as stale for a
required rendition, and an unneeded class marked `not_required` does not
establish source availability. `previous_generation_serving` overlaps
rebuilding. Do not add it to the total.

These processing counters do not inspect term matches, dates, or attachment
families, and they do not promise a successful
[strict report](search-exports.md).

### Read and continue a text window

`rendition window` reads the current version and picks the one active rendition
with readable text. If several renditions have text, it lists their names and
exits with a usage error; pass `--profile` to choose one. When a profile was
removed or edited, Docbank identifies its retained rendition by fingerprint, and
you can pass the listed `--attachment` to select it. Passing `--version` pins
the current version, which must remain current and live. By default, the command
returns at most 8,000 characters. Use `--max-chars` for 1 to 16,000 characters,
and pass `--json` to get the text, full rendition identity, offsets, and EOF
flag. The returned text is stored Markdown, including its metadata envelope and
escaped punctuation; it is not rendered or unescaped.

Human output prints a continuation command that you can run to keep reading the
same attachment:

```bash
docbank rendition window id:<node-id> --version <version-uuid> --profile <name> \
  --attachment <attachment-id> --offset <next-offset> --max-chars 8000
```

Offsets count Unicode scalar values rather than bytes or visible grapheme
clusters, from 0 to 2,147,483,647. Any nonzero offset requires an attachment ID,
and a pinned first window can also pass `--attachment` at offset zero. The
command refuses a replaced rendition instead of continuing into its replacement.
An offset exactly at EOF succeeds with empty text, while an offset past EOF fails. No
command fetches remaining windows automatically.

Discovery uses the document catalog and cannot find a file deeper than 256 path
components, with a path longer than 16 KiB in UTF-8 bytes, or with a filename
longer than 255 Unicode scalar values. That filename cap does not apply to
ancestor directories. The CLI names the exceeded limit, and refreshing the
selection does not solve it. If you already know the active attachment, pass
`--attachment` to skip discovery; the version and any supplied profile are still
checked.

Each unpinned discovery scans the traversable live vault tree, whereas pinned
calls skip that scan. The text reader still reads from the beginning to reach
the requested offset, so a bounded result does not imply a constant-time seek. A
window identifies the full artifact's checksum without independently verifying
the entire file; use `rendition get` below when you need full verification.

## Read a retained rendition

When a profile retains sanitized Markdown, its active attachment is an
authenticated Docbank resource that you can read by attachment ID:

```bash
docbank rendition get <attachment-id>
```

The command verifies the complete stream before accepting it and writes the
self-describing Markdown to standard output. A rendition is bounded to 64 MiB.
It is neither a replacement original nor a general-purpose provider export. The
web application and TUI display the same retained sanitized Markdown and its
identity, and they do not enable raw provider Markdown as ordinary document
content.

## Retention, consent, and backup

Your profile controls whether Docbank retains sanitized Markdown, provider
Markdown, and typed artifacts. Every rendition profile retains normalized
evidence, and embedding work retains vector sets. Because the preview lists the
classes for the selected profile, you do not have to infer retention from a
provider name.

Retained derivatives are catalog-authorized content. They follow the source
version's lifecycle, appear in catalog-authorized backups, and can keep related
source content reachable while the catalog still authorizes them. Physical
lexical and ANN indexes are rebuildable projections rather than backup
authority. Restoring a backup never calls a rendition or embedding provider, and
new provider work requires a new plan and consent.

Use the derivative-purge API only after running its own preview. A live purge
can remove selected live derivative heads, attachments, builds, artifacts,
segments, and vector sets, though it cannot rewrite immutable backup snapshots.
Removing Markdown alone does not establish that other retained derivative
classes or backup copies are gone.

Consent is scoped to the current principal, processing scope, profile, provider
disclosure, input classes, and retained classes, and it can be active, required,
expired, or revoked. API clients may grant a future expiry or revoke current
operator consent. The web app and API can revoke all processing consent for this
operator across profiles. A changed plan fingerprint, expired consent, or
revocation stops new egress and prevents publication under that authorization.

Next, configure a local profile in
[Document processing configuration](configuration.md), search an authorized
source set in [Automatic and processing search](search.md), and inspect the data
model in [Document derivatives](../architecture/document-derivatives.md).
