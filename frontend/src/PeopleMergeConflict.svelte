<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { onDestroy, untrack } from "svelte";
  import { PhotoFaces } from "./photoFaces.svelte.js";
  import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";
  import type { MergeConflict, People } from "./people.svelte.js";
  let { people, conflict }: { people: People; conflict: MergeConflict } = $props();
  // The conflict names its exact version, which may be retained rather than current.
  const faces = untrack(() => {
    const faces = new PhotoFaces(
      people.session,
      people.onauthfailure,
      false,
      () => {},
      people.access,
    );
    void faces.load(conflict.version);
    return faces;
  });
  onDestroy(() => faces.dispose());
  let confirmation = $state<PhotoCullConfirmation>();
  const face = $derived(faces.faces.find((row) => row.face_id === conflict.face_id));
  const assigned = $derived(face && faces.person(face));
  const rejected = $derived(
    (face?.rejected_person_ids ?? []).map((id) => faces.personLabel(id)).join(", "),
  );
  async function clear() {
    if (!face || !assigned) return;
    const captured = { ...face };
    const name = faces.personLabel(assigned.person_id);
    if (
      (await confirmation!.confirm(
        `Clear the name ${name} from ${faces.number(captured)}?`,
        "Clear name",
      )) &&
      (await faces.clear(captured))
    )
      people.conflictCleared(conflict);
  }
</script>

<li>
  {#if faces.loading}<span role="status">Loading face…</span>
  {:else if face}<span
      >{faces.caption(face)}{#if rejected}&nbsp;· marked not {rejected}{/if}</span
    >{#if assigned}<Button size="sm" disabled={faces.busy} onclick={clear}>Clear name</Button
      >{/if}{/if}
  {#if faces.error}<span role="alert">{faces.error}</span>{/if}
</li>
<PhotoCullConfirmation bind:this={confirmation} />
