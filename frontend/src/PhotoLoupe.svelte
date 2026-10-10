<script lang="ts">
  import { tick, untrack } from "svelte";
  import { Button, IconButton, Spinner, trapFocus } from "@kenn-io/kit-ui";
  import X from "@lucide/svelte/icons/x";
  import ChevronLeft from "@lucide/svelte/icons/chevron-left";
  import ChevronRight from "@lucide/svelte/icons/chevron-right";
  import Info from "@lucide/svelte/icons/info";
  import RotateCcw from "@lucide/svelte/icons/rotate-ccw";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import { isAppShortcutSuppressed } from "./shortcuts.js";
  import { APIError } from "./api-transport.js";
  import { previewEligibility } from "./download.js";
  import PhotoCell from "./PhotoCell.svelte";
  import PhotoLoupeImage from "./PhotoLoupeImage.svelte";

  let { photo, error, note = "", photos, cache, items, canLoadMore, onmove, onchoose, onclose, onretry, onreload, reloading = false, reloadRevision = 0 }: {
    photo?: PhotoBrowseRow; error: string; note?: string; photos: Photos; cache: PhotoPreviewCache;
    items: PhotoBrowseRow[]; canLoadMore: boolean; onmove: (direction: -1 | 1) => void; onchoose: (id: string) => void; onclose: () => void; onretry: () => void; onreload?: () => void; reloading?: boolean; reloadRevision?: number;
  } = $props();
  let info = $state(false);
  let url = $state("");
  let quality = $state("");
  let failure = $state<{rank:number; message:string; action?:"reload" | "fit" | "large"}>();
  let retrying = $state(false);
  const unavailable = $derived(!retrying && !!(error || failure));
  let busy = $state(false);
  let image = $state<PhotoLoupeImage>();
  let dialog = $state<HTMLDivElement>();
  let controller: AbortController;
  let urls: string[] = [];
  let rank = -1;
  let largeStarted = false;
  const identity = $derived(photo ? `${photo.asset_id}:${photo.content_version_id}` : "");
  const index = $derived(items.findIndex(item => item.asset_id === photo?.asset_id));
  const rail = $derived(index < 0 ? [] : items.slice(Math.max(0, index - 4), index + 5));
  const previous = $derived(index > 0);
  const next = $derived(index >= 0 && (index < items.length - 1 || canLoadMore && !photos.error && !photos.loading));
  const originalAllowed = $derived.by(() => {
    try { return !!photo && previewEligibility(photo.media_type, 0).kind === "image"; } catch { return false; }
  });
  const orientations = ["", "Normal", "Mirrored horizontally", "Rotated 180°", "Mirrored vertically", "Mirrored horizontally, rotated 270°", "Rotated 90° clockwise", "Mirrored horizontally, rotated 90°", "Rotated 270° clockwise"];
  const facts = $derived(photo ? [
    ["Captured", photo.capture_time], ["Dimensions", photo.width_px && photo.height_px ? `${photo.width_px} × ${photo.height_px}` : undefined],
    ["Camera", [photo.camera_make, photo.camera_model].filter(Boolean).join(" ")],
    ["Lens", [photo.lens_make, photo.lens_model].filter(Boolean).join(" ")],
    ["ISO", photo.iso], ["Aperture", photo.f_number == null ? undefined : `f/${photo.f_number}`],
    ["Exposure", photo.exposure_time_seconds == null ? undefined : photo.exposure_time_seconds > 0 && photo.exposure_time_seconds < 1 && Math.abs(1 / photo.exposure_time_seconds - Math.round(1 / photo.exposure_time_seconds)) <= 1e-6 / photo.exposure_time_seconds ? `1/${Math.round(1 / photo.exposure_time_seconds)} s` : `${photo.exposure_time_seconds} s`],
    ["Exposure bias", photo.exposure_bias_ev == null ? undefined : `${photo.exposure_bias_ev} EV`],
    ["Focal length", photo.focal_length_mm == null ? undefined : `${photo.focal_length_mm} mm`],
    ["Orientation", photo.orientation == null ? undefined : orientations[photo.orientation]], ["Format", photo.media_type],
  ].filter(([, value]) => value !== undefined && value !== null && value !== "") : []);

  $effect(() => {
    void identity;
    void reloadRevision;
    const selected = untrack(() => photo);
    url = ""; quality = ""; failure = undefined; busy = false; retrying = false;
    urls = []; rank = -1; largeStarted = false;
    controller = new AbortController();
    const active = controller;
    if (!selected) return () => active.abort();
    void load(selected, "grid", active);
    const timer = setTimeout(() => void load(selected, "fit", active), 150);
    void tick().then(() => { if (!active.signal.aborted && image?.isZoomed()) zoom(); });
    const ownedURLs = urls;
    return () => { clearTimeout(timer); active.abort(); for (const objectURL of ownedURLs.splice(0)) URL.revokeObjectURL(objectURL); };
  });
  $effect(() => {
    const neighbors = index < 0 ? [] : items.slice(Math.max(0, index - 2), index + 3).filter(item => item.asset_id !== photo?.asset_id);
    const active = new AbortController();
    const timer = setTimeout(async () => {
      for (const neighbor of neighbors) {
        if (active.signal.aborted) break;
        const signal = AbortSignal.any([active.signal, AbortSignal.timeout(30_000)]);
        try {
          const slot = await photos.preview(neighbor, "fit", signal);
          if (slot.state === "ready" && slot.generation_id) await cache.get(neighbor.asset_id, slot.generation_id, signal);
        } catch {}
      }
    }, 150);
    return () => { clearTimeout(timer); active.abort(); };
  });
  async function show(blobURL: string, level: number, label: string, active: AbortController) {
    const ownedURLs = urls;
    ownedURLs.push(blobURL);
    try {
      const decoded = new Image(); decoded.src = blobURL;
      await decoded.decode();
    } catch (cause) {
      const index = ownedURLs.indexOf(blobURL);
      if (index >= 0) { ownedURLs.splice(index, 1); URL.revokeObjectURL(blobURL); }
      throw cause;
    }
    if (!active.signal.aborted && level >= rank) { rank = level; url = blobURL; quality = label; if (failure && failure.rank <= level) failure = undefined; }
  }
  function fail(result: NonNullable<typeof failure>, active: AbortController) {
    untrack(() => { if (!active.signal.aborted && result.rank >= rank && result.rank >= (failure?.rank ?? -1)) failure = result; });
  }
  async function load(selected: PhotoBrowseRow, size: "grid" | "fit" | "large", active: AbortController, reload = false) {
    if (size === "large") largeStarted = true;
    const signal = AbortSignal.any([active.signal, AbortSignal.timeout(30_000)]);
    const level = size === "grid" ? 0 : size === "fit" ? 1 : 2;
    try {
      const slot = size === "grid" ? selected.previews.grid : await photos.preview(selected, size, signal);
      if (slot.state !== "ready" || !slot.generation_id) {
        if (slot.state !== "missing") fail({rank:level, message:slot.state === "unsupported" ? "Preview unsupported. Download this photo from Documents." : "Preview unavailable for this version. Download this photo from Documents."}, active);
        return;
      }
      const blob = await cache.get(selected.asset_id, slot.generation_id, signal, reload);
      if (active.signal.aborted) return;
      const objectURL = URL.createObjectURL(blob);
      await show(objectURL, level, size, active);
    } catch (cause) {
      const reload = cause instanceof APIError && (cause.code === "photo_display_changed" || cause.status === 404);
      fail({rank:level, message:cause instanceof Error ? cause.message : String(cause), action:reload ? "reload" : size === "grid" ? undefined : size}, active);
      if (!active.signal.aborted && size === "large") largeStarted = false;
    }
  }
  function zoom() {
    if (!photo || largeStarted || quality === "original") return;
    void load(photo, "large", controller);
  }
  async function retry() {
    if (retrying || reloading || !photo || (failure?.action !== "fit" && failure?.action !== "large")) return;
    const selected = photo, action = failure.action, active = controller;
    dialog?.focus({preventScroll:true});
    retrying = true;
    try { await load(selected, action, active, true); }
    finally { if (active === controller && !active.signal.aborted) retrying = false; }
  }
  async function original() {
    if (!photo || busy) return;
    dialog?.focus({preventScroll:true});
    const selected = photo, active = controller;
    busy = true;
    try {
      const objectURL = await photos.original(selected, AbortSignal.any([active.signal, AbortSignal.timeout(30_000)]));
      if (active.signal.aborted) { URL.revokeObjectURL(objectURL); return; }
      await show(objectURL, 3, "original", active);
    } catch (cause) { fail({rank:3, message:cause instanceof Error ? cause.message : String(cause), action:cause instanceof APIError && cause.code === "photo_display_changed" ? "reload" : undefined}, active); }
    finally { if (!active.signal.aborted) busy = false; }
  }
  function move(direction: -1 | 1) { dialog?.focus({preventScroll:true}); onmove(direction); }
  function key(event: KeyboardEvent) {
    if (event.altKey || event.ctrlKey || event.metaKey || isAppShortcutSuppressed(event, false, event.currentTarget as HTMLElement, ".loupe-toolbar button, .edge button, .filmstrip button, .loupe-error button")) return;
    if (event.key === "Escape") { event.preventDefault(); onclose(); }
    else if (event.key === "ArrowLeft" && previous) { event.preventDefault(); move(-1); }
    else if (event.key === "ArrowRight" && next) { event.preventDefault(); move(1); }
    else if (event.key.toLowerCase() === "i") { event.preventDefault(); info = !info; }
    else if (event.key === "0") { event.preventDefault(); image?.reset(); }
  }
</script>

<div bind:this={dialog} class="photo-loupe" role="dialog" aria-modal="true" aria-label={photo ? `Photo viewer, ${photo.name}` : "Photo viewer"} tabindex="-1" onkeydown={key} {@attach trapFocus}>
  <header class="loupe-toolbar">
    <IconButton ariaLabel="Close photo viewer" onclick={onclose}><X size={18} /></IconButton>
    <div class="caption"><strong>{photo?.name ?? "Photo"}</strong>{#if index >= 0}<span>{index + 1} of {photos.total.toLocaleString()}</span>{/if}</div>
    <div class="mobile-navigation"><IconButton ariaLabel="Previous photo" disabled={!previous} onclick={() => move(-1)}><ChevronLeft size={18} /></IconButton><IconButton ariaLabel="Next photo" disabled={!next} onclick={() => move(1)}><ChevronRight size={18} /></IconButton></div>
    <Button size="sm" disabled={!originalAllowed || busy || quality === "original"} onclick={original}>{#if busy}<Spinner size={14} />{/if}View original</Button>
    <IconButton ariaLabel="Reset zoom" onclick={() => image?.reset()}><RotateCcw size={18} /></IconButton>
    <IconButton ariaLabel="Photo information" ariaPressed={info} onclick={() => info = !info}><Info size={18} /></IconButton>
  </header>
  {#if items.length && photos.error}<div class="loupe-error" role="alert"><span>{photos.error}</span><Button size="sm" disabled={photos.loading} onclick={() => { dialog?.focus({preventScroll:true}); onretry(); }}>Retry navigation</Button></div>{/if}
  {#if note || error || failure}<div class="loupe-error" role="alert">{#if note}<span>{note}</span>{/if}{#if error}<span>{error}</span>{/if}{#if failure}<span>{failure.message}</span>{/if}{#if note || error || failure?.action === "reload"}<Button size="sm" disabled={reloading} onclick={() => { dialog?.focus({preventScroll:true}); onreload?.(); }}>Reload photo</Button>{/if}{#if (failure?.action === "fit" || failure?.action === "large") && photo}<Button size="sm" disabled={retrying || reloading} onclick={retry}>Retry preview</Button>{/if}</div>{/if}
  <div class="loupe-body">
    <div class="stage">
      <div class="edge previous"><IconButton ariaLabel="Previous photo" disabled={!previous} onclick={() => move(-1)}><ChevronLeft size={24} /></IconButton></div>
      {#key identity}<PhotoLoupeImage bind:this={image} {url} {unavailable} name={photo?.name ?? "Photo"} onzoom={zoom} onmove={move} />{/key}
      <div class="edge next"><IconButton ariaLabel="Next photo" disabled={!next} onclick={() => move(1)}><ChevronRight size={24} /></IconButton></div>
      {#if quality}<span class="quality" aria-live="polite">{quality === "original" ? "Original" : `${quality[0]!.toUpperCase()}${quality.slice(1)} preview`}</span>{/if}
    </div>
    {#if info}<aside class="photo-info" aria-label="Photo details"><h2>Photo details</h2><dl>{#each facts as [label, value]}<dt>{label}</dt><dd>{value}</dd>{/each}</dl></aside>{/if}
  </div>
  {#if rail.length}<nav class="filmstrip" aria-label="Photos in current result">{#each rail as item (item.asset_id)}<div class="thumb"><PhotoCell compact photo={item} {cache} selected={item.asset_id === photo?.asset_id} onclick={() => onchoose(item.asset_id)} /></div>{/each}</nav>{/if}
</div>
<style>
  .photo-loupe { position: absolute; inset: 0; z-index: 10; display: flex; flex-direction: column; background: var(--bg-surface); color: var(--text-primary); outline: none; }
  .loupe-toolbar { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-3) var(--space-4); border-bottom: 1px solid var(--border-default); }
  .caption { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 4px; }
  .caption strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: var(--font-size-sm); }
  .caption span, .quality { color: var(--text-muted); font-size: var(--font-size-xs); }
  .loupe-body { flex: 1; min-height: 0; display: flex; }
  .stage { position: relative; flex: 1; min-width: 0; min-height: 0; display: flex; background: var(--bg-inset); }
  .edge { position: absolute; top: 50%; transform: translateY(-50%); z-index: 1; background: var(--bg-surface); border-radius: var(--radius-sm); }
  .previous { left: 12px; } .next { right: 12px; }
  .quality { position: absolute; bottom: 12px; left: 50%; transform: translateX(-50%); background: var(--bg-surface); padding: 4px 8px; border-radius: var(--radius-sm); }
  .photo-info { width: 320px; flex-shrink: 0; padding: var(--space-5); overflow-y: auto; border-left: 1px solid var(--border-default); }
  h2 { margin-top: 0; font-size: var(--font-size-sm); }
  dl { margin: 0; font-size: var(--font-size-sm); } dt { color: var(--text-muted); margin-top: 16px; } dd { margin: 4px 0 0; overflow-wrap: anywhere; }
  .filmstrip { display: flex; gap: 8px; padding: 12px; justify-content: center; overflow: hidden; border-top: 1px solid var(--border-default); }
  .thumb { flex-shrink: 0; width: 72px; height: 54px; }
  .loupe-error { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; padding: 8px 16px; font-size: var(--font-size-sm); background: var(--bg-inset); }
  .mobile-navigation { display: none; }
  @media (max-width: 760px) {
    .loupe-toolbar { flex-wrap: wrap; gap: 8px; padding: 8px; } .caption { min-width: 100px; } .mobile-navigation { display: flex; } .edge, .filmstrip { display: none; }
    .loupe-body { flex-direction: column; } .stage { min-height: 180px; } .photo-info { width: auto; max-height: 35%; border-left: 0; border-top: 1px solid var(--border-default); padding: 12px; }
  }
</style>
