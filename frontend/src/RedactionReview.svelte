<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { Disclosure, Button, Modal, SelectDropdown, Spinner, Toggle } from "@kenn-io/kit-ui";
  import RedactionNumbering from "./RedactionNumbering.svelte";
  import { RecipientPackageController } from "./recipientPackageController.svelte.js";
  import type { NumberingInputs } from "./redactionNumbering.js";
  import OriginalPreview from "./OriginalPreview.svelte";
  import type { PDFReviewBridge, ReaderReviewStatus, TextSelectionStatus } from "./pdfReview.js";
  import { checkMaskBudget, maskBoxes } from "./pdfTextSelection.js";
  import PageCanvas from "./PageCanvas.svelte";
  import { pageBinding } from "./pages.js";
  import { APIError } from "./api-transport.js";
  import {
    getNode,
    discardRedactionPreviews,
    type RedactionPreparation,
    type RedactionMask,
    type RedactionReview,
    type RedactionPreview,
    type PDFRedactionBox,
    type PDFRedactionSelector,
  } from "./generated/docbank.js";
  import { randomUUID } from "./crypto.js";
  import { offerPreparedDownload } from "./download.js";
  import {
    prepareRedaction,
    resolveRedaction,
    decodeExpansion,
    discardRedaction,
  } from "./redaction.js";
  import {
    createRedactionPreview,
    readRedactionImage,
    readRedactionText,
    redactionTicket,
  } from "./redactionPreview.js";
  import type { SelectedSource } from "./selectedSource.js";
  let {
    session,
    source,
    authorizationRevision = $bindable(),
    onclose,
    onauthfailure,
    active = false,
    onworkspaceclose,
    onpublished = () => {},
    onproductions,
  }: {
    onpublished?: () => void;
    onproductions?: () => void;
    session: string;
    source: SelectedSource;
    authorizationRevision: number;
    onclose: () => void;
    onworkspaceclose: () => void;
    active?: boolean;
    onauthfailure: (cause: unknown) => void;
  } = $props();
  let phase = $state<
    "preparing" | "editing" | "resolving" | "rendering" | "releasing" | "release-failed" | "ready"
  >("preparing");
  let preparation = $state<RedactionPreparation>();
  let masks = $state<RedactionMask[]>([]);
  let textGroups = $state<{ ids: string[]; page: number }[]>([]);
  let textSelection = $state<TextSelectionStatus>({ busy: false, error: "" });
  let selectionAnnouncement = $state("");
  let review = $state<RedactionReview>();
  let numberingReviews = $state<{ inputs: NumberingInputs; published: boolean }[]>([]);
  const packageController = new RecipientPackageController(
    () => session,
    (cause) => onauthfailure(cause),
  );
  const latestNumbering = $derived(numberingReviews.at(-1));
  let numberingLocked = $state(false);
  let preview = $state<RedactionPreview>();
  let expansion = $state<RedactionMask>();
  let error = $state("");
  let notice = $state("");
  let page = $state(1),
    zoom = $state("page");
  let tab = $state<"original" | "redacted" | "text">("original");
  let drawing = $state(false),
    dragging = $state(false),
    downloading = $state(false);
  let dialog = $state<"discard" | "download">();
  let title = $state(untrack(() => source.name));
  let readerStatus = $state<ReaderReviewStatus>({ page: 1, ready: false, error: "" });
  let bridge = $state<PDFReviewBridge>();
  let closeWorkspace = false;
  let generation = 0,
    reviewing = false;
  let outputURL = $state(""),
    outputText = $state(""),
    outputLoading = $state(false);
  let controller = new AbortController(),
    imageController = new AbortController();
  let disposed = false,
    expiry: ReturnType<typeof setTimeout> | undefined;
  const busy = $derived(
    phase === "preparing" ||
      phase === "resolving" ||
      phase === "rendering" ||
      phase === "releasing" ||
      downloading,
  );
  const currentPage = $derived(preparation?.map.pages[page - 1]);
  const imageReady = $derived(readerStatus.ready);
  const boxes = $derived.by(() => {
    if (!preparation) return [];
    if (preview && review) return review.resolved.redact_boxes;
    return masks.flatMap((m) =>
      maskBoxes(preparation!, m.selector).map((box) => ({
        ...box,
        outline: m.selector.kind !== "text",
      })),
    );
  });
  const maskItems = $derived(
    masks.flatMap((mask) => {
      const group = textGroups.find((g) => g.ids.includes(mask.id));
      return group && group.ids[0] !== mask.id ? [] : [{ mask, group }];
    }),
  );
  const expansionBoxes = $derived(
    preparation && expansion ? maskBoxes(preparation, expansion.selector) : [],
  );
  const expansionKind = $derived(masks.find((m) => m.id === expansion?.id)?.selector.kind);
  const gapPages = $derived([...new Set(preparation?.map.gaps.map((g) => g.box.page) ?? [])]);
  function clearImage() {
    imageController.abort();
    imageController = new AbortController();
    if (outputURL) URL.revokeObjectURL(outputURL);
    outputURL = "";
    outputText = "";
    outputLoading = false;
  }
  function fail(cause: unknown) {
    if (disposed) return;
    if (cause instanceof APIError && cause.status === 401) {
      dispose();
      onauthfailure(cause);
      return;
    }
    if (cause instanceof APIError && cause.status === 410) {
      clearTimeout(expiry);
      preview = undefined;
      review = undefined;
      tab = "original";
      phase = "editing";
      dialog = undefined;
      void showPage();
    }
    error = cause instanceof Error ? cause.message : String(cause);
  }
  function dispose() {
    if (disposed) return;
    disposed = true;
    endReview();
  }
  function close() {
    endReview();
    if (closeWorkspace) onworkspaceclose();
    else onclose();
  }
  // A containing reader delegates dismissal here when this review is open.
  export function requestClose(workspace = false) {
    closeWorkspace = workspace;
    if (masks.length || busy || preview) dialog = "discard";
    else close();
  }
  async function showPage() {
    clearImage();
    if (disposed || !preparation) return;
    if (tab === "original") {
      bridge?.goToPage(page);
      return;
    }
    const output = preview,
      selectedPage = page,
      signal = imageController.signal;
    if (!output) return;
    outputLoading = true;
    try {
      if (tab === "text") {
        const text = await readRedactionText(session, output, selectedPage, signal);
        if (signal.aborted || disposed) return;
        const expected = review?.resolved.runs
          .filter((r) => r.page === selectedPage)
          .map((r) => r.text)
          .join("");
        if (text !== expected)
          throw new Error("Sanitized text disagrees with the reviewed result.");
        outputText = text;
      } else {
        const url = await readRedactionImage(session, output, selectedPage, signal);
        if (signal.aborted || disposed) {
          URL.revokeObjectURL(url);
          return;
        }
        outputURL = url;
      }
    } catch (cause) {
      if (!signal.aborted) fail(cause);
    } finally {
      if (!signal.aborted) outputLoading = false;
    }
  }
  function changePage(n: number) {
    if (busy) return;
    dragging = false;
    page = n;
    void showPage();
  }
  function changeTab(value: typeof tab) {
    drawing = false;
    dragging = false;
    tab = value;
    void showPage();
  }
  function changed() {
    if (!numberingLocked) numberingReviews = numberingReviews.filter((item) => item.published);
    review = undefined;
    expansion = undefined;
    error = "";
    notice = "";
  }
  function addMask(selector: PDFRedactionSelector) {
    if (busy || numberingLocked || preview || !preparation) return;
    const addition = { id: "00000000-0000-4000-8000-000000000001", selector };
    try {
      checkMaskBudget([...masks, addition]);
      addition.id = randomUUID();
      changed();
      masks = [...masks, addition];
    } catch (cause) {
      fail(cause);
    }
  }
  function addBox(box: PDFRedactionBox) {
    addMask({ kind: "rectangle", map_sha256: preparation!.map.sha256, boxes: [box] });
  }
  function addSelectedText() {
    const candidate = bridge?.selectedText();
    if (!candidate || !preparation || busy || numberingLocked || preview) return;
    try {
      // Admit with UUID-sized placeholders before allocating the mask IDs.
      const additions: RedactionMask[] = candidate.spans.map((span) => ({
        id: "00000000-0000-4000-8000-000000000001",
        selector: { kind: "text", map_sha256: preparation!.map.sha256, span: { ...span } },
      }));
      checkMaskBudget([...masks, ...additions]);
      additions.forEach((m) => (m.id = randomUUID()));
      changed();
      masks = [...masks, ...additions];
      textGroups = [...textGroups, { ids: additions.map((m) => m.id), page: candidate.page }];
      bridge?.clearSelection();
      selectionAnnouncement = `Text mask ${maskItems.length} added on page ${candidate.page}.`;
    } catch (cause) {
      fail(cause);
    }
  }
  function maskPage(pageNumber: number) {
    if (!preparation) return;
    addMask({ kind: "page", map_sha256: preparation.map.sha256, pages: [pageNumber] });
  }
  async function revalidate(signal: AbortSignal) {
    const node = await getNode(source.nodeID, { session, signal });
    signal.throwIfAborted();
    if (
      node.kind !== "file" ||
      node.trashed_at ||
      node.current_version_id !== source.versionID ||
      node.blob_hash !== source.blobHash ||
      node.size !== source.size
    )
      throw new Error("The source changed. Close this review and reopen the intended PDF.");
    authorizationRevision = Math.max(authorizationRevision, node.revision);
    title = node.name;
  }
  async function renderPreview() {
    if (
      !preparation ||
      !masks.length ||
      busy ||
      numberingLocked ||
      dragging ||
      phase === "release-failed"
    )
      return;
    controller.abort();
    controller = new AbortController();
    const signal = controller.signal,
      current = generation;
    error = "";
    notice = "";
    expansion = undefined;
    drawing = false;
    phase = "resolving";
    const prep = preparation,
      selectedMasks = structuredClone($state.snapshot(masks));
    let creationAttempted = false;
    try {
      await revalidate(signal);
      const resolved = await resolveRedaction(session, prep, selectedMasks, signal);
      if (signal.aborted || disposed) return;
      review = resolved;
      await revalidate(signal);
      if (signal.aborted || disposed) return;
      phase = "rendering";
      creationAttempted = true;
      const result = await createRedactionPreview(session, prep, selectedMasks, resolved, signal);
      if (signal.aborted || disposed) return;
      if (!numberingReviews.some((item) => item.inputs.review.sha256 === resolved.sha256)) {
        numberingReviews.push({
          inputs: structuredClone({
            preparation: $state.snapshot(prep),
            masks: selectedMasks,
            review: resolved,
          }),
          published: false,
        });
      }
      preview = result;
      phase = "ready";
      tab = "redacted";
      // Server expiry is authoritative. Local time only schedules a release;
      // keep the handle until discard succeeds, even if the clocks disagree.
      expiry = setTimeout(
        () => {
          if (preview?.id !== result.id) return;
          void back(true);
        },
        30 * 60 * 1000,
      );
      await showPage();
    } catch (cause) {
      if (signal.aborted || disposed) return;
      if (cause instanceof APIError && cause.code === "selection_expansion_required") {
        try {
          expansion = decodeExpansion(cause.redaction, prep, selectedMasks);
        } catch (invalid) {
          fail(invalid);
        }
      } else fail(cause);
    } finally {
      if (!disposed && current === generation && !preview) {
        if (creationAttempted) await releaseUnreceivedPreview();
        else phase = "editing";
      }
    }
  }
  function cancelRender() {
    controller.abort();
    notice = "Preview canceled.";
    if (!preparation) close();
  }
  async function releaseUnreceivedPreview() {
    if (disposed) return;
    // Creation may have completed even when its response was lost. A fresh
    // signal lets owner-scoped cleanup finish before another create is allowed.
    controller = new AbortController();
    const current = generation;
    phase = "releasing";
    try {
      await discardRedactionPreviews({ session, signal: controller.signal });
      if (disposed || current !== generation) return;
      review = undefined;
      phase = "editing";
    } catch (cause) {
      if (disposed || current !== generation) return;
      fail(cause);
      if (!disposed) phase = "release-failed";
    }
  }
  async function back(expired = false) {
    if (!preview) return;
    const old = preview;
    clearTimeout(expiry);
    try {
      await discardRedaction(session, old);
    } catch (cause) {
      fail(cause);
      return;
    }
    if (disposed || preview?.id !== old.id) return;
    preview = undefined;
    review = undefined;
    phase = "editing";
    tab = "original";
    notice = expired ? "Preview expired. Create a new preview before downloading." : "";
    void showPage();
  }
  async function download() {
    dialog = undefined;
    if (!preview) return;
    const output = preview;
    downloading = true;
    error = "";
    try {
      const url = await redactionTicket(session, output, controller.signal);
      if (disposed || preview?.id !== output.id) return;
      offerPreparedDownload({ url, name: `${title.replace(/\.pdf$/i, "")}-redacted.zip` });
      notice = "Download started. Keep this tab open until the download finishes.";
    } catch (cause) {
      if (!controller.signal.aborted) fail(cause);
    } finally {
      downloading = false;
    }
  }
  function savePrivate(kind: "preparation" | "masks" | "review") {
    const saved = review && preparation ? { preparation, masks, review } : latestNumbering?.inputs;
    if (!saved) return;
    const value = saved[kind];
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(value)], { type: "application/json" }),
    );
    // eslint-disable-next-line no-control-regex -- Strip unsafe download filename characters.
    const stem = title.replace(/\.pdf$/i, "").replace(/[<>:"/\\|?*\u0000-\u001f]/g, "_");
    offerPreparedDownload({ url, name: `${stem}-PRIVATE-${kind}.json` });
    setTimeout(() => URL.revokeObjectURL(url), 0);
  }
  function endReview() {
    if (!reviewing) return;
    reviewing = false;
    void packageController.stop();
    generation++;
    controller.abort();
    clearImage();
    clearTimeout(expiry);
    void discardRedactionPreviews({ session, keepalive: true }).catch(() => undefined);
    bridge?.clearSelection();
    masks = [];
    textGroups = [];
    textSelection = { busy: false, error: "" };
    selectionAnnouncement = "";
    preparation = undefined;
    numberingReviews = [];
    numberingLocked = false;
    review = undefined;
    preview = undefined;
    expansion = undefined;
    drawing = false;
    dragging = false;
    tab = "original";
    notice = "";
    error = "";
    dialog = undefined;
  }
  $effect(() => {
    if (!active) return;
    untrack(() => {
      reviewing = true;
      closeWorkspace = false;
      controller = new AbortController();
      phase = "preparing";
      const signal = controller.signal;
      void (async () => {
        await revalidate(signal);
        const prep = await prepareRedaction(
          session,
          pageBinding(source, authorizationRevision),
          signal,
        );
        if (disposed || signal.aborted) return;
        preparation = prep;
        phase = "editing";
      })().catch((cause) => {
        if (!signal.aborted && !disposed) {
          fail(cause);
          phase = "editing";
        }
      });
    });
    return () => untrack(endReview);
  });
  onMount(() => {
    const escape = (e: KeyboardEvent) => {
      if (e.key === "Escape" && dialog) {
        e.preventDefault();
        e.stopPropagation();
        dialog = undefined;
      }
    };
    const hide = () => {
      dispose();
      onworkspaceclose();
    };
    window.addEventListener("pagehide", hide);
    window.addEventListener("keydown", escape, true);
    return () => {
      window.removeEventListener("pagehide", hide);
      window.removeEventListener("keydown", escape, true);
      dispose();
    };
  });
</script>

<div class="review-workspace" class:active>
  <div class="original-reader" hidden={active && tab !== "original"}>
    <OriginalPreview
      {session}
      {source}
      {authorizationRevision}
      onauthfailure={fail}
      expanded
      review={active
        ? {
            preparation,
            boxes,
            gaps: preparation?.map.gaps ?? [],
            proposed: expansionBoxes,
            drawing: drawing && !busy && !numberingLocked && !preview && !dialog,
            visible: tab === "original",
            editable: phase === "editing" && !dialog && !preview && !busy && !numberingLocked,
            oncommit: addBox,
            ondrag: (value) => untrack(() => (dragging = value)),
            ondrawing: (value) => untrack(() => (drawing = value)),
            onselection: (value) =>
              untrack(() => {
                textSelection = value;
                selectionAnnouncement = "";
              }),
          }
        : undefined}
      onbridge={(value) => (bridge = value)}
      onstatus={(status) => {
        readerStatus = status;
        if (tab === "original") page = status.page;
      }}
    />
  </div>
  {#if active}
    <div class="redaction-editor">
      <span class="kit-sr-only" role="status" aria-live="polite">{selectionAnnouncement}</span>
      {#if textSelection.error}<p class="notice" role="status">{textSelection.error}</p>{/if}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      {#if notice}<p class="notice" role="status">{notice}</p>{/if}
      {#if phase === "preparing"}<p role="status"><Spinner size={16} /> Preparing PDF…</p>
        <Button onclick={close}>Cancel preparation</Button>{/if}
      {#if preparation && currentPage}
        {#if tab !== "original"}<div class="toolbar">
            <nav aria-label="Redaction pages">
              <Button size="sm" disabled={page === 1 || busy} onclick={() => changePage(page - 1)}
                >Previous page</Button
              >
              <SelectDropdown
                title="Page"
                disabled={busy}
                value={String(page)}
                options={preparation.map.pages.map((p) => ({
                  value: String(p.number),
                  label: `${p.number} of ${preparation!.map.pages.length}${gapPages.includes(p.number) ? " · gaps" : ""}`,
                }))}
                onchange={(v) => changePage(Number(v))}
              />
              <Button
                size="sm"
                disabled={page === preparation.frames.length || busy}
                onclick={() => changePage(page + 1)}>Next page</Button
              >
            </nav>
            <SelectDropdown
              title="Zoom"
              value={zoom}
              options={[
                { value: "page", label: "Fit page" },
                { value: "fit", label: "Fit width" },
                ...[50, 100, 200].map((n) => ({ value: String(n), label: `${n}%` })),
              ]}
              onchange={(v) => {
                zoom = v;
                dragging = false;
              }}
            />
          </div>
        {/if}
        <div class="tabs" role="group" aria-label="Redaction view">
          {#each ["original", "redacted", "text"] as value}<Button
              size="sm"
              disabled={busy || (value !== "original" && !preview)}
              surface={tab === value ? "solid" : "soft"}
              onclick={() => changeTab(value as typeof tab)}
              >{value === "original"
                ? "Original"
                : value === "redacted"
                  ? "Redacted"
                  : "Text"}</Button
            >{/each}
        </div>
        <div class="mask-details">
          <section class="canvas-column" aria-label="Redaction page">
            {#if tab === "original"}
              <p class="notice">
                Original is rendered in your browser. Check Redacted before downloading.
              </p>
              {#if readerStatus.error}<p class="error" role="alert">{readerStatus.error}</p>
                <Button onclick={() => bridge?.retry(page)}>Retry page</Button>
              {:else if !imageReady}<p role="status">
                  <Spinner size={16} /> Rendering page {page}…
                </p>{/if}
            {/if}
            {#if drawing}<p id="mask-keyboard" class="notice">
                Drag a rectangle, or use arrows (Shift: 0.1 inch), Enter for each corner, Escape to
                cancel.
              </p>{/if}
          </section>
          <aside aria-label="Masks and coverage">
            <h3>Masks <span>{maskItems.length}</span></h3>
            {#if masks.length === 0}<p class="notice">No masks</p>{/if}
            <ol>
              {#each maskItems as { mask, group }, index (mask.id)}<li>
                  <span
                    >{#if group}Selected text · Page {group.page}
                    {:else if mask.selector.kind === "page"}Whole page {mask.selector.pages?.join(
                        ", ",
                      )}
                    {:else}Rectangle {index + 1} · Page {[
                        ...new Set(mask.selector.boxes?.map((b) => b.page)),
                      ].join(", ")}{/if}</span
                  ><Button
                    size="sm"
                    disabled={busy || numberingLocked || !!preview}
                    onclick={() => {
                      changed();
                      masks = masks.filter((m) => !(group?.ids ?? [mask.id]).includes(m.id));
                      textGroups = textGroups.filter((g) => g !== group);
                    }}>Remove</Button
                  >
                </li>{/each}
            </ol>
            {#if gapPages.length}<h3>Coverage gaps</h3>
              <p class="notice">Cover each outlined gap. Unordered gaps need a whole-page mask.</p>
              <div class="gap-pages">
                {#each gapPages as n}<Button size="sm" onclick={() => changePage(n)}
                    >Page {n}{preparation.map.gaps.some((g) => g.box.page === n && g.unordered)
                      ? " · whole page"
                      : ""}</Button
                  >{/each}
              </div>{/if}
            {#if expansion}<section class="expansion" aria-label="Proposed mask expansion">
                <h3>Expand mask?</h3>
                <p>
                  {#if expansionKind === "text"}
                    The safety margin reaches nearby text. Review the added area before continuing.
                  {:else}
                    The selected region cuts through text. Review the full region before continuing.
                  {/if}
                </p>
                <div class="gap-pages">
                  {#each [...new Set(expansionBoxes.map((b) => b.page))] as n}
                    <Button size="sm" onclick={() => changePage(n)}>Page {n}</Button>
                  {/each}
                </div>
                <Button
                  size="sm"
                  tone="info"
                  onclick={() => {
                    const proposal = expansion!;
                    masks = masks.map((m) => (m.id === proposal.id ? proposal : m));
                    changed();
                  }}>Use expanded mask</Button
                ><Button size="sm" onclick={() => (expansion = undefined)}>Keep editing</Button>
              </section>{/if}
          </aside>
        </div>
        {#if review || latestNumbering}<Disclosure variant="plain" title="Private review files">
            <p>
              Keep these for offline verification. Preparation contains unredacted evidence; do not
              include it with the delivery.
            </p>
            <div class="actions">
              {#each ["preparation", "masks", "review"] as kind}<Button
                  size="sm"
                  onclick={() => savePrivate(kind as "preparation" | "masks" | "review")}
                  >Save {kind}</Button
                >{/each}
              {#if !latestNumbering?.published}<p class="notice">
                  Save all three before numbering to finish through the CLI if this tab closes.
                </p>{/if}
            </div>
          </Disclosure>{/if}
        {#each numberingReviews as item (item.inputs.review.sha256)}
          <RedactionNumbering
            {session}
            inputs={item.inputs}
            {title}
            {packageController}
            bind:locked={numberingLocked}
            {onauthfailure}
            onpublished={() => {
              item.published = true;
              onpublished();
            }}
            onnotice={(message) => (notice = message)}
            {onproductions}
          />
        {/each}
        {#if preview}<p class="notice">
            Preview expires {new Date(preview.expires_at).toLocaleTimeString()}. It is removed on
            tab close, reload or daemon restart.
          </p>{/if}
      {/if}
    </div>
    {#if tab !== "original" && preparation && currentPage}<div class="review-output">
        {#if outputLoading}<p role="status"><Spinner size={16} /> Verifying preview…</p>
        {:else if tab === "text"}<pre
            aria-label={`Sanitized text for page ${page}`}>{outputText}</pre>
        {:else if outputURL}<PageCanvas
            url={outputURL}
            width={currentPage.width}
            height={currentPage.height}
            {zoom}
            maxHeight="min(65vh, 800px)"
            label={`Verified redacted page ${page} of ${title}`}
          />{/if}
      </div>{/if}
    <div class="review-footer">
      <div class="footer actions">
        <Button onclick={() => requestClose()}>Back to reading</Button>
        {#if phase === "resolving" || phase === "rendering"}<span role="status"
            ><Spinner size={16} />{phase === "resolving"
              ? "Resolving masks…"
              : "Rendering and verifying PDF…"}</span
          ><Button onclick={cancelRender}>Cancel preview</Button>
        {:else if phase === "releasing" || phase === "release-failed"}
          {#if phase === "releasing"}<span role="status"
              ><Spinner size={16} />Releasing preview…</span
            >{/if}
          <Button disabled={phase === "releasing"} onclick={() => void releaseUnreceivedPreview()}
            >Release preview</Button
          >
        {:else if preview}<Button
            disabled={downloading || numberingLocked}
            onclick={() => void back()}>Back to masks</Button
          ><Button disabled={downloading || outputLoading} onclick={() => (dialog = "download")}
            >Download unnumbered ZIP</Button
          >
        {:else if preparation}<Toggle
            disabled={busy || numberingLocked || !imageReady || masks.length >= 256}
            label="Draw rectangle"
            bind:checked={drawing}
            onchange={() => {
              dragging = false;
              if (drawing) bridge?.focus();
            }}
          /><Button
            disabled={busy ||
              numberingLocked ||
              drawing ||
              !imageReady ||
              !textSelection.candidate ||
              textSelection.busy}
            onclick={addSelectedText}
            >{textSelection.busy ? "Matching text…" : "Mask selected text"}</Button
          >{#each [page] as pageNumber (pageNumber)}<Button
              disabled={busy || numberingLocked || !imageReady || masks.length >= 256}
              onclick={() => maskPage(pageNumber)}>Mask whole page {pageNumber}</Button
            >{/each}<Button
            tone="info"
            surface="solid"
            disabled={busy ||
              numberingLocked ||
              dragging ||
              textSelection.busy ||
              masks.length === 0}
            onclick={() => void renderPreview()}>Preview redaction</Button
          >{/if}
      </div>
    </div>
  {/if}
</div>
{#if dialog === "discard"}<Modal title="Discard redaction?" onclose={() => (dialog = undefined)}
    ><p>The masks and temporary preview will be removed. The original is unchanged.</p>
    {#if numberingLocked}<p>
        Any reserved numbers and retained original remain. Keep the private review files to finish
        publication through the CLI.
      </p>{/if}
    {#snippet footer()}<Button onclick={() => (dialog = undefined)}>Keep reviewing</Button><Button
        tone="danger"
        onclick={close}>Discard and close</Button
      >{/snippet}</Modal
  >
{:else if dialog === "download" && preview}<Modal
    title="Download reviewed PDF?"
    onclose={() => (dialog = undefined)}
    ><p>{title} · {preview.pages.length} pages · {maskItems.length} masks</p>
    <p>Downloads this reviewed result. The original is unchanged.</p>
    {#snippet footer()}<Button onclick={() => (dialog = undefined)}>Keep reviewing</Button><Button
        tone="info"
        surface="solid"
        onclick={() => void download()}>Download ZIP</Button
      >{/snippet}</Modal
  >{/if}

<style>
  .review-workspace {
    position: relative;
    display: grid;
    grid-template-rows: minmax(0, 1fr);
    height: 100%;
    min-height: 0;
    min-width: 0;
  }
  .review-workspace.active {
    grid-template-columns: minmax(0, 1fr) 290px;
    grid-template-rows: minmax(0, 1fr) auto;
    gap: var(--space-3);
  }
  .original-reader {
    min-width: 0;
    min-height: 0;
    grid-column: 1;
    grid-row: 1;
  }
  .original-reader[hidden] {
    visibility: hidden;
    display: block;
  }
  .redaction-editor {
    grid-column: 2;
    grid-row: 1;
    overflow: auto;
    align-content: start;
    padding: var(--space-3);
    display: grid;
    gap: var(--space-4);
    min-width: 0;
  }
  .review-output {
    grid-column: 1;
    grid-row: 1;
    min-width: 0;
    min-height: 0;
    overflow: auto;
    background: var(--bg-inset);
  }
  .review-footer {
    grid-column: 1/-1;
  }
  .toolbar,
  nav,
  .tabs,
  .actions,
  .gap-pages {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .toolbar {
    justify-content: space-between;
  }
  .mask-details {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-5);
    align-items: start;
  }
  .canvas-column {
    min-width: 0;
  }
  aside {
    display: grid;
    gap: var(--space-3);
    font-size: var(--font-size-sm);
  }
  h3 {
    margin: 0;
    font-size: var(--font-size-sm);
    font-weight: 600;
  }
  h3 span,
  .notice {
    color: var(--text-muted);
  }
  ol {
    padding: 0;
    list-style: none;
    margin: 0;
    display: grid;
    gap: var(--space-2);
  }
  li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }
  p {
    margin: 0;
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }
  .error {
    color: var(--status-error-text);
  }
  .expansion {
    display: grid;
    gap: var(--space-3);
    border: 1px solid var(--border-default);
    padding: var(--space-3);
  }
  .footer {
    padding: var(--space-4) var(--space-6);
    justify-content: flex-end;
  }
  .footer span {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-right: auto;
    font-size: var(--font-size-sm);
  }
  pre {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    padding: var(--space-4);
    background: var(--bg-inset);
    font-size: var(--font-size-sm);
  }
  @media (max-width: 760px) {
    .review-workspace.active {
      grid-template-columns: minmax(0, 1fr);
      grid-template-rows: minmax(200px, 1fr) minmax(180px, 0.8fr) auto;
      overflow-y: auto;
    }
    .redaction-editor {
      grid-column: 1;
      grid-row: 2;
      overflow-y: scroll;
      scrollbar-gutter: stable;
      scrollbar-width: thin;
      scrollbar-color: var(--text-muted) transparent;
    }
    .review-footer {
      grid-row: 3;
      position: sticky;
      bottom: 0;
      z-index: 1;
      background: var(--bg-surface);
    }
    .mask-details {
      grid-template-columns: minmax(0, 1fr);
    }
    .redaction-editor {
      padding: var(--space-3);
    }
    .footer {
      padding: var(--space-3);
    }
  }
</style>
