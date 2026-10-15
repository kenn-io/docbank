<script lang="ts">
  import { Button, Modal } from "@kenn-io/kit-ui";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import { fittedPhotoView, type PhotoView } from "./photoView.js";
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
  $effect(() => {
    selected = [...ids];
  });
  $effect(() => {
    const members = selected;
    const controller = new AbortController();
    rows = [];
    errors = [];
    void Promise.all(
      members.map(async (id) => {
        try {
          return await photos.lookup(id, controller.signal);
        } catch (cause) {
          if (!controller.signal.aborted)
            errors = [...errors, cause instanceof Error ? cause.message : String(cause)];
          return undefined;
        }
      }),
    ).then((values) => {
      if (!controller.signal.aborted) rows = values.filter((row): row is PhotoBrowseRow => !!row);
    });
    return () => controller.abort();
  });
  function move(direction: -1 | 1) {
    const order = photos.items.map((item) => item.asset_id);
    const positions = selected.map((id) => order.indexOf(id));
    if (
      positions.some(
        (index) => index < 0 || index + direction < 0 || index + direction >= order.length,
      )
    )
      return;
    selected = positions.map((index) => order[index + direction]!);
    view = { ...fittedPhotoView };
    crop = undefined;
  }
  function key(event: KeyboardEvent) {
    if (event.ctrlKey || event.altKey || event.metaKey || event.isComposing || event.repeat) return;
    if (event.key === "Escape") {
      event.preventDefault();
      onclose();
    } else if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
      event.preventDefault();
      move(event.key === "ArrowLeft" ? -1 : 1);
    } else if (event.key.toLowerCase() === "z") {
      event.preventDefault();
      view = view.actual ? { ...fittedPhotoView } : { ...view, actual: true };
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
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="review-shell" onkeydown={key}>
  <Modal
    ariaLabel="Compare photos"
    closable={false}
    closeOnOverlayClick={false}
    {onclose}
    width="100vw"
    maxWidth="100vw"
    maxHeight="100svh"
  >
    <div class="photo-compare">
      <header>
        <Button size="sm" onclick={onclose}>Exit compare</Button><strong
          >Compare {selected.length} photos</strong
        ><span>Zoom and navigation are linked</span><Button size="sm" onclick={() => move(-1)}
          >Previous</Button
        ><Button size="sm" onclick={() => move(1)}>Next</Button><Button
          size="sm"
          onclick={() => (view = view.actual ? { ...fittedPhotoView } : { ...view, actual: true })}
          >1:1 (Z)</Button
        ><Button
          size="sm"
          onclick={() => {
            regionMode = !regionMode;
            view = { ...fittedPhotoView };
          }}>Manual closeup</Button
        >{#if crop}<Button size="sm" onclick={() => (crop = undefined)}>Clear closeup</Button>{/if}
      </header>
      {#if regionMode}<p role="status">
          Click a face or draw a region on any frame. Matching closeups appear below.
        </p>{/if}
      {#each errors as error}<p role="alert">{error}</p>{/each}
      <div class="frames">
        {#each rows as photo (photo.asset_id)}<section aria-label={photo.name}>
            <h2>{photo.name}</h2>
            <PhotoStage
              {photo}
              {photos}
              {cache}
              {view}
              onview={(value) => (view = value)}
              onmove={move}
              {regionMode}
              onregion={region}
            />
          </section>{/each}
      </div>
      {#if crop}<div class="closeups" aria-label="Matching closeups">
          {#each rows as photo (photo.asset_id)}<section aria-label={`Closeup of ${photo.name}`}>
              <h2>{photo.name}</h2>
              <PhotoStage {photo} {photos} {cache} view={crop} onview={(value) => (crop = value)} />
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
    display: flex;
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
    flex: 1;
    min-width: 0;
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
