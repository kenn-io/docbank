<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import type { PhotoFaces } from "./photoFaces.svelte.js";
  import type { PhotoBrowseRow, FaceBox } from "./generated/docbank.js";
  import { photoFaceCropStyle } from "./photoRotation.js";
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
      label: faces.caption(face),
      box: face.box,
    })),
  ]);
  function report(id: string) {
    return photo.quality?.face_reports?.find((r) => r.face_id === id);
  }
  function measurements(r: NonNullable<ReturnType<typeof report>>) {
    return [
      ["Sharpness", r.sharpness],
      ["Light", r.light],
      ["Facing", r.facing],
      ["Prominence", r.prominence],
      ["Eyes", r.eyes_open === null ? null : r.eyes_open ? 1 : 0],
      ["Smile", r.smile === null ? null : r.smile ? 1 : 0],
    ] as const;
  }
  function style(box?: FaceBox) {
    if (!source) return "";
    return photoFaceCropStyle(box, source.width, source.height, photo.rotation, 128);
  }
</script>

{#if tiles.length || faces.error}<section aria-label="Face closeups" class="closeups">
    {#each tiles as tile (tile.id)}
      {@const r = report(tile.id)}
      <figure>
        <div class="crop">
          {#if source}<img src={source.url} alt="" style={style(tile.box)} />{:else}<span
              >No preview</span
            >{/if}
        </div>
        <figcaption>
          {tile.label}
          {#if r}
            {#if r.grade}<span class="grade" data-grade={r.grade}>{r.grade}</span>{/if}
            <dl>
              {#each measurements(r) as [label, value]}<div>
                  <dt>{label}</dt>
                  <dd title={value === null ? "Unavailable" : undefined}>
                    {value === null
                      ? "—"
                      : label === "Eyes"
                        ? value
                          ? "Open"
                          : "Closed"
                        : label === "Smile"
                          ? value
                            ? "Yes"
                            : "No"
                          : `${Math.round(value * 100)}%`}
                  </dd>
                </div>{/each}
            </dl>
          {/if}
        </figcaption>
      </figure>{/each}
    {#if faces.error}<span role="alert">{faces.error}</span>
      <Button
        size="sm"
        disabled={faces.loading || faces.busy}
        onclick={() => faces.load(faces.version)}>Retry</Button
      >{/if}
  </section>{/if}

<style>
  dl {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    margin: var(--space-3) 0 0;
  }
  dl div {
    display: flex;
    gap: var(--space-2);
  }
  dd {
    margin: 0;
  }
  .grade {
    margin-left: var(--space-2);
  }
  .grade[data-grade="green"] {
    color: var(--accent-green);
  }
  .grade[data-grade="yellow"] {
    color: var(--accent-yellow);
  }
  .grade[data-grade="red"] {
    color: var(--accent-red);
  }
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
