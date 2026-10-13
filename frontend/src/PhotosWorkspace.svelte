<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from "svelte";
  import { Button, EmptyState, IconButton, Menu, MenuTrigger, MenuContent, MenuItem, Modal, TextInput, SelectDropdown, SearchInput, Spinner } from "@kenn-io/kit-ui";
  import StarIcon from "@lucide/svelte/icons/star";
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { Photos, PhotoReturn } from "./photos.svelte.js";
  import { groupPhotos, monthLabel, ROW_HEIGHTS, type Density } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import { isAppShortcutSuppressed } from "./shortcuts.js";
  import PhotoGrid from "./PhotoGrid.svelte";
  import PhotoLoupe from "./PhotoLoupe.svelte";
  import PhotoTimeline from "./PhotoTimeline.svelte";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import FacetSidebar from "./FacetSidebar.svelte";
  import SelectionDock from "./SelectionDock.svelte";
  import PhotoAlbumPicker from "./PhotoAlbumPicker.svelte";
  import PhotoAlbumPending from "./PhotoAlbumPending.svelte";
  import { type PhotoAlbums, type PhotoAlbumItem } from "./photoAlbums.svelte.js";
  import PhotoRejectsModal from "./PhotoRejectsModal.svelte";

  let { photos, cache, title = "Library", albums, albumID = "", onnavigate = () => {}, ontrashed, onhidden, onactionerror, oninvalidate, photoID = "", onphotochange = () => {} }: { photos: Photos; cache: PhotoPreviewCache; title?: string; albums?: PhotoAlbums; albumID?: string; onnavigate?: (path: string) => void; ontrashed?: (source: Photos) => void; onhidden?: () => void | Promise<void>; onactionerror?: (error: string) => void; oninvalidate?: () => void | Promise<void>; photoID?: string; onphotochange?: (id: string, mode: "open" | "step" | "close") => void } = $props();
  const album = $derived(albums?.items.find(item => item.id === albumID));
  let alive = true;
  onDestroy(() => alive = false);
  let picker = $state<{ focus: () => void; addToTarget: () => Promise<boolean> }>();
  let renaming = $state(false);
  let renameAlbum = $state<PhotoAlbumItem>();
  let renameError = $state("");
  let name = $state("");
  let modal = $state<"delete" | "duplicate" | undefined>();
  let modalError = $state("");
  let modalAlbum = $state<PhotoAlbumItem>();
  let duplicateName = $state("");
  let renameInput = $state<HTMLInputElement>();
  const albumSortOptions = [{ value: "added_time", label: "Added" }, { value: "capture_time", label: "Captured" }, { value: "import_time", label: "Imported" }];
  let trashOpen = $state(false);
  let direct = $state<PhotoBrowseRow>();
  let detailError = $state("");
  let detailLoading = $state(false);
  let reload = $state.raw<{id:string}>();
  let consumedReload: typeof reload;
  let reloadRevision = $state(0);
  let detailController: AbortController | undefined;
  const fromGrid = $derived(photos.viewerReturn?.contextID === photos.contextID);
  const listed = $derived(photos.items.some(item => item.asset_id === photoID));
  const membershipNote = $derived(photoID && fromGrid && !listed && !photos.loading ? "This photo is no longer in the refreshed result." : "");
  let localReturn: PhotoReturn | undefined;
  let openingElement: HTMLElement | undefined;
  let openingID = "";
  let restorePosition: (() => Promise<void>) | undefined;
  $effect(() => {
    const id = photoID;
    if (!id) { direct = undefined; detailError = ""; if (fromGrid) void restoreGrid(); return; }
    const controller = new AbortController();
    detailController = controller;
    const explicit = reload?.id === id && reload !== consumedReload;
    if (!explicit) { direct = undefined; detailError = ""; }
    if (!explicit && fromGrid && listed) { detailLoading = false; return () => controller.abort(); }
    detailLoading = true;
    if (explicit) consumedReload = reload;
    void photos.lookup(id, AbortSignal.any([controller.signal, AbortSignal.timeout(30_000)])).then(photo => { if (!controller.signal.aborted) { const index = photos.items.findIndex(item => item.asset_id === id); if (index >= 0) photos.items[index] = photo; direct = photo; detailError = ""; if (explicit) reloadRevision++; } }).catch(cause => { if (!controller.signal.aborted) detailError = cause instanceof Error ? cause.message : String(cause); }).finally(() => { if (!controller.signal.aborted) detailLoading = false; });
    return () => controller.abort();
  });
  async function restoreGrid() {
    const record = photos.viewerReturn!;
    await tick();
    if (photoID || photos.viewerReturn !== record) return;
    if (record === localReturn) await restorePosition?.();
    await tick();
    const control = record.control === "check" ? "input[type=checkbox]" : record.control === "open" ? ".open-photo" : ".photo-image";
    const target = record === localReturn && openingElement?.isConnected ? openingElement : document.querySelector<HTMLElement>(`[data-asset="${record.assetID}"] ${control}`);
    target?.focus({ preventScroll: true });
    if (!photoID && photos.viewerReturn === record) photos.viewerReturn = undefined;
  }
  export function captureReturn(element: HTMLElement = document.activeElement as HTMLElement) {
    openingElement = element; openingID = element.closest<HTMLElement>("[data-asset]")?.dataset.asset ?? ""; restorePosition = preserve();
    photos.viewerReturn = {contextID:photos.contextID, assetID:openingID, control:element.matches(".open-photo") ? "open" : element.matches("input[type=checkbox]") ? "check" : "image", scrollTop:photos.scrollTop};
    localReturn = photos.viewerReturn;
  }
  function open(id: string, element: HTMLElement) {
    captureReturn(element);
    onphotochange(id, "open");
  }
  async function move(direction: -1 | 1) {
    const controller = detailController;
    if (!fromGrid || !current || !controller) return;
    const id = current.asset_id;
    const index = ordered.findIndex(item => item.asset_id === id);
    if (index < 0) return;
    if (direction === 1 && index === ordered.length - 1 && photos.cursor) await photos.loadMore(preserve);
    if (controller.signal.aborted || photoID !== id) return;
    const currentIndex = ordered.findIndex(item => item.asset_id === id);
    if (currentIndex < 0) return;
    const next = ordered[currentIndex + direction];
    if (next) onphotochange(next.asset_id, "step");
  }
  async function retryNavigation() {
    await photos.retry(preserve);
  }
  function select(id: string, event: MouseEvent) {
    const touch = "pointerType" in event && event.pointerType === "touch";
    if (touch && !event.shiftKey && !event.ctrlKey && !event.metaKey) {
      if (photos.selection.selectedIDs.size) photos.check(id, !photos.selection.selectedIDs.has(id), false, orderedIDs);
      else open(id, event.currentTarget as HTMLElement);
    }
    else photos.select(id, event, orderedIDs);
  }
  let grid = $state<{ preservePosition: () => (() => Promise<void>) }>();
  let rejectsOpen = $state(false);
  const preserve = () => grid?.preservePosition();
  export function refresh() { return photos.refresh(preserve); }
  async function setHidden(id: string) {
    if (photos.hiding || photos.trashing) return;
    onactionerror?.("");
    await photos.setHidden(id, preserve, onhidden, onactionerror, oninvalidate);
  }
  const groups = $derived(groupPhotos(photos.items, photos.query.sort?.field === "relevance" ? "relevance" : albumID && photos.query.sort?.field !== "capture_time" ? "flat" : photos.grouping));
  // Albums keep their own scope and sort, so search and filter counts live in Library and Hidden.
  const searchable = $derived(!albumID);
  const filtered = $derived(searchable && (Boolean(photos.baseQuery.text.trim()) || Object.values(photos.baseQuery.filters ?? {}).some(value => Array.isArray(value) ? value.length > 0 : value !== undefined && value !== false && value !== "")));
  const ordered = $derived(photos.items);
  const current = $derived(photoID ? (fromGrid ? ordered.find(item => item.asset_id === photoID) ?? direct : direct) : undefined);
  const orderedIDs = $derived(groups.flatMap(group => group.items.map(item => item.asset_id)));
  const dateLabel = $derived(photos.date?.length === 7 ? monthLabel(photos.date) : photos.date);
  const densityOptions = [{ value: "compact", label: "Compact" }, { value: "comfortable", label: "Comfortable" }, { value: "large", label: "Large" }];
  let search = $state(untrack(() => photos.baseQuery.text));
  $effect(() => { search = photos.query.text; });
  const sortOptions = $derived([{ value: "capture_time", label: "Capture date" }, ...(photos.baseQuery.text.trim() ? [{ value: "relevance", label: "Relevance" }] : [])]);
  const groupingOptions = [{ value: "months", label: "Months" }, { value: "sessions", label: "Capture sessions" }];

  onMount(() => {
    void photos.resume(preserve);
    return () => { photos.cancelPending(); openingElement = undefined; restorePosition = undefined; localReturn = undefined; };
  });
  function relayout(change: () => void) {
    const restore = preserve();
    change();
    void restore?.();
  }
  function escape(event: KeyboardEvent) {
    if (!photoID && event.key === "Escape" && !isAppShortcutSuppressed(event, false, document, ".photo-cell")) photos.clearSelection();
    if (albums && !photoID && event.key.toLowerCase() === "b" && !event.repeat && !event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey && !isAppShortcutSuppressed(event, !photos.selection.selectedIDs.size, document, ".photo-cell")) {
      event.preventDefault();
      if (albums.rejectBusy()) return;
      if (albums.targetID) void picker?.addToTarget();
      else picker?.focus();
    }
  }
  async function refreshView() { await Promise.all([refresh(), albums?.load()]); }
  async function remove() {
    if (!albums || !album) return;
    await albums.members(album, photos.scope(), true);
  }
  async function rename() {
    if (!albums || !renameAlbum || albums.busy || !name.trim()) return;
    const inspected = renameAlbum;
    renameError = "";
    const result = await albums.update(inspected, { name: name.trim() });
    if (!alive || !renaming || renameAlbum !== inspected) return;
    if (result) renaming = false;
    else { renameError = albums.error; albums.error = ""; if (!albums.loadError) renameAlbum = albums.items.find(item => item.id === inspected.id) ?? inspected; }
  }
  function beginRename() { renameError = ""; renameAlbum = album; name = album?.name ?? ""; renaming = true; }
  function openModal(kind: "delete" | "duplicate") { modalError = ""; modalAlbum = album; modal = kind; duplicateName = `${album?.name ?? "Album"} copy`; }
  async function confirm() {
    if (!albums || !modal || !modalAlbum || albums.busy || modal === "duplicate" && albums.unconfirmed) return;
    modalError = "";
    const inspected = modalAlbum;
    const result = modal === "delete" ? await albums.delete(inspected) : await albums.duplicate(inspected, duplicateName);
    if (!alive || !modal) return;
    if (!result) { modalError = albums.error; albums.error = ""; if (!albums.loadError) modalAlbum = albums.items.find(item => item.id === inspected.id) ?? inspected; }
    if (result) { const deleting = modal === "delete"; modal = undefined; onnavigate(deleting ? "/photos/albums" : `/photos/albums/${result.id}`); }
  }
  $effect(() => { if (renaming && renameInput) { renameInput.focus(); renameInput.select(); } });
</script>

{#snippet sourceSummary(source: PhotoAlbumItem)}
  <span>{source.name}{#if !albums?.loadError && source.included_count !== undefined}{" · "}{source.included_count.toLocaleString()} {source.included_count === 1 ? "photo" : "photos"}{/if}</span>
{/snippet}

<svelte:window onkeydown={escape} />
<main class="photos-workspace" aria-label={albumID ? "Photo album" : "Photo library"}>
  <div class="library-grid" inert={!!photoID}>
  <div class="photo-toolbar browser-toolbar">
    <div class="library-title">
      {#if albumID}<button type="button" class="album-back" onclick={() => onnavigate("/photos/albums")}>Albums</button>{/if}
      {#if renaming && renameAlbum}<form onsubmit={event => { event.preventDefault(); void rename(); }}>{@render sourceSummary(renameAlbum)}<TextInput ariaLabel="Album name" bind:value={name} bind:inputEl={renameInput} onkeydown={event => { if (event.key === "Escape") { event.preventDefault(); renaming = false; } }} /><Button type="submit" size="sm" disabled={albums?.busy || !name.trim()}>Save</Button><Button size="sm" onclick={() => renaming = false}>Cancel</Button>{#if renameError}<span role="alert">{renameError}</span>{/if}</form>
      {:else}<h1>{albumID ? album?.name ?? "Album" : title}</h1>{/if}
      <span>{!photos.started ? (photos.loading ? "Loading photos" : "Photos") : `${photos.total.toLocaleString()} photos`}{photos.date ? ` ${photos.date.length === 10 ? "on" : "in"} ${dateLabel}` : ""}{photos.started ? ` · ${photos.items.length.toLocaleString()} loaded` : ""}</span>
    </div>
    <div class="toolbar-actions">
      {#if !photos.hidden}<nav class="photo-views" aria-label="Photo views"><button type="button" class="photo-toggle kit-button kit-control-states kit-button--sm" aria-pressed={photos.view === "grid"} onclick={() => photos.setView("grid")}>Grid</button><button type="button" class="photo-toggle kit-button kit-control-states kit-button--sm" aria-pressed={photos.view === "timeline"} onclick={() => photos.setView("timeline")}>Timeline</button></nav>{/if}
      <div class="photo-options">
        {#if albumID}<SelectDropdown title="Sort photos" value={photos.query.sort?.field ?? "added_time"} options={albumSortOptions} onchange={value => void photos.setSort({ field: value as "added_time" | "capture_time" | "import_time", direction: "desc" })} />{/if}
        {#if !albumID || photos.query.sort?.field === "capture_time"}<SelectDropdown title="Group photos" disabled={photos.query.sort?.field !== "capture_time"} value={photos.grouping} options={groupingOptions} onchange={value => relayout(() => photos.grouping = value as "months" | "sessions")} />{/if}
        <SelectDropdown title="Grid density" value={photos.density} options={densityOptions} onchange={value => relayout(() => photos.setDensity(value as Density))} />
      </div>
      {#if album && albums}
        <IconButton ariaLabel={album.starred ? "Unstar album" : "Star album"} title={album.starred ? "Unstar album" : "Star album"} ariaPressed={album.starred} disabled={albums.busy || renaming} onclick={() => void albums!.update(album!, { starred: !album!.starred })}><StarIcon size="16" fill={album.starred ? "currentColor" : "none"} /></IconButton>
        <Menu align="end"><MenuTrigger ariaLabel="Album actions" disabled={albums.busy || renaming}>More</MenuTrigger><MenuContent ariaLabel="Album actions"><MenuItem onselect={beginRename}>Rename</MenuItem><MenuItem onselect={() => openModal("duplicate")}>Duplicate…</MenuItem><MenuItem tone="danger" onselect={() => openModal("delete")}>Delete album…</MenuItem></MenuContent></Menu>
      {/if}<Button size="sm" disabled={photos.loading || photos.hiding || photos.trashing || albums?.busy} onclick={() => void refreshView()}>Refresh previews</Button>
      <Button size="sm" disabled={photos.loading || photos.hiding || photos.trashing || photos.rejectsLoading} onclick={() => { rejectsOpen = true; void photos.previewRejects(); }}>Move rejects</Button>
    </div>
  </div>
  {#if photos.actionError && !onactionerror}<p role="alert">{photos.actionError}</p>{/if}
  {#if searchable}
  <form class="photo-search" onsubmit={event => { event.preventDefault(); void photos.setQuery({ ...photos.baseQuery, text: search, syntax: "simple", sort: { field: search.trim() ? "relevance" : "capture_time", direction: "desc" } }); }}>
    <SearchInput disabled={photos.loading || photos.hiding || photos.trashing} ariaLabel="Search photos" placeholder="Search photos" bind:value={search} />
    <Button type="submit" size="sm" disabled={photos.loading || photos.hiding || photos.trashing}>Search</Button>
    <SelectDropdown disabled={photos.loading || photos.hiding || photos.trashing} title="Sort photos" value={photos.query.sort?.field} options={sortOptions} onchange={value => void photos.setSort({ field: value as "capture_time" | "relevance", direction: "desc" })} />
    {#if filtered}
      <Button type="button" size="sm" disabled={photos.loading || photos.hiding || photos.trashing} onclick={() => { search = ""; void photos.setQuery({ ...photos.baseQuery, text: "", filters: {}, sort: { field: "capture_time", direction: "desc" } }); }}>Clear filters</Button>
    {/if}
  </form>
  {/if}
  {#if photos.date}
    <div class="photo-date-selection"><Button size="sm" onclick={() => void photos.selectDate()}>Clear date</Button></div>
  {/if}
  {#if photos.view === "timeline"}
    <PhotoTimeline facet={photos.timeline} loading={photos.timelineLoading} error={photos.timelineError} selected={photos.date} onselect={date => void photos.selectDate(date)} onretry={() => void photos.loadTimeline()} />
  {/if}
  {#if albums && modal !== "duplicate"}<PhotoAlbumPending {albums} {onnavigate} />{/if}
  {#if albums?.error}<div class="photo-error" role="alert">{albums.error}</div>{/if}
  {#if albums?.notice}<div class="photo-notice" role="status">{albums.notice}{#if albums.noticeID}<a href={`/photos/albums/${albums.noticeID}`} onclick={event => { event.preventDefault(); onnavigate(`/photos/albums/${albums!.noticeID}`); }}>Open album</a>{/if}</div>{/if}
  {#if photos.query.sort?.field === "relevance" && photos.total > photos.items.length}
    <p class="photo-search-limit">Showing the best 250 of {photos.total.toLocaleString()} matches. Refine the search or sort by capture date to see all.</p>
  {/if}
  {#if photos.error}
    <div class="photo-error" role="alert"><span>{photos.error}</span><Button size="sm" onclick={() => void photos.retry(preserve)}>Retry</Button></div>
  {/if}
  <div class="photo-browser">
  {#if searchable}
  <div class="photo-facets">
    {#if photos.facetsLoading}<div class="photo-loading" role="status"><Spinner size={14} />Loading counts…</div>{/if}
    {#if photos.facetsError}<div class="photo-error" role="alert"><span>{photos.facetsError}</span>{#if photos.facetsRetryable}<Button size="sm" onclick={() => void photos.retryFacets()}>Retry counts</Button>{/if}</div>{/if}
    <FacetSidebar disabled={photos.loading || photos.hiding || photos.trashing} facets={photos.facets} query={photos.baseQuery} unit="photos" onchange={value => void photos.setQuery(value)} />
  </div>
  {/if}
  <div class="photo-results">
  {#if albumID && albums && albums.initialized && !albums.loading && !albums.busy && modal !== "delete" && !albums.loadError && !album}<EmptyState title="Album not found"><Button onclick={() => onnavigate("/photos/albums")}>Back to albums</Button></EmptyState>
  {:else if photos.items.length}
    {#key photos.baseQuery}
    <PhotoGrid bind:this={grid} bind:scrollTop={photos.scrollTop} {groups} targetRowHeight={ROW_HEIGHTS[photos.density]} loading={photos.loading || !!photoID} {cache} hidden={photos.hidden} onhidden={id => void setHidden(id)} selectedIDs={photos.selection.selectedIDs} onselect={select} onopen={open} oncheck={(id, checked, range) => photos.check(id, checked, range, orderedIDs)} onloadmore={() => void photos.loadMore(preserve)} ondragstart={albums ? (id, event) => albums?.startDrag(id, event, photos) : undefined} ondragend={albums ? () => { if (albums) albums.drag = undefined; } : undefined} />
    {/key}
  {:else if !photos.loading && !photos.error}
    <EmptyState title={photos.date ? `No photos ${photos.date.length === 10 ? "on this day" : photos.date.length === 7 ? "in this month" : "in this year"}` : filtered ? "No matching photos" : photos.hidden ? "No hidden photos" : albumID ? "This album is empty" : "Your photo library is empty"} description={photos.date ? undefined : filtered ? "Clear a filter or try another search." : photos.hidden ? "Choose Hide from a photo's actions menu in Library." : albumID ? "Go to Library, select photos, then choose Add to album or press B." : "Import photos with docbank photos import to browse them here."}>
      {#snippet icon()}<ImageIcon size="24" />{/snippet}
      {#if albumID && !photos.date}<Button onclick={() => onnavigate("/photos")}>Go to Library</Button>{/if}
    </EmptyState>
  {/if}
  <div class="photo-loading" role="status">{#if photos.loading}<Spinner size={14} />Loading photos…{:else if photos.cursor && !photos.error}<Button size="sm" onclick={() => void photos.loadMore(preserve)}>Load more</Button>{/if}</div>
  </div></div>
  {#if !photoID}
  <SelectionDock context="photos" selectedCount={photos.selection.selectedIDs.size} visibleDocumentCount={photos.items.length} loadedSelected={photos.items.every(item => photos.selection.selectedIDs.has(item.asset_id))} wholeQueryCount={photos.total} allResults={photos.allResults} onallresults={() => photos.selectAllResults()} onclear={() => photos.clearSelection()} onselectvisible={() => photos.selectLoaded()} ontrash={() => { photos.trashError = ""; trashOpen = true; }} trashDisabled={photos.trashing || photos.hiding || photos.loading || photos.allResults}>
    {#snippet photoActions()}
      {#if albums}<PhotoAlbumPicker bind:this={picker} {photos} {albums} />{/if}
      {#if album && albums}<Button size="sm" disabled={albums.busy || !photos.allResults && photos.selection.selectedIDs.size > 1000} onclick={() => void remove()}>Remove from album</Button><Button size="sm" disabled={albums.busy || photos.allResults || photos.selection.selectedIDs.size !== 1} onclick={() => void albums!.cover(album!, [...photos.selection.selectedIDs][0])}>Use as cover</Button>{/if}
    {/snippet}
  </SelectionDock>
  {/if}
  </div>
  {#if photoID}<PhotoLoupe photo={current} error={detailError} note={membershipNote} reloading={detailLoading} {reloadRevision} onreload={() => reload = {id:photoID}} onretry={retryNavigation} {photos} {cache} items={fromGrid ? ordered : []} canLoadMore={fromGrid && !!photos.cursor} onmove={move} onchoose={id => onphotochange(id, "step")} onclose={() => onphotochange("", "close")} />{/if}
</main>
{#if modal && modalAlbum && albums}
  <Modal title={modal === "delete" ? "Delete album" : "Duplicate album"} onclose={() => { if (!albums.busy) modal = undefined; }}>
    {#if modal === "delete"}<p>Delete "{modalAlbum.name}"? Its {albums.loadError || modalAlbum.included_count === undefined ? "" : `${modalAlbum.included_count.toLocaleString()} `}photos stay in your library.</p>{:else}{@render sourceSummary(modalAlbum)}<TextInput ariaLabel="Copy name" bind:value={duplicateName} />{/if}
    {#if modalError}<p role="alert">{modalError}</p>{/if}
    {#if modal === "duplicate"}<PhotoAlbumPending {albums} onnavigate={path => { modal = undefined; onnavigate(path); }} />{/if}
    <div class="modal-actions"><Button disabled={albums.busy} onclick={() => modal = undefined}>Cancel</Button><Button tone={modal === "delete" ? "danger" : "info"} disabled={albums.busy || modal === "duplicate" && (!!albums.unconfirmed || !duplicateName.trim())} onclick={() => void confirm()}>{modal === "delete" ? "Delete album" : "Duplicate album"}</Button></div>
  </Modal>
{/if}

{#if rejectsOpen}
  <PhotoRejectsModal {photos} onclose={() => { photos.cancelRejects(); rejectsOpen = false; }} onmove={async () => { const source = photos; if (await source.trashRejects(preserve, () => ontrashed?.(source))) rejectsOpen = false; }} />
{/if}

{#if trashOpen}
  <Modal title="Move selected photos to trash?" tone="danger" ariaLabel="Move selected photos to trash" onclose={() => { if (!photos.trashing) trashOpen = false; }} closeOnOverlayClick={!photos.trashing}>
    <p>Move {photos.selection.selectedIDs.size} selected photo{photos.selection.selectedIDs.size === 1 ? "" : "s"} and every RAW, image, video, and sidecar member to recoverable trash. Stored files and album membership stay intact.</p>
    {#if photos.trashError}<p role="alert">{photos.trashError} Failed photos remain selected for retry.</p>{/if}
    {#snippet footer()}
      <Button disabled={photos.trashing} onclick={() => trashOpen = false}>Keep in Docbank</Button>
      <Button tone="danger" disabled={photos.trashing || photos.selection.selectedIDs.size === 0} onclick={async () => { const source = photos; if (await source.trashSelected(preserve, () => ontrashed?.(source))) trashOpen = false; }}>{photos.trashing ? "Moving…" : "Move to trash"}</Button>
    {/snippet}
  </Modal>
{/if}

<style>
  .library-grid { display: flex; flex-direction: column; flex: 1; min-height: 0; }
  .photos-workspace { position: relative; max-height: calc(100dvh - var(--header-height)); display: flex; flex-direction: column; flex: 1; min-height: 0; overflow: hidden; background: var(--bg-surface); }
  .photo-search { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); padding: var(--space-3) var(--space-5); border-bottom: 1px solid var(--border-default); }
  .photo-search-limit { margin: 0; padding: var(--space-3) var(--space-5); color: var(--text-muted); font-size: var(--font-size-sm); }
  .photo-browser { display: flex; flex: 1; min-height: 0; overflow: hidden; }
  .photo-facets { display: flex; flex-direction: column; width: 230px; flex-shrink: 0; overflow: auto; }
  .photo-facets :global(.facets) { flex: 1; }
  .photo-results { display: flex; flex-direction: column; flex: 1; min-width: 0; min-height: 0; }
  @media (max-width: 640px) { .photo-browser { flex-direction: column; } .photo-facets { width: auto; max-height: 180px; } }
  .photo-toolbar { border-bottom: 1px solid var(--border-default); }
  .photo-date-selection { display: flex; gap: var(--space-3); align-items: center; padding: var(--space-2) var(--space-5); font-size: var(--font-size-sm); color: var(--text-primary); }
  .library-title h1 { margin: 0 0 4px; font-size: var(--font-size-lg); color: var(--text-primary); }
  .library-title span { font-size: var(--font-size-xs); color: var(--text-muted); }
  .photo-options { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .photo-views { display: flex; gap: var(--space-2); }
  .photo-error { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-3) var(--space-5); color: var(--text-primary); background: var(--bg-inset); }
  .photo-notice { display: flex; flex-wrap: wrap; gap: var(--space-3); padding: var(--space-2) var(--space-5); font-size: var(--font-size-sm); }
  .photo-notice a { color: var(--accent-blue); }
  .album-back { border: 0; padding: 0; background: transparent; color: var(--accent-blue); font-size: var(--font-size-xs); cursor: pointer; margin-bottom: 5px; }
  form, .modal-actions { display: flex; gap: var(--space-2); align-items: center; flex-wrap: wrap; }
  .modal-actions { justify-content: flex-end; margin-top: var(--space-4); }
  @media (max-width: 640px) { .photo-toolbar { flex-wrap: wrap; gap: var(--space-3); } }
  .photo-loading { height: 38px; flex-shrink: 0; display: flex; gap: var(--space-2); align-items: center; justify-content: center; padding: var(--space-2); color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
