<script lang="ts">
  import { onMount, tick } from "svelte";
  import { Button, Modal } from "@kenn-io/kit-ui";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import { fittedPhotoView, toggleActualPhotoView, type PhotoView } from "./photoView.js";
  import PhotoStage from "./PhotoStage.svelte";
  let {
    ids,
    photos,
    cache,
    onclose,
  }: { ids: string[]; photos: Photos; cache: PhotoPreviewCache; onclose: () => void } = $props();
  let selected = $state<string[]>([]);
  let rows = $state.raw<PhotoBrowseRow[]>([]);
  let errors = $state.raw<string[]>([]);
  let view = $state<PhotoView>({ ...fittedPhotoView });
  let regionMode = $state(false);
  let crop = $state<PhotoView>();
  let moving = $state(false);
  let navigationError = $state("");
  let retryDirection = $state<-1 | 1>(1);
  let content = $state<HTMLDivElement>();
  let retry = $state(0);
  let loading = $state(false);
  onMount(() => {
    void restoreFocus();
  });
  $effect(() => {
    selected = [...ids];
  });
  $effect(() => {
    const members = selected;
    void retry;
    const controller = new AbortController();
    errors = [];
    loading = true;
    void photos
      .lookupMany(members, controller.signal)
      .then((values) => {
        if (!controller.signal.aborted) {
          rows = values;
          void restoreFocus();
        }
      })
      .catch((cause) => {
        if (!controller.signal.aborted)
          errors = [cause instanceof Error ? cause.message : String(cause)];
      })
      .finally(() => {
        if (!controller.signal.aborted) loading = false;
      });
    return () => controller.abort();
  });
  async function move(direction: -1 | 1) {
    if (moving || photos.loading) return;
    moving = true;
    navigationError = "";
    const result = await photos.moveGroup(selected, direction);
    moving = false;
    if (result.status === "failed") {
      navigationError = result.error;
      retryDirection = direction;
    }
    if (result.status !== "moved") return;
    selected = result.ids;
    view = { ...fittedPhotoView };
    crop = undefined;
    void restoreFocus();
  }
  function key(event: KeyboardEvent) {
    if (event.ctrlKey || event.altKey || event.metaKey || event.isComposing || event.repeat) return;
    if (event.key === "Escape") {
      event.preventDefault();
      onclose();
    } else if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
      event.preventDefault();
      void move(event.key === "ArrowLeft" ? -1 : 1);
    } else if (event.key.toLowerCase() === "z") {
      event.preventDefault();
      view = toggleActualPhotoView(view);
    }
  }
  function region(value: { x: number; y: number; width: number; height: number }) {
    crop = {
      scale: Math.min(32, Math.max(1, 1 / Math.max(value.width, value.height))),
      x: value.x,
      y: value.y,
      actual: false,
    };
    regionMode = false;
    void restoreFocus();
  }
  function cancelRegion() {
    regionMode = false;
    void restoreFocus();
  }
  async function restoreFocus() {
    await tick();
    content?.focus({ preventScroll: true });
  }
</script>

<div class="review-shell">
  <Modal
    ariaLabel="Compare photos"
    closable={false}
    closeOnOverlayClick={false}
    {onclose}
    width="100vw"
    maxWidth="100vw"
    maxHeight="100svh"
  >
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="photo-compare" tabindex="-1" bind:this={content} onkeydown={key}>
      <header>
        <Button size="sm" onclick={onclose}>Exit compare</Button><strong
          >Compare {selected.length} photos</strong
        ><span>Zoom and navigation are linked</span><Button
          size="sm"
          disabled={moving || photos.loading}
          onclick={() => void move(-1)}>Previous</Button
        ><Button
          size="sm"
          disabled={moving ||
            photos.loading ||
            (!photos.cursor && selected.some((id) => id === photos.items.at(-1)?.asset_id))}
          onclick={() => void move(1)}>Next</Button
        ><Button size="sm" onclick={() => (view = toggleActualPhotoView(view))}>1:1 (Z)</Button
        ><Button
          size="sm"
          onclick={() => {
            regionMode = !regionMode;
            view = { ...fittedPhotoView };
          }}>Manual closeup</Button
        >{#if crop}<Button size="sm" onclick={() => (crop = undefined)}>Clear closeup</Button>{/if}
      </header>
      {#if regionMode}<p role="status">
          Click or draw on a frame, or focus it and use arrow keys to position a closeup. Press
          Enter to use that area. Matching closeups appear below.
        </p>{/if}
      {#if navigationError}<p role="alert">
          {navigationError}<Button
            size="sm"
            disabled={moving || photos.loading}
            onclick={() => void move(retryDirection)}
            >Retry {retryDirection > 0 ? "next" : "previous"} photo</Button
          >
        </p>{/if}
      {#each errors as error}<p role="alert">{error}</p>{/each}
      {#if errors.length}<Button size="sm" disabled={loading} onclick={() => retry++}>Retry</Button
        >{/if}
      <div class="frames" style={`--columns: ${Math.min(4, rows.length)}`}>
        {#each rows as photo (photo.asset_id)}<section aria-label={photo.name}>
            <h2>{photo.name}</h2>
            <PhotoStage
              {photo}
              {photos}
              {cache}
              {view}
              onfocus={restoreFocus}
              onreload={(row) =>
                (rows = rows.map((item) => (item.asset_id === row.asset_id ? row : item)))}
              onunavailable={(message) => {
                rows = rows.filter((item) => item.asset_id !== photo.asset_id);
                errors = [...errors, message];
              }}
              onview={(value) => (view = value)}
              onmove={move}
              {regionMode}
              onregion={region}
              oncancelregion={cancelRegion}
            />
          </section>{/each}
      </div>
      {#if crop}<div
          class="closeups"
          aria-label="Matching closeups"
          style={`--columns: ${Math.min(4, rows.length)}`}
        >
          {#each rows as photo (photo.asset_id)}<section aria-label={`Closeup of ${photo.name}`}>
              <h2>{photo.name}</h2>
              <PhotoStage
                {photo}
                {photos}
                {cache}
                view={crop}
                onview={(value) => (crop = value)}
                onfocus={restoreFocus}
              />
            </section>{/each}
        </div>{/if}
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

  .photo-compare {
    height: 100svh;
    width: 100%;
    display: flex;
    flex-direction: column;
    background: var(--bg-surface);
    color: var(--text-primary);
  }
  header {
    display: flex;
    flex-wrap: wrap;
    gap: 12px;
    padding: 12px;
    align-items: center;
    background: var(--bg-surface);
  }
  header strong {
    flex: 1;
  }
  p {
    padding: 8px 12px;
    margin: 0;
  }
  .frames,
  .closeups {
    display: grid;
    grid-template-columns: repeat(var(--columns), minmax(0, 1fr));
    grid-auto-rows: minmax(0, 1fr);
    flex: 1;
    min-height: 0;
  }
  .closeups {
    max-height: 35%;
    border-top: 1px solid var(--border-default);
  }
  section {
    display: flex;
    flex-direction: column;
    min-width: 0;
    min-height: 0;
    border-right: 1px solid var(--border-default);
  }
  h2 {
    font-size: var(--font-size-sm);
    margin: 0;
    padding: 8px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
