<script lang="ts">
  import { onMount, untrack } from "svelte";
  import L from "leaflet";
  import "leaflet/dist/leaflet.css";
  import "leaflet.markercluster";
  import "leaflet.markercluster/dist/MarkerCluster.css";
  import { type MapTiles } from "./photoMap.js";
  import type { PhotoMapRow } from "./generated/docbank.js";
  import type { MapView } from "./photos.svelte.js";
  let { items, complete = true, tiles, view, onopen, onvisible, onview }: { items: PhotoMapRow[]; complete?: boolean; tiles: MapTiles; view?: MapView; onopen: (id: string, element: HTMLElement) => void; onvisible: (rows: PhotoMapRow[]) => void; onview?: (view: MapView) => void } = $props();
  let container: HTMLDivElement;
  let map: L.Map | undefined;
  let cluster: L.MarkerClusterGroup | undefined;
  let frame: number | undefined;
  const markers = new Map<string, L.Marker>();
  function cancelBuild() { if (frame !== undefined) L.Util.cancelAnimFrame(frame); frame = undefined; }
  function visible() {
    if (!map || !complete) return;
    const bounds = map.getPixelBounds();
    onvisible(items.filter(row => row.latitude != null && row.longitude != null && bounds.contains(map!.project([row.latitude,row.longitude]))));
  }
  function build(photos = items) {
    if (!cluster || !map) return;
    cancelBuild();
    const ids = new Set(photos.map(row => row.asset_id)), removed: L.Marker[] = [], pending: PhotoMapRow[] = [];
    for (const [id, marker] of markers) if (!ids.has(id)) { removed.push(marker); markers.delete(id); }
    for (const row of photos) {
      const marker = markers.get(row.asset_id);
      const position = marker?.getLatLng();
      if (!marker || row.latitude == null || row.longitude == null || position?.lat !== row.latitude || position?.lng !== row.longitude || marker.options.title !== row.name) {
        if (marker) { removed.push(marker); markers.delete(row.asset_id); }
        pending.push(row);
      }
    }
    cluster.removeLayers(removed);
    const target = cluster;
    let offset = 0;
    function batch() {
      frame = undefined;
      const layers: L.Marker[] = [], end = Math.min(offset + 200, pending.length);
      for (; offset < end; offset++) {
        const photo = pending[offset]!;
        if (photo.latitude == null || photo.longitude == null) continue;
        const marker = L.marker([photo.latitude, photo.longitude], { title: photo.name, alt: photo.name, icon: L.divIcon({ className: "photo-map-dot", iconSize: [16,16] }) });
        marker.on("keypress", event => { if (event.originalEvent.key === "Enter" || event.originalEvent.key === " ") { event.originalEvent.preventDefault(); const element = marker.getElement(); if (element) onopen(photo.asset_id, element); } });
        marker.on("click", () => { const element = marker.getElement(); if (element) onopen(photo.asset_id, element); });
        marker.on("add", () => { const element = marker.getElement(); if (element) element.dataset.mapAsset = photo.asset_id; });
        markers.set(photo.asset_id, marker);
        layers.push(marker);
      }
      target.addLayers(layers);
      if (offset < pending.length) frame = L.Util.requestAnimFrame(batch);
    }
    if (pending.length) frame = L.Util.requestAnimFrame(batch);
    visible();
  }
  $effect(() => { for (const row of items) { void row.latitude; void row.longitude; void row.name; } void complete; untrack(build); return cancelBuild; });
  export function focus(id: string) {
    const marker = markers.get(id);
    const element = marker ? cluster?.getVisibleParent(marker)?.getElement() : undefined;
    (element ?? container).focus({ preventScroll: true });
  }
  onMount(() => {
    const bounds: L.LatLngBoundsExpression = [[-90,-180],[90,180]];
    map = L.map(container, { maxZoom: tiles.max_zoom, maxBounds: bounds, maxBoundsViscosity: 1 }).setView(view ? [view.lat, view.lng] : [20,0], view?.zoom ?? 2);
    const attribution = document.createElement("span"); attribution.textContent = tiles.attribution;
    L.tileLayer(tiles.tile_url, { attribution: attribution.innerHTML, maxZoom: tiles.max_zoom, noWrap: true, bounds }).addTo(map);
    cluster = L.markerClusterGroup({ showCoverageOnHover: false, iconCreateFunction: group => L.divIcon({ className: "photo-map-cluster", html: String(group.getChildCount()), iconSize: [36,36] }) });
    cluster.on("clusterkeypress", event => {
      if ("originalEvent" in event && event.originalEvent instanceof KeyboardEvent && event.originalEvent.key === " ") { event.originalEvent.preventDefault(); cluster?.fire("click", event); }
    });
    cluster.addTo(map); build();
    map.on("moveend", () => { if (!map) return; visible(); const center = map.getCenter(); onview?.({ lat: center.lat, lng: center.lng, zoom: map.getZoom() }); });
    const observer = new ResizeObserver(() => { if (container.clientWidth && container.clientHeight) map?.invalidateSize(); }); observer.observe(container);
    return () => { cancelBuild(); observer.disconnect(); map?.remove(); map = undefined; cluster = undefined; markers.clear(); };
  });
</script>
<div bind:this={container} class="photo-map" aria-label="Photo map"></div>
<style>
  .photo-map { height: 100%; width: 100%; z-index: 0; }
  :global(.photo-map-dot) { background: var(--accent-blue); border: 2px solid white; border-radius: 50%; }
  :global(.photo-map-cluster) { display: flex; align-items: center; justify-content: center; border-radius: 50%; color: var(--text-primary); background: var(--bg-surface); border: 2px solid var(--accent-blue); font-weight: 600; }
</style>
