<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import type { PhotoFaces } from "./photoFaces.svelte.js";
  import type { PhotoBrowseRow, FaceBox } from "./generated/docbank.js";
  import { photoFaceCrop, photoRotationTransform, rotatedDimensions } from "./photoRotation.js";
  let {
    photo,
    faces,
    fit,
  }: {
    photo: PhotoBrowseRow;
    faces: PhotoFaces;
    fit?: { url: string; content_version_id: string; width: number; height: number };
  } = $props();
  const source = $derived(
    fit?.content_version_id === faces.version && faces.version === photo.content_version_id
      ? fit
      : undefined,
  );
  const tiles = $derived([
    ...faces.assertions
      .filter((row) => row.photo_mark && row.action === "assert")
      .map((row) => ({
        id: row.assertion_id,
        label: faces.personLabel(row.person_id),
        box: undefined as FaceBox | undefined,
      })),
    ...faces.ordered.map((face) => ({
      id: face.face_id,
      label: faces.person(face)
        ? `${faces.number(face)} · ${faces.label(face)}`
        : faces.label(face),
      box: face.box,
    })),
  ]);
  function style(box?: FaceBox) {
    if (!source) return "";
    const size = rotatedDimensions(source.width, source.height, photo.rotation);
    const crop = box ? photoFaceCrop(box, source.width, source.height, photo.rotation) : undefined;
    const scale = 128 / (crop?.side ?? Math.max(size.width, size.height));
    const left = crop ? 64 + (size.width / 2 - crop.x - crop.side / 2) * scale : 64;
    const top = crop ? 64 + (size.height / 2 - crop.y - crop.side / 2) * scale : 64;
    return `width:${source.width * scale}px;height:${source.height * scale}px;left:${left}px;top:${top}px;transform:${photoRotationTransform(photo.rotation)}`;
  }
</script>

{#if tiles.length || faces.error}<section aria-label="Face closeups" class="closeups">
    {#each tiles as tile (tile.id)}<figure>
        <div class="crop">
          {#if source}<img src={source.url} alt="" style={style(tile.box)} />{:else}<span
              >No preview</span
            >{/if}
        </div>
        <figcaption>{tile.label}</figcaption>
      </figure>{/each}
    {#if faces.error}<span role="alert">{faces.error}</span>
      <Button
        size="sm"
        disabled={faces.loading || faces.busy}
        onclick={() => faces.load(faces.version)}>Retry</Button
      >{/if}
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
    max-width: none;
  }
  figcaption,
  span {
    font-size: var(--font-size-xs);
  }
</style>
