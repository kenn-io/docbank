<script lang="ts">
  import { onMount, untrack } from "svelte";
  import L from "leaflet";
  import "leaflet/dist/leaflet.css";
  import "leaflet.markercluster";
  import "leaflet.markercluster/dist/MarkerCluster.css";
  import { type MapTiles } from "./photoMap.js";
  import { APIError, sessionResponse } from "./api-transport.js";
  import type { PhotoMapRow } from "./generated/docbank.js";
  import type { MapView, PhotoRow } from "./photos.svelte.js";
  let {
    items,
    appendedFrom,
    tiles,
    session,
    onauthfailure,
    view,
    onopen,
    onvisible,
    onview,
  }: {
    items: PhotoMapRow[];
    appendedFrom?: PhotoRow[];
    tiles: MapTiles;
    session: string;
    onauthfailure?: (cause: unknown) => void;
    view?: MapView;
    onopen: (id: string, element: HTMLElement) => void;
    onvisible: (rows: PhotoMapRow[]) => void;
    onview?: (view: MapView) => void;
  } = $props();
  let container: HTMLDivElement;
  let map: L.Map | undefined;
  let cluster: L.MarkerClusterGroup | undefined;
  let frame: number | undefined;
  let tileError = $state(false);
  const failedTiles = new Set<string>();
  let scanned: PhotoMapRow[] = [];
  let inView: PhotoMapRow[] = [];
  const markers = new Map<string, L.Marker>();
  let built: PhotoMapRow[] = [];
  let pending: PhotoMapRow[] = [];
  let offset = 0;
  function cancelBuild() {
    if (frame !== undefined) L.Util.cancelAnimFrame(frame);
    frame = undefined;
  }
  function visible(incremental = false) {
    if (!map) return;
    const bounds = map.getPixelBounds();
    const append = incremental && appendedFrom === scanned;
    const hits = (append ? items.slice(scanned.length) : items).filter(
      (row) =>
        row.latitude != null &&
        row.longitude != null &&
        bounds.contains(map!.project([row.latitude, row.longitude])),
    );
    inView = append ? [...inView, ...hits] : hits;
    scanned = items;
    onvisible(inView);
  }
  function build(photos = items) {
    if (!cluster || !map) return;
    cancelBuild();
    const append = appendedFrom === built;
    const removed: L.Marker[] = [];
    if (!append) {
      pending = [];
      offset = 0;
      const ids = new Set(photos.map((row) => row.asset_id));
      for (const [id, marker] of markers)
        if (!ids.has(id)) {
          removed.push(marker);
          markers.delete(id);
        }
    }
    for (const row of append ? photos.slice(built.length) : photos) {
      const marker = markers.get(row.asset_id);
      const position = marker?.getLatLng();
      if (
        !marker ||
        row.latitude == null ||
        row.longitude == null ||
        position?.lat !== row.latitude ||
        position?.lng !== row.longitude ||
        marker.options.title !== row.name
      ) {
        if (marker) {
          removed.push(marker);
          markers.delete(row.asset_id);
        }
        pending.push(row);
      }
    }
    built = photos;
    cluster.removeLayers(removed);
    const target = cluster;
    function batch() {
      frame = undefined;
      const layers: L.Marker[] = [],
        end = Math.min(offset + 200, pending.length);
      for (; offset < end; offset++) {
        const photo = pending[offset]!;
        if (photo.latitude == null || photo.longitude == null) continue;
        const marker = L.marker([photo.latitude, photo.longitude], {
          title: photo.name,
          alt: photo.name,
          autoPanOnFocus: false,
          icon: L.divIcon({ className: "photo-map-dot", iconSize: [16, 16] }),
        });
        marker.on("keypress", (event) => {
          if (event.originalEvent.key === "Enter" || event.originalEvent.key === " ") {
            event.originalEvent.preventDefault();
            const element = marker.getElement();
            if (element) onopen(photo.asset_id, element);
          }
        });
        marker.on("click", () => {
          const element = marker.getElement();
          if (element) onopen(photo.asset_id, element);
        });
        marker.on("add", () => {
          const element = marker.getElement();
          if (element) element.dataset.mapAsset = photo.asset_id;
        });
        markers.set(photo.asset_id, marker);
        layers.push(marker);
      }
      target.addLayers(layers);
      if (offset < pending.length) frame = L.Util.requestAnimFrame(batch);
      else {
        pending = [];
        offset = 0;
      }
    }
    if (offset < pending.length) frame = L.Util.requestAnimFrame(batch);
    visible(true);
  }
  $effect(() => {
    void items;
    untrack(build);
    return cancelBuild;
  });
  export function focus(id: string) {
    const marker = markers.get(id);
    const element = marker ? cluster?.getVisibleParent(marker)?.getElement() : undefined;
    (element ?? container).focus({ preventScroll: true });
  }
  onMount(() => {
    const bounds: L.LatLngBoundsExpression = [
      [-90, -180],
      [90, 180],
    ];
    map = L.map(container, {
      maxZoom: tiles.max_zoom,
      maxBounds: bounds,
      maxBoundsViscosity: 1,
    }).setView(view ? [view.lat, view.lng] : [20, 0], view?.zoom ?? 2);
    const attribution = document.createElement("span");
    attribution.textContent = tiles.attribution;
    const tileKey = ({ coords }: L.TileEvent) => `${coords.z}/${coords.x}/${coords.y}`;
    const clearTile = (event: L.TileEvent) => {
      failedTiles.delete(tileKey(event));
      tileError = failedTiles.size > 0;
    };
    // Tiles load through the daemon with the browser session, so the provider key stays server-side.
    const disposers = new Map<HTMLElement, () => void>();
    const SessionTiles = L.TileLayer.extend({
      createTile(this: L.TileLayer, coords: L.Coords, done: L.DoneCallback) {
        const tile = document.createElement("img"),
          controller = new AbortController();
        let blobURL = "",
          disposed = false;
        const release = () => {
          if (blobURL) URL.revokeObjectURL(blobURL);
          blobURL = "";
        };
        disposers.set(tile, () => {
          disposed = true;
          controller.abort();
          release();
        });
        // Leaflet overwrites onload/onerror when it abandons a tile, so listen instead.
        tile.alt = "";
        tile.addEventListener("load", () => {
          if (!disposed) {
            release();
            done(undefined, tile);
          }
        });
        tile.addEventListener("error", () => {
          if (!disposed) {
            release();
            done(new Error("tile image failed"), tile);
          }
        });
        sessionResponse(this.getTileUrl(coords), {
          session,
          signal: controller.signal,
          headers: { Accept: "image/*" },
        })
          .then((response) => response.blob())
          .then((blob) => {
            if (!disposed) {
              blobURL = URL.createObjectURL(blob);
              tile.src = blobURL;
            }
          })
          .catch((cause) => {
            if (disposed) return;
            if (cause instanceof APIError && (cause.status === 401 || cause.status === 403))
              onauthfailure?.(cause);
            else done(cause as Error, tile);
          });
        return tile;
      },
    }) as new (url: string, options: L.TileLayerOptions) => L.TileLayer;
    const dispose = (event: L.TileEvent) => {
      disposers.get(event.tile)?.();
      disposers.delete(event.tile);
    };
    new SessionTiles(tiles.tile_url, {
      attribution: attribution.innerHTML,
      maxZoom: tiles.max_zoom,
      noWrap: true,
      bounds,
    })
      .on("tileerror", (event) => {
        failedTiles.add(tileKey(event));
        tileError = true;
      })
      .on("tileload", clearTile)
      .on("tileabort", dispose)
      .on("tileunload", (event) => {
        dispose(event);
        clearTile(event);
      })
      .addTo(map);
    cluster = L.markerClusterGroup({
      showCoverageOnHover: false,
      iconCreateFunction: (group) =>
        L.divIcon({
          className: "photo-map-cluster",
          html: String(group.getChildCount()),
          iconSize: [36, 36],
        }),
    });
    cluster.on("clusterkeypress", (event) => {
      if (
        "originalEvent" in event &&
        event.originalEvent instanceof KeyboardEvent &&
        event.originalEvent.key === " "
      ) {
        event.originalEvent.preventDefault();
        cluster?.fire("click", event);
      }
    });
    cluster.addTo(map);
    build();
    map.on("moveend", () => {
      if (!map) return;
      visible();
      const center = map.getCenter();
      onview?.({ lat: center.lat, lng: center.lng, zoom: map.getZoom() });
    });
    const observer = new ResizeObserver(() => {
      if (container.clientWidth && container.clientHeight) map?.invalidateSize();
    });
    observer.observe(container);
    return () => {
      cancelBuild();
      observer.disconnect();
      map?.remove();
      map = undefined;
      cluster = undefined;
      markers.clear();
    };
  });
</script>

<div class="photo-map-frame">
  {#if tileError}<div class="photo-error" role="alert">
      Map tiles from {tiles.host} couldn't load. Check [web.map_tiles], then restart Docbank.
    </div>{/if}
  <div bind:this={container} class="photo-map" aria-label="Photo map"></div>
</div>

<style>
  .photo-map-frame {
    position: relative;
    height: 100%;
    width: 100%;
  }
  .photo-error {
    position: absolute;
    left: 60px;
    right: var(--space-3);
    z-index: 1;
    padding: var(--space-3) var(--space-5);
    color: var(--text-primary);
    background: var(--bg-inset);
  }
  .photo-map {
    height: 100%;
    width: 100%;
    z-index: 0;
  }
  :global(.photo-map-dot) {
    background: var(--accent-blue);
    border: 2px solid white;
    border-radius: 50%;
  }
  :global(.photo-map-cluster) {
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: 50%;
    color: var(--text-primary);
    background: var(--bg-surface);
    border: 2px solid var(--accent-blue);
    font-weight: 600;
  }
</style>
