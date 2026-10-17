<script lang="ts">
  import { APIError } from "./api-transport.js";
  import { onMount, onDestroy } from "svelte";
  import { Button, EmptyState, Modal, Spinner, TextInput } from "@kenn-io/kit-ui";
  import StarIcon from "@lucide/svelte/icons/star";
  import ImageIcon from "@lucide/svelte/icons/image";
  import { type PhotoAlbums } from "./photoAlbums.svelte.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import PhotoAlbumCover from "./PhotoAlbumCover.svelte";
  import PhotoAlbumPending from "./PhotoAlbumPending.svelte";
  import { listSavedQueries, subscribeSavedQueries, type SavedQuery } from "./savedQueries.js";
  let {
    albums,
    cache,
    onnavigate,
    onopensmart,
    smartDisabled = false,
  }: {
    albums: PhotoAlbums;
    cache: PhotoPreviewCache;
    onnavigate: (path: string) => void;
    onopensmart?: (saved: SavedQuery) => void;
    smartDisabled?: boolean;
  } = $props();
  let smartSets = $state<SavedQuery[]>([]);
  let smartError = $state("");
  let smartOffset = $state(0);
  let smartTotal = $state(0);
  let smartLoading = $state(false);
  let smartGeneration = 0;
  async function loadSmartSets(reset = false, signal?: AbortSignal) {
    if (smartLoading && !reset) return;
    const generation = reset ? ++smartGeneration : smartGeneration;
    if (reset) {
      smartSets = [];
      smartOffset = 0;
      smartTotal = 0;
    }
    smartLoading = true;
    smartError = "";
    try {
      const page = await listSavedQueries(albums.session, "query", smartOffset, 100, true, signal);
      signal?.throwIfAborted();
      if (!alive || generation !== smartGeneration) return;
      const seen = new Set(smartSets.map((saved) => saved.id));
      for (const saved of page.items)
        if (!seen.has(saved.id)) {
          seen.add(saved.id);
          smartSets.push(saved);
        }
      smartOffset += page.items.length;
      smartTotal = page.items.length ? page.total : smartOffset;
    } catch (cause) {
      if (alive && generation === smartGeneration) {
        if (cause instanceof APIError && cause.status === 401) albums.onauthfailure(cause);
        smartError = cause instanceof Error ? cause.message : String(cause);
      }
      throw cause;
    } finally {
      if (alive && generation === smartGeneration) smartLoading = false;
    }
  }
  onMount(() => {
    if (!onopensmart) return;
    void loadSmartSets().catch(() => {});
    return subscribeSavedQueries((session, signal) => {
      if (session === albums.session) return loadSmartSets(true, signal);
    });
  });
  let alive = true;
  onDestroy(() => (alive = false));
  let creating = $state(false);
  let createError = $state("");
  let name = $state("");
  function beginCreate() {
    creating = true;
    createError = "";
    name = "";
  }
  async function create() {
    if (albums.busy || albums.unconfirmed) return;
    const album = await albums.create(name);
    if (!alive || !creating) return;
    if (!album) {
      createError = albums.error;
      albums.error = "";
    }
    if (album) {
      creating = false;
      name = "";
      onnavigate(`/photos/albums/${album.id}`);
    }
  }
</script>

<main class="albums-workspace" aria-label="Albums">
  <div class="browser-toolbar album-toolbar">
    <div>
      <h1>Albums</h1>
      <span
        >{albums.items.length.toLocaleString()}
        {albums.items.length === 1 ? "album" : "albums"}</span
      >
    </div>
    <Button size="sm" tone="info" onclick={beginCreate}>New album</Button>
  </div>
  <section class="smart-sets" aria-label="Smart sets">
    {#if smartSets.length}<h2>Smart sets</h2>
      <ul>
        {#each smartSets as saved (saved.id)}<li>
            <Button
              disabled={smartDisabled}
              onclick={() => {
                if (!smartDisabled) onopensmart?.(saved);
              }}>{saved.name}</Button
            ><span>Smart set</span>
          </li>{/each}
      </ul>{/if}
    {#if smartError}<p role="alert">
        {smartError}<Button onclick={() => void loadSmartSets().catch(() => {})}
          >Retry smart sets</Button
        >
      </p>{/if}
    {#if smartOffset < smartTotal}<Button
        disabled={smartLoading}
        onclick={() => void loadSmartSets().catch(() => {})}>Load more smart sets</Button
      >{/if}
  </section>
  {#if !creating}<PhotoAlbumPending {albums} {onnavigate} />{/if}
  {#if albums.error}<div class="album-error" role="alert">{albums.error}</div>{/if}
  {#if albums.loadError}<div class="album-error" role="alert">
      {albums.loadError}<Button size="sm" onclick={() => void albums.load()}>Retry</Button>
    </div>{/if}
  {#if albums.loading && !albums.items.length}<div class="album-loading" role="status">
      <Spinner />Loading albums…
    </div>
  {:else if !albums.items.length && !albums.loadError}<EmptyState
      title="Your albums are empty"
      description="Create an album, then add photos from Library."
      >{#snippet icon()}<ImageIcon size="24" />{/snippet}<Button onclick={beginCreate}
        >New album</Button
      ></EmptyState
    >
  {:else}
    <ul class="album-grid">
      {#each albums.items as album (album.id)}
        <li class="album-card">
          <PhotoAlbumCover {album} {cache} unavailable={!!albums.loadError} />
          <a
            href={`/photos/albums/${album.id}`}
            onclick={(event) => {
              if (!event.ctrlKey && !event.metaKey && !event.shiftKey && event.button === 0) {
                event.preventDefault();
                onnavigate(`/photos/albums/${album.id}`);
              }
            }}
          >
            <div class="album-caption">
              <strong title={album.name}>{album.name}</strong><span
                >{albums.loadError || album.included_count === undefined
                  ? "Count unavailable"
                  : `${album.included_count.toLocaleString()} ${album.included_count === 1 ? "photo" : "photos"}`}</span
              >
            </div>
          </a>
          <button
            type="button"
            class="album-star"
            aria-label={album.starred ? `Unstar ${album.name}` : `Star ${album.name}`}
            aria-pressed={album.starred}
            disabled={albums.busy}
            onclick={() => void albums.update(album, { starred: !album.starred })}
            ><StarIcon size="18" fill={album.starred ? "currentColor" : "none"} /></button
          >
        </li>
      {/each}
    </ul>
  {/if}
</main>
{#if creating}
  <Modal
    title="New album"
    onclose={() => {
      if (!albums.busy) creating = false;
    }}
  >
    <form
      onsubmit={(event) => {
        event.preventDefault();
        void create();
      }}
    >
      <TextInput ariaLabel="Album name" bind:value={name} />
      <div class="modal-actions">
        <Button disabled={albums.busy} onclick={() => (creating = false)}>Cancel</Button><Button
          type="submit"
          tone="info"
          disabled={albums.busy || !!albums.unconfirmed || !name.trim()}>Create album</Button
        >
      </div>
    </form>
    <PhotoAlbumPending
      {albums}
      onnavigate={(path) => {
        creating = false;
        onnavigate(path);
      }}
    />
    {#if createError}<p role="alert">{createError}</p>{/if}
  </Modal>
{/if}

<style>
  .smart-sets {
    padding: 12px;
  }
  .smart-sets h2 {
    margin: 0 0 8px;
    font-size: var(--font-size-sm);
  }
  .smart-sets ul {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    padding: 0;
    margin: 0;
    list-style: none;
  }
  .smart-sets li {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }
  .albums-workspace {
    flex: 1;
    min-width: 0;
    overflow: auto;
    background: var(--bg-surface);
  }
  .album-toolbar {
    border-bottom: 1px solid var(--border-default);
  }
  h1 {
    margin: 0 0 4px;
    font-size: var(--font-size-lg);
  }
  .album-toolbar span {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }
  .album-grid {
    list-style: none;
    padding: var(--space-5);
    margin: 0;
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
    gap: var(--space-4);
  }
  .album-card {
    position: relative;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    overflow: hidden;
  }
  a {
    display: block;
    color: var(--text-primary);
    text-decoration: none;
  }
  a::before {
    content: "";
    position: absolute;
    inset: 0;
  }
  a:focus-visible::before {
    outline: 3px solid var(--accent-blue);
    outline-offset: -3px;
  }
  .album-caption {
    padding: var(--space-3);
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  strong {
    display: -webkit-box;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
    overflow-wrap: anywhere;
  }
  .album-caption span {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }
  .album-star {
    position: absolute;
    z-index: 1;
    top: 8px;
    right: 8px;
    padding: 8px;
    display: flex;
    background: var(--bg-surface);
    color: var(--text-primary);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    cursor: pointer;
  }
  .album-error,
  .album-loading {
    display: flex;
    gap: var(--space-3);
    align-items: center;
    padding: var(--space-4);
  }
  .modal-actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-3);
    margin-top: var(--space-4);
  }
  @media (max-width: 640px) {
    .album-grid {
      padding: var(--space-3);
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: var(--space-3);
    }
  }
</style>
