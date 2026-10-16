<script lang="ts">
  import { photoObjectURL, type PhotoPreviewCache } from "./photoPreviewCache.js";
  import type { PhotoFaces } from "./photoFaces.svelte.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import { photoRotationStyle, observePhotoSize } from "./photoRotation.js";
  let {
    photo,
    faces,
    cache,
  }: { photo: PhotoBrowseRow; faces: PhotoFaces; cache: PhotoPreviewCache } = $props();
  let tiles = $state.raw<{ id: string; label: string; url: string; error: string }[]>([]);
  let width = $state(128),
    height = $state(128);
  $effect(() => {
    const selected = photo,
      generation = photo.previews.fit.generation_id;
    const entries = [
      ...faces.assertions
        .filter((row) => row.photo_mark && row.action === "assert")
        .map((row) => ({
          id: row.assertion_id,
          label: row.display_name,
          assertion_id: row.assertion_id,
        })),
      ...[...faces.faces]
        .sort(
          (a, b) =>
            b.box.width * b.box.height - a.box.width * a.box.height ||
            a.face_id.localeCompare(b.face_id),
        )
        .map((face) => ({ id: face.face_id, label: faces.label(face), face_id: face.face_id })),
    ];
    tiles = entries.map((entry) => ({ id: entry.id, label: entry.label, url: "", error: "" }));
    if (!generation || faces.version !== selected.content_version_id) return;
    const releases = entries.map((entry) =>
      photoObjectURL(
        (signal) =>
          cache.getFaceCrop(
            selected.asset_id,
            {
              content_version_id: selected.content_version_id,
              generation_id: generation,
              ...("face_id" in entry
                ? { face_id: entry.face_id }
                : { assertion_id: entry.assertion_id }),
            },
            signal,
          ),
        (url) => {
          tiles = tiles.map((tile) => (tile.id === entry.id ? { ...tile, url } : tile));
        },
        (cause) => {
          tiles = tiles.map((tile) =>
            tile.id === entry.id
              ? { ...tile, error: cause instanceof Error ? cause.message : String(cause) }
              : tile,
          );
        },
      ),
    );
    return () => releases.forEach((release) => release());
  });
</script>

{#if tiles.length || faces.error}<section aria-label="Face closeups" class="closeups">
    {#each tiles as tile (tile.id)}<figure>
        <div
          class="crop"
          {@attach (element) =>
            observePhotoSize(element, (w, h) => {
              width = w;
              height = h;
            })}
        >
          {#if tile.url}<img
              src={tile.url}
              alt={tile.label}
              style={photoRotationStyle(photo.rotation, width, height)}
            />{:else}<span role={tile.error ? "alert" : "status"}
              >{tile.error || "Loading closeup…"}</span
            >{/if}
        </div>
        <figcaption>{tile.label}</figcaption>
      </figure>{/each}
    {#if faces.error}<span role="alert">{faces.error}</span>{/if}
  </section>{/if}

<style>
  .closeups {
    display: flex;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow-x: auto;
  }
  figure {
    margin: 0;
    flex: 0 0 128px;
  }
  .crop {
    position: relative;
    width: 128px;
    height: 128px;
    overflow: hidden;
    background: var(--bg-inset);
  }
  img {
    position: absolute;
    top: 50%;
    left: 50%;
    object-fit: contain;
  }
  figcaption,
  span {
    font-size: var(--font-size-xs);
  }
</style>
