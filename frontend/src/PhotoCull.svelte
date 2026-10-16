<script lang="ts">
  import { onDestroy, untrack } from "svelte";
  import { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";
  import { Button, Modal, Notice } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import type { PhotoCull } from "./photoCull.svelte.js";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import type {
    PhotoAuthoredPatch,
    PhotoAuthoredReceipt,
    PhotoBrowseRow,
    Tag,
  } from "./generated/docbank.js";
  import PhotoStage from "./PhotoStage.svelte";
  import PhotoCell from "./PhotoCell.svelte";
  import { fittedPhotoView, type PhotoView } from "./photoView.js";
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
    () => new PhotoAuthoredEditor(photos.session, photos.onauthfailure, photos.authored.undoState),
  );
  onDestroy(() => editor.dispose());
  let photo = $state<PhotoBrowseRow>();
  let error = $state("");
  let loading = $state(false);
  let view = $state<PhotoView>({ ...fittedPhotoView });
  let info = $state(0);
  let help = $state(false);
  let announcement = $state("");
  const checkpoint = $derived(cull.run!.checkpoint);
  const rail = $derived(
    checkpoint.pass.slice(Math.max(0, checkpoint.position - 4), checkpoint.position + 5),
  );
  let neighbors = $state.raw<PhotoBrowseRow[]>([]);
  $effect(() => {
    const id = cull.currentID;
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
    const ids = rail;
    const controller = new AbortController();
    neighbors = [];
    void photos
      .lookupMany(ids, controller.signal)
      .then((rows) => {
        if (!controller.signal.aborted) neighbors = rows;
      })
      .catch(() => {});
    return () => controller.abort();
  });
  $effect(() => {
    const complete = cull.complete;
    const passID = checkpoint.passID;
    if (complete && passID) untrack(() => void cull.checkRemainder());
  });
  function move(direction: -1 | 1) {
    if (editor.canChangeTarget()) cull.move(direction);
  }
  async function refresh(receipt: PhotoAuthoredReceipt, tags: Tag[]) {
    onmetadatachanged?.(receipt, tags);
    photos.metadataChanged(receipt);
    const after = receipt.after.find((file) => file.file_id === photo?.display_file_id);
    if (photo && after) photo = { ...photo, flag: after.values.flag, rating: after.values.rating };
  }
  async function decide(patch: PhotoAuthoredPatch) {
    if (!photo || loading || cull.stale || cull.busy || !editor.canChangeTarget()) return;
    editor.reset();
    const id = photo.asset_id;
    await editor.edit({ asset_ids: [id] }, patch, false, refresh);
    if (editor.error || editor.dirty || editor.conflict) return;
    announcement =
      patch.flag !== undefined
        ? `${photo.name}: ${patch.flag || "flag cleared"}`
        : `${photo.name}: ${patch.rating} stars`;
    if (checkpoint.autoAdvance && cull.currentID === id) cull.move(1);
  }
  function close() {
    if (editor.canChangeTarget()) onclose();
  }
  function key(event: KeyboardEvent) {
    const action = cullKey(event);
    if (action === undefined) return;
    event.preventDefault();
    event.stopPropagation();
    if (action === "close") close();
    else if (action === "previous") move(-1);
    else if (action === "next") move(1);
    else if (action === "auto") cull.toggleAutoAdvance();
    else if (action === "scope" && editor.canChangeTarget()) void cull.nextPass();
    else if (action === "zoom")
      view = view.actual ? { ...fittedPhotoView } : { ...view, actual: true };
    else if (action === "info") info = (info + 1) % 3;
    else if (action === "help") help = !help;
    else if (action === "pick") void decide({ flag: "pick" });
    else if (action === "reject") void decide({ flag: "reject" });
    else if (action === "unflag") void decide({ flag: "" });
    else if (typeof action === "number") void decide({ rating: action });
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="review-shell" onkeydown={key}>
  <Modal
    ariaLabel="Cull photos"
    closable={false}
    closeOnOverlayClick={false}
    onclose={close}
    width="100vw"
    maxWidth="100vw"
    maxHeight="100svh"
  >
    <div class="photo-cull">
      <header>
        <Button size="sm" onclick={close}>Exit cull</Button>
        <strong>{photo?.name ?? (cull.complete ? "Pass complete" : "Cull photos")}</strong>
        <span
          >{Math.min(checkpoint.position + 1, checkpoint.pass.length)} of {checkpoint.pass
            .length}</span
        >
        <Button
          size="sm"
          disabled={cull.busy || cull.stale || editor.busy}
          onclick={() => {
            if (editor.canChangeTarget()) void cull.nextPass();
          }}>Scope: {checkpoint.scope}</Button
        >
        <Button
          size="sm"
          disabled={cull.stale || cull.busy}
          onclick={() => cull.toggleAutoAdvance()}
          >Auto-advance: {checkpoint.autoAdvance ? "on" : "off"}</Button
        >
        <Button size="sm" onclick={() => (help = !help)}>Keys (?)</Button>
      </header>
      {#if cull.error || editor.error || editor.notice}<p role="alert">
          {cull.error || editor.error || editor.notice}
        </p>{/if}
      {#if cull.progress && !cull.complete}<p role="status">{cull.progress}</p>{/if}
      {#if editor.dirty || editor.conflict}<div class="controls">
          <Button
            onclick={() => {
              editor.clearDraft();
              editor.reset();
            }}>Clear failed edit</Button
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
              disabled={cull.busy || cull.stale}
              onclick={() => {
                if (editor.canChangeTarget()) void cull.nextPass(true);
              }}>Review unflagged remainder</Button
            >{/if}
          <Button onclick={close}>Back to photos</Button>
        </section>
      {:else}
        <div class="body">
          {#if photo}<PhotoStage
              {photo}
              {photos}
              {cache}
              {view}
              onview={(value) => (view = value)}
              onmove={move}
            />
          {:else}<div class="unavailable" role="status">
              {loading ? "Loading photo…" : error}<Button onclick={() => move(1)}>Next photo</Button
              >
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
          <Button
            size="sm"
            disabled={checkpoint.position === 0 || editor.busy}
            onclick={() => move(-1)}>Previous</Button
          >
          <Button
            size="sm"
            disabled={!photo || cull.stale || editor.busy}
            onclick={() => void decide({ flag: "pick" })}>Pick (P)</Button
          >
          <Button
            size="sm"
            disabled={!photo || cull.stale || editor.busy}
            onclick={() => void decide({ flag: "reject" })}>Reject (X)</Button
          >
          <Button
            size="sm"
            disabled={!photo || cull.stale || editor.busy}
            onclick={() => void decide({ flag: "" })}>Clear flag (U)</Button
          >
          <Button
            size="sm"
            disabled={cull.stale || editor.busy}
            onclick={() => {
              if (editor.canChangeTarget()) cull.skip();
            }}>Skip</Button
          >
          <span>{photo ? `${photo.flag || "Unflagged"} · ${photo.rating} stars` : ""}</span>
          <Button
            size="sm"
            onclick={() =>
              (view = view.actual ? { ...fittedPhotoView } : { ...view, actual: true })}
            >1:1 (Z)</Button
          >
          <Button size="sm" onclick={() => (info = (info + 1) % 3)}>EXIF (I)</Button>
          <Button size="sm" disabled={cull.stale || editor.busy} onclick={() => move(1)}
            >Next</Button
          >
        </div>
        <nav class="filmstrip" aria-label="Cull pass">
          {#each rail as id}{@const row = neighbors.find((item) => item.asset_id === id)}
            <div class:current={id === cull.currentID} class="thumb">
              {#if row}<PhotoCell
                  photo={row}
                  selected={id === cull.currentID}
                  compact={true}
                  onclick={() => {
                    if (editor.canChangeTarget()) cull.choose(id);
                  }}
                  {cache}
                  onopen={() => {
                    if (editor.canChangeTarget()) cull.choose(id);
                  }}
                />{:else}<Button
                  size="sm"
                  onclick={() => {
                    if (editor.canChangeTarget()) cull.choose(id);
                  }}>Unavailable</Button
                >{/if}
            </div>{/each}
        </nav>
      {/if}
      {#if editor.receiptID}<Notice
          tone="success"
          message={editor.undoState.summary}
          actionLabel={cull.stale || editor.busy || editor.dirty ? undefined : "Undo"}
          onaction={() => void editor.undo(refresh)}
        />{/if}
      <span class="kit-sr-only" aria-live="polite">{announcement}</span>
    </div>
  </Modal>
</div>

<style>
  .review-shell :global(.kit-modal-body) {
    padding: 0;
    overflow: hidden;
  }
  .review-shell :global(.kit-modal-panel) {
    border: 0;
    border-radius: 0;
  }

  .photo-cull {
    height: 100svh;
    width: 100%;
    display: flex;
    flex-direction: column;
    background: var(--bg-surface);
    color: var(--text-primary);
  }
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
