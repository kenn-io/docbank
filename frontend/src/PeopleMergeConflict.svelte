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
  const pair = $derived(people.merge?.pair.map((person) => person.person_id) ?? []);
  // Only a rejection of one of the two people blocks this merge.
  const blocking = $derived((face?.rejected_person_ids ?? []).filter((id) => pair.includes(id)));
  async function withdraw(personID: string) {
    if (!face) return;
    const captured = { ...face };
    const name = faces.personLabel(personID);
    if (
      (await confirmation!.confirm(
        `Withdraw "not ${name}" from ${faces.number(captured)}? The face keeps its current name.`,
        "Withdraw rejection",
      )) &&
      (await faces.withdrawRejection(captured, personID))
    )
      people.conflictCleared(conflict);
  }
</script>

<li>
  {#if faces.loading}<span role="status">Loading face…</span>
  {:else if face}<span>{faces.caption(face)}</span>{#each blocking as id (id)}<Button
        size="sm"
        disabled={faces.busy}
        onclick={() => withdraw(id)}>Withdraw "not {faces.personLabel(id)}"</Button
      >{/each}{/if}
  {#if faces.error}<span role="alert">{faces.error}</span>{/if}
</li>
<PhotoCullConfirmation bind:this={confirmation} />
