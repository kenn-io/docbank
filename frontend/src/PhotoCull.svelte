<script lang="ts">
  import { onDestroy, onMount, untrack, tick } from "svelte";
  import { restorePhotoReviewFocus } from "./photoReview.js";
  import "./photoReview.css";
  import { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";
  import { Button, Modal, Notice } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import type { PhotoCull } from "./photoCull.svelte.js";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import type {
    PhotoAuthoredPatch,
    PhotoAuthoredReceipt,
    PhotoAuthoredSnapshot,
    PhotoBrowseRow,
    Tag,
  } from "./generated/docbank.js";
  import { prefetchPreviews } from "./photoPreview.svelte.js";
  import PhotoStage from "./PhotoStage.svelte";
  import PhotoCell from "./PhotoCell.svelte";
  import { fittedPhotoView, toggleActualPhotoView, type PhotoView } from "./photoView.js";
  import { photoFacts } from "./photoFacts.js";
  import { cullKey } from "./shortcuts.js";
  let {
    cull,
    photos,
    cache,
    onclose,
    onmetadatachanged,
  }: {
    cull: PhotoCull;
    photos: Photos;
    cache: PhotoPreviewCache;
    onclose: () => void;
    onmetadatachanged?: (receipt: PhotoAuthoredReceipt, tags: Tag[]) => void;
  } = $props();
  const editor = untrack(
    () =>
      (photos.cullEditor ??= new PhotoAuthoredEditor(
        photos.session,
        photos.onauthfailure,
        photos.authored.undoState,
      )),
  );
  onDestroy(() => {
    cull.cancel();
    reloadController.abort();
  });
  let container = $state<HTMLDivElement>();
  const focus = () => restorePhotoReviewFocus(() => container);
  onMount(() => {
    void focus();
  });
  let photo = $state<PhotoBrowseRow>();
  let error = $state("");
  let loading = $state(false);
  let lookupAttempt = $state(0);
  const reloadController = new AbortController();
  let checkingDecision = $state(false);
  const busy = $derived(editor.loading || editor.busy || checkingDecision);
  let view = $state<PhotoView>({ ...fittedPhotoView });
  let info = $state(0);
  let help = $state(false);
  let announcement = $state("");
  const checkpoint = $derived(cull.run!.checkpoint);
  const rail = $derived(
    checkpoint.pass.slice(Math.max(0, checkpoint.position - 4), checkpoint.position + 5),
  );
  let neighbors = $state.raw<PhotoBrowseRow[]>([]);
  let railError = $state("");
  let railLoading = $state(false);
  const currentID = $derived(cull.currentID);
  $effect(() => {
    const id = currentID;
    void lookupAttempt;
    const controller = new AbortController();
    photo = undefined;
    error = "";
    loading = !!id;
    view = { ...fittedPhotoView };
    if (id)
      void photos
        .lookup(id, controller.signal)
        .then((row) => {
          if (!controller.signal.aborted) photo = row;
        })
        .catch((cause) => {
          if (controller.signal.aborted) return;
          error = cause instanceof Error ? cause.message : String(cause);
          if (cause instanceof APIError && cause.status === 404) cull.unavailable(id);
        })
        .finally(() => {
          if (!controller.signal.aborted) loading = false;
        });
    return () => controller.abort();
  });
  $effect(() => {
    const ids = rail.filter((id) => id !== currentID);
    const controller = new AbortController();
    railError = "";
    railLoading = true;
    void photos
      .lookupMany(ids, controller.signal)
      .then((rows) => {
        if (!controller.signal.aborted) neighbors = rows;
      })
      .catch((cause) => {
        if (!controller.signal.aborted)
          railError = cause instanceof Error ? cause.message : String(cause);
      })
      .finally(() => {
        if (!controller.signal.aborted) railLoading = false;
      });
    return () => controller.abort();
  });
  $effect(() => {
    const complete = cull.complete;
    const passID = checkpoint.passID;
    if (complete && passID) untrack(() => void cull.checkRemainder());
  });
  $effect(() =>
    prefetchPreviews(
      photos,
      cache,
      neighbors.filter((row) => row.asset_id !== currentID),
    ),
  );
  function canChangeTarget() {
    return editor.canChangeTarget();
  }
  async function nextPass(followUp = false) {
    if (canChangeTarget()) await cull.nextPass(followUp);
  }
  function move(direction: -1 | 1) {
    if (canChangeTarget()) cull.move(direction);
  }
  async function refresh(receipt: PhotoAuthoredReceipt, tags: Tag[]) {
    onmetadatachanged?.(receipt, tags);
    photos.metadataChanged(receipt);
    reconcile(receipt.after);
    if (cull.complete) await cull.checkRemainder();
  }
  function reconcile(targets: PhotoAuthoredSnapshot[]) {
    const updated = (row: PhotoBrowseRow) => {
      const after = targets.find((file) => file.file_id === row.display_file_id);
      return after
        ? {
            ...row,
            flag: after.values.flag,
            rating: after.values.rating,
            rotation: after.values.rotation,
          }
        : row;
    };
    if (photo) photo = updated(photo);
    neighbors = neighbors.map(updated);
  }
  async function decide(patch: PhotoAuthoredPatch) {
    if (!photo || loading || busy || cull.stale || cull.busy || !canChangeTarget()) return;
    editor.reset();
    await applyDecision(photo, patch);
  }
  async function applyDecision(selected: PhotoBrowseRow, patch: PhotoAuthoredPatch, retry = false) {
    checkingDecision = true;
    try {
      if (!(await cull.verifyDecision())) return;
    } finally {
      checkingDecision = false;
    }
    const id = selected.asset_id;
    await editor.edit({ asset_ids: [id] }, retry ? {} : patch, false, refresh, {
      viewed: retry
        ? undefined
        : {
            file_id: selected.display_file_id,
            content_version_id: selected.content_version_id,
          },
    });
    await tick();
    if (editor.error || editor.dirty || editor.conflict) return;
    reconcile(editor.targets);
    announcement =
      patch.flag !== undefined
        ? `${selected.name}: ${patch.flag || "flag cleared"}`
        : `${selected.name}: ${patch.rating} stars`;
    if (checkpoint.autoAdvance && cull.currentID === id) cull.move(1);
  }
  async function retryDecision() {
    if (!photo || busy || editor.loading) return;
    if (!editor.conflict) {
      await applyDecision(photo, { ...editor.patch }, true);
      return;
    }
    editor.loading = true;
    try {
      const row = await photos.lookup(photo.asset_id, reloadController.signal);
      if (reloadController.signal.aborted) return;
      photo = row;
      view = { ...fittedPhotoView };
      editor.reset();
    } catch (cause) {
      if (!reloadController.signal.aborted)
        editor.error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      editor.loading = false;
    }
  }
  function close() {
    if (canChangeTarget()) onclose();
  }
  function key(event: KeyboardEvent) {
    const action = cullKey(event);
    if (action === undefined) return;
    event.preventDefault();
    event.stopPropagation();
    if (action === "close") close();
    else if (action === "previous") move(-1);
    else if (action === "next") move(1);
    else if (action === "auto") {
      if (!busy && !cull.busy && !cull.stale) cull.toggleAutoAdvance();
    } else if (action === "scope") void nextPass();
    else if (action === "zoom") view = toggleActualPhotoView(view);
    else if (action === "info") info = (info + 1) % 3;
    else if (action === "help") help = !help;
    else if (action === "pick") void decide({ flag: "pick" });
    else if (action === "reject") void decide({ flag: "reject" });
    else if (action === "unflag") void decide({ flag: "" });
    else if (typeof action === "number") void decide({ rating: action });
  }
</script>

<svelte:document onkeydown={(event) => event.target === document.body && key(event)} />

<div class="review-shell">
  <Modal
    ariaLabel="Cull photos"
    closable={false}
    closeOnOverlayClick={false}
    onclose={close}
    width="100vw"
    maxWidth="100vw"
    maxHeight="100svh"
  >
    <!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_static_element_interactions -->
    <div class="photo-review photo-cull" tabindex="-1" bind:this={container} onkeydown={key}>
      <header>
        <Button size="sm" disabled={busy} onclick={close}>Exit cull</Button>
        <strong>{photo?.name ?? (cull.complete ? "Pass complete" : "Cull photos")}</strong>
        <span
          >{Math.min(checkpoint.position + 1, checkpoint.pass.length)} of {checkpoint.pass
            .length}</span
        >
        <Button size="sm" disabled={cull.busy || cull.stale || busy} onclick={() => void nextPass()}
          >Scope: {checkpoint.scope}</Button
        >
        <Button
          size="sm"
          disabled={cull.stale || cull.busy || busy}
          onclick={() => cull.toggleAutoAdvance()}
          >Auto-advance: {checkpoint.autoAdvance ? "on" : "off"}</Button
        >
        <Button size="sm" onclick={() => (help = !help)}>Keys (?)</Button>
      </header>
      {#if cull.error || editor.error || editor.notice}<p role="alert">
          {cull.error ||
            (editor.noticeTone === "warning" && editor.notice) ||
            editor.error ||
            editor.notice}
        </p>{/if}
      {#if cull.progress && !cull.complete}<p role="status">{cull.progress}</p>{/if}
      {#if editor.dirty || editor.conflict}<div class="controls">
          <Button disabled={busy} onclick={() => void retryDecision()}
            >{editor.conflict ? "Reload photo" : "Retry edit"}</Button
          >
          <Button
            disabled={busy || editor.loading}
            onclick={() => {
              editor.clearDraft();
              editor.reset();
            }}>Discard edit</Button
          >
        </div>{/if}
      {#if help}<p class="key-map">
          ← / ↑ previous · → / ↓ next · P pick · X reject · U clear flag · 0–5 rating · A
          auto-advance · S next scope · Z 1:1 · I EXIF · ? keys · Esc exit
        </p>{/if}
      {#if cull.complete}
        <section class="completion">
          <h2>Pass complete</h2>
          <p>
            {checkpoint.visited.length} viewed · {checkpoint.skipped.length} skipped · {checkpoint
              .unavailable.length} unavailable
          </p>
          {#if cull.progress}<p role="status">{cull.progress}</p>{/if}
          {#if cull.remainder}<Button
              disabled={cull.busy || cull.stale || busy}
              onclick={() => void nextPass(true)}>Review unflagged remainder</Button
            >{/if}
          <Button disabled={busy || cull.busy || cull.stale} onclick={() => move(-1)}
            >Previous</Button
          >
          <Button disabled={busy} onclick={close}>Back to photos</Button>
        </section>
      {:else}
        <div class="body">
          {#if photo}<PhotoStage
              {photo}
              {photos}
              {cache}
              {view}
              onfocus={focus}
              onreload={(row) => (photo = row)}
              onunavailable={(message) => {
                cull.unavailable(photo!.asset_id);
                photo = undefined;
                error = message;
              }}
              onview={(value) => (view = value)}
              onmove={move}
            />
          {:else}<div class="unavailable" role="status">
              {loading ? "Loading photo…" : error}
              {#if !loading && error}<Button disabled={busy} onclick={() => lookupAttempt++}
                  >Retry</Button
                >{/if}
              <Button disabled={busy} onclick={() => move(1)}>Next photo</Button>
            </div>{/if}
          {#if info && photo}<aside aria-label="Photo facts">
              <dl>
                {#each photoFacts(photo).slice(0, info === 1 ? 5 : undefined) as [label, value]}<dt>
                    {label}
                  </dt>
                  <dd>{value}</dd>{/each}
              </dl>
            </aside>{/if}
        </div>
        <div class="controls">
          <Button size="sm" disabled={checkpoint.position === 0 || busy} onclick={() => move(-1)}
            >Previous</Button
          >
          <Button
            size="sm"
            disabled={!photo || cull.stale || busy}
            onclick={() => void decide({ flag: "pick" })}>Pick (P)</Button
          >
          <Button
            size="sm"
            disabled={!photo || cull.stale || busy}
            onclick={() => void decide({ flag: "reject" })}>Reject (X)</Button
          >
          <Button
            size="sm"
            disabled={!photo || cull.stale || busy}
            onclick={() => void decide({ flag: "" })}>Clear flag (U)</Button
          >
          <Button
            size="sm"
            disabled={cull.stale || busy}
            onclick={() => {
              if (canChangeTarget()) cull.skip();
            }}>Skip</Button
          >
          <span>{photo ? `${photo.flag || "Unflagged"} · ${photo.rating} stars` : ""}</span>
          <Button size="sm" onclick={() => (view = toggleActualPhotoView(view))}>1:1 (Z)</Button>
          <Button size="sm" onclick={() => (info = (info + 1) % 3)}>EXIF (I)</Button>
          <Button size="sm" disabled={cull.stale || busy} onclick={() => move(1)}>Next</Button>
        </div>
        {#if railError}<p role="alert">{railError}</p>{/if}
        <nav class="filmstrip" aria-label="Cull pass">
          <fieldset disabled={busy} style="display: contents">
            {#each rail as id}{@const row =
                id === currentID ? photo : neighbors.find((item) => item.asset_id === id)}
              <div class:current={id === cull.currentID} class="thumb">
                {#if row}<PhotoCell
                    photo={row}
                    selected={id === cull.currentID}
                    compact={true}
                    onclick={() => {
                      if (canChangeTarget()) cull.choose(id);
                    }}
                    {cache}
                    onopen={() => {
                      if (canChangeTarget()) cull.choose(id);
                    }}
                  />{:else}<Button
                    size="sm"
                    onclick={() => {
                      if (canChangeTarget()) cull.choose(id);
                    }}
                    >{railLoading ? "Loading…" : railError ? "Retry photo" : "Unavailable"}</Button
                  >{/if}
              </div>{/each}
          </fieldset>
        </nav>
      {/if}
      {#if editor.receiptID}<Notice
          tone="success"
          message={editor.undoState.summary}
          actionLabel={cull.stale || busy || editor.dirty ? undefined : "Undo"}
          onaction={() => void editor.undo(refresh)}
        />{/if}
      <span class="kit-sr-only" aria-live="polite">{announcement}</span>
    </div>
  </Modal>
</div>

<style>
  header,
  .controls {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 12px;
    padding: 12px;
    background: var(--bg-surface);
  }
  header strong {
    flex: 1;
  }
  .body {
    display: flex;
    flex: 1;
    min-height: 0;
  }
  aside {
    width: 260px;
    padding: 16px;
    overflow: auto;
  }
  dt {
    color: var(--text-muted);
    margin-top: 12px;
  }
  dd {
    margin: 4px 0;
  }
  .filmstrip {
    flex-shrink: 0;
    display: flex;
    gap: 8px;
    justify-content: center;
    padding: 12px;
    overflow: auto;
  }
  .thumb {
    width: 88px;
    height: 66px;
    flex-shrink: 0;
  }
  .current {
    outline: 2px solid var(--accent-blue);
  }
  .completion,
  .unavailable {
    margin: auto;
    padding: 24px;
  }
  .key-map {
    padding: 12px;
    margin: 0;
  }
  p[role="alert"] {
    padding: 8px;
  }
  @media (max-width: 760px) {
    aside {
      width: 180px;
    }
    .filmstrip {
      justify-content: start;
    }
  }
</style>
