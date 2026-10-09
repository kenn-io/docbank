<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { Button, EmptyState, IconButton, Menu, MenuTrigger, MenuContent, MenuItem, Modal, TextInput, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import StarIcon from "@lucide/svelte/icons/star";
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { Photos } from "./photos.svelte.js";
  import { groupPhotos, ROW_HEIGHTS, type Density } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import { isAppShortcutSuppressed } from "./shortcuts.js";
  import PhotoGrid from "./PhotoGrid.svelte";
  import SelectionDock from "./SelectionDock.svelte";
  import PhotoAlbumPicker from "./PhotoAlbumPicker.svelte";
  import { type PhotoAlbums, type PhotoAlbumItem } from "./photoAlbums.svelte.js";

  let { photos, cache, albums, albumID = "", onnavigate = () => {} }: { photos: Photos; cache: PhotoPreviewCache; albums?: PhotoAlbums; albumID?: string; onnavigate?: (path: string) => void } = $props();
  const album = $derived(albums?.items.find(item => item.id === albumID));
  let alive = true;
  onDestroy(() => alive = false);
  let picker = $state<{ focus: () => void; addToTarget: () => Promise<boolean> }>();
  let renaming = $state(false);
  let name = $state("");
  let modal = $state<"delete" | "duplicate" | undefined>();
  let modalError = $state("");
  let modalAlbum = $state<PhotoAlbumItem>();
  let duplicateName = $state("");
  let renameInput = $state<HTMLInputElement>();
  const sortOptions = [{ value: "added_time", label: "Added" }, { value: "capture_time", label: "Captured" }, { value: "import_time", label: "Imported" }];
  let grid = $state<{ preservePosition: () => (() => Promise<void>) }>();
  const preserve = () => grid?.preservePosition();
  const groups = $derived(groupPhotos(photos.items, albumID && photos.query.sort?.field !== "capture_time" ? "flat" : photos.grouping));
  const orderedIDs = $derived(groups.flatMap(group => group.items.map(item => item.asset_id)));
  const densityOptions = [{ value: "compact", label: "Compact" }, { value: "comfortable", label: "Comfortable" }, { value: "large", label: "Large" }];
  const groupingOptions = [{ value: "months", label: "Months" }, { value: "sessions", label: "Capture sessions" }];

  onMount(() => {
    void photos.resume(preserve);
    return () => photos.cancelPending();
  });
  function relayout(change: () => void) {
    const restore = preserve();
    change();
    void restore?.();
  }
  function escape(event: KeyboardEvent) {
    if (event.key === "Escape" && !isAppShortcutSuppressed(event, false, document, ".photo-cell")) photos.clearSelection();
    if (event.key.toLowerCase() === "b" && !event.repeat && !event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey && !isAppShortcutSuppressed(event, !photos.selection.selectedIDs.size, document, ".photo-cell")) {
      event.preventDefault();
      if (albums?.rejectBusy()) return;
      if (albums?.targetID) void picker?.addToTarget();
      else picker?.focus();
    }
  }
  async function refresh() { await Promise.all([photos.refresh(preserve), albums?.load()]); }
  async function remove() {
    if (!albums || !album) return;
    await albums.members(album, photos.scope(), photos, true, preserve);
  }
  async function rename() {
    if (!albums || !album || !name.trim()) return;
    const result = await albums.update(album, { name: name.trim() });
    if (alive && renaming && result) renaming = false;
  }
  function beginRename() { name = album?.name ?? ""; renaming = true; }
  function openModal(kind: "delete" | "duplicate") { modalError = ""; modalAlbum = album; modal = kind; duplicateName = `${album?.name ?? "Album"} copy`; }
  async function confirm() {
    if (!albums || !modalAlbum) return;
    const current = albums.items.find(item => item.id === modalAlbum!.id) ?? modalAlbum;
    const result = modal === "delete" ? await albums.delete(current) : await albums.duplicate(current, duplicateName);
    if (!alive || !modal) return;
    if (!result) { modalError = albums.error; albums.error = ""; }
    if (result) { const deleting = modal === "delete"; modal = undefined; onnavigate(deleting ? "/photos/albums" : `/photos/albums/${result.id}`); }
  }
  $effect(() => { if (renaming && renameInput) { renameInput.focus(); renameInput.select(); } });
</script>

<svelte:window onkeydown={escape} />
<main class="photos-workspace" aria-label={albumID ? "Photo album" : "Photo library"}>
  <div class="photo-toolbar browser-toolbar">
    <div class="library-title">
      {#if albumID}<button type="button" class="album-back" onclick={() => onnavigate("/photos/albums")}>Albums</button>{/if}
      {#if renaming}<form onsubmit={event => { event.preventDefault(); void rename(); }}><TextInput ariaLabel="Album name" bind:value={name} bind:inputEl={renameInput} onkeydown={event => { if (event.key === "Escape") { event.preventDefault(); renaming = false; } }} /><Button type="submit" size="sm" disabled={albums?.busy || !name.trim()}>Save</Button><Button size="sm" onclick={() => renaming = false}>Cancel</Button></form>
      {:else}<h1>{albumID ? album?.name ?? "Album" : "Library"}</h1>{/if}
      <span>{photos.total.toLocaleString()} photos · {photos.items.length.toLocaleString()} loaded</span>
    </div>
    <div class="toolbar-actions">
      <div class="photo-options">
        {#if albumID}<SelectDropdown title="Sort photos" value={photos.query.sort?.field ?? "added_time"} options={sortOptions} onchange={value => void photos.setQuery({ ...photos.query, sort: { field: value as "added_time" | "capture_time" | "import_time", direction: "desc" } })} />{/if}
        {#if !albumID || photos.query.sort?.field === "capture_time"}<SelectDropdown title="Group photos" value={photos.grouping} options={groupingOptions} onchange={value => relayout(() => photos.grouping = value as "months" | "sessions")} />{/if}
        <SelectDropdown title="Grid density" value={photos.density} options={densityOptions} onchange={value => relayout(() => photos.setDensity(value as Density))} />
      </div>
      {#if album && albums}
        <IconButton ariaLabel={album.starred ? "Unstar album" : "Star album"} title={album.starred ? "Unstar album" : "Star album"} ariaPressed={album.starred} disabled={albums.busy} onclick={() => void albums!.update(album!, { starred: !album!.starred })}><StarIcon size="16" fill={album.starred ? "currentColor" : "none"} /></IconButton>
        <Menu align="end"><MenuTrigger ariaLabel="Album actions" disabled={albums.busy}>More</MenuTrigger><MenuContent ariaLabel="Album actions"><MenuItem onselect={beginRename}>Rename</MenuItem><MenuItem onselect={() => openModal("duplicate")}>Duplicate…</MenuItem><MenuItem tone="danger" onselect={() => openModal("delete")}>Delete album…</MenuItem></MenuContent></Menu>
      {/if}<Button size="sm" disabled={photos.loading || albums?.busy} onclick={() => void refresh()}>Refresh previews</Button>
    </div>
  </div>
  {#if albums?.error}<div class="photo-error" role="alert">{albums.error}</div>{/if}
  {#if albums?.notice}<div class="photo-notice" role="status">{albums.notice}{#if albums.noticeID}<a href={`/photos/albums/${albums.noticeID}`} onclick={event => { event.preventDefault(); onnavigate(`/photos/albums/${albums!.noticeID}`); }}>Open album</a>{/if}</div>{/if}
  {#if photos.error}
    <div class="photo-error" role="alert"><span>{photos.error}</span><Button size="sm" onclick={() => void photos.retry(preserve)}>Retry</Button></div>
  {/if}
  {#if albumID && albums && albums.initialized && !albums.loading && !albums.busy && modal !== "delete" && !albums.loadError && !album}<EmptyState title="Album not found"><Button onclick={() => onnavigate("/photos/albums")}>Back to albums</Button></EmptyState>
  {:else if photos.items.length}
    {#key photos.query}
    <PhotoGrid bind:this={grid} bind:scrollTop={photos.scrollTop} {groups} targetRowHeight={ROW_HEIGHTS[photos.density]} loading={photos.loading} {cache} selectedIDs={photos.selection.selectedIDs} onselect={(id, event) => photos.select(id, event, orderedIDs)} oncheck={(id, checked, range) => photos.check(id, checked, range, orderedIDs)} onloadmore={() => void photos.loadMore(preserve)} ondragstart={(id, event) => albums?.startDrag(id, event, photos, preserve)} ondragend={() => { if (albums) albums.drag = undefined; }} />
    {/key}
  {:else if !photos.loading && !photos.error}
    <EmptyState title={albumID ? "This album is empty" : "Your photo library is empty"} description={albumID ? "Go to Library, select photos, then choose Add to album or press B." : "Import photos with docbank photos import to browse them here."}>
      {#snippet icon()}<ImageIcon size="24" />{/snippet}
      {#if albumID}<Button onclick={() => onnavigate("/photos")}>Go to Library</Button>{/if}
    </EmptyState>
  {/if}
  <div class="photo-loading" role="status">{#if photos.loading}<Spinner size={14} />Loading photos…{:else if photos.cursor && !photos.error}<Button size="sm" onclick={() => void photos.loadMore(preserve)}>Load more</Button>{/if}</div>
  <SelectionDock context="photos" selectedCount={photos.selection.selectedIDs.size} visibleDocumentCount={photos.items.length} loadedSelected={photos.items.every(item => photos.selection.selectedIDs.has(item.asset_id))} wholeQueryCount={photos.total} allResults={photos.allResults} onallresults={() => photos.selectAllResults()} onclear={() => photos.clearSelection()} onselectvisible={() => photos.selectLoaded()}>
    {#snippet photoActions()}
      {#if albums}<PhotoAlbumPicker bind:this={picker} {photos} {albums} {preserve} />{/if}
      {#if album && albums}<Button size="sm" disabled={albums.busy} onclick={() => void remove()}>Remove from album</Button><Button size="sm" disabled={albums.busy || photos.allResults || photos.selection.selectedIDs.size !== 1} onclick={() => void albums!.cover(album!, [...photos.selection.selectedIDs][0])}>Use as cover</Button>{/if}
    {/snippet}
  </SelectionDock>
</main>
{#if modal && modalAlbum && albums}
  <Modal title={modal === "delete" ? "Delete album" : "Duplicate album"} onclose={() => { if (!albums.busy) modal = undefined; }}>
    {#if modal === "delete"}<p>Delete "{modalAlbum.name}"? Its {modalAlbum.included_count === undefined ? "" : `${modalAlbum.included_count.toLocaleString()} `}photos stay in your library.</p>{:else}<TextInput ariaLabel="Copy name" bind:value={duplicateName} />{/if}
    {#if modalError}<p role="alert">{modalError}</p>{/if}
    <div class="modal-actions"><Button disabled={albums.busy} onclick={() => modal = undefined}>Cancel</Button><Button tone={modal === "delete" ? "danger" : "info"} disabled={albums.busy || modal === "duplicate" && !duplicateName.trim()} onclick={() => void confirm()}>{modal === "delete" ? "Delete album" : "Duplicate album"}</Button></div>
  </Modal>
{/if}

<style>
  .photos-workspace { max-height: calc(100dvh - var(--header-height)); display: flex; flex-direction: column; flex: 1; min-height: 0; overflow: hidden; background: var(--bg-surface); }
  .photo-toolbar { border-bottom: 1px solid var(--border-default); }
  .library-title h1 { margin: 0 0 4px; font-size: var(--font-size-lg); color: var(--text-primary); }
  .library-title span { font-size: var(--font-size-xs); color: var(--text-muted); }
  .photo-options { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .photo-error { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-3) var(--space-5); color: var(--text-primary); background: var(--bg-inset); }
  .photo-notice { display: flex; flex-wrap: wrap; gap: var(--space-3); padding: var(--space-2) var(--space-5); font-size: var(--font-size-sm); }
  .photo-notice a { color: var(--accent-blue); }
  .album-back { border: 0; padding: 0; background: transparent; color: var(--accent-blue); font-size: var(--font-size-xs); cursor: pointer; margin-bottom: 5px; }
  form, .modal-actions { display: flex; gap: var(--space-2); align-items: center; flex-wrap: wrap; }
  .modal-actions { justify-content: flex-end; margin-top: var(--space-4); }
  @media (max-width: 640px) { .photo-toolbar { flex-wrap: wrap; gap: var(--space-3); } }
  .photo-loading { height: 38px; flex-shrink: 0; display: flex; gap: var(--space-2); align-items: center; justify-content: center; padding: var(--space-2); color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
