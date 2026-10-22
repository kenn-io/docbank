<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { listPersonFacePreviews } from "./generated/docbank.js";
  import { onDestroy, untrack } from "svelte";
  import {
    faceCrop,
    PhotoPreviewCache,
    type PhotoPreviewCache as PreviewCache,
  } from "./photoPreviewCache.js";
  import { PhotoFaces } from "./photoFaces.svelte.js";
  import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";
  import type { MergeConflict, People } from "./people.svelte.js";

  let {
    people,
    conflict,
    cache,
  }: { people: People; conflict: MergeConflict; cache: PreviewCache } = $props();
  const faces = untrack(
    () =>
      new PhotoFaces(
        people.session,
        people.onauthfailure,
        !!people.access?.deadline,
        () => {},
        people.access,
      ),
  );
  let confirmation = $state<PhotoCullConfirmation>();
  let image = $state<{ url: string; style: string }>();
  let previewLoading = $state(true);
  let previewError = $state("");
  let previewController: AbortController | undefined;
  let imageRelease: (() => void) | undefined;
  let hiddenCache: PreviewCache | undefined;
  const face = $derived(faces.faces.find((row) => row.face_id === conflict.face_id));
  const assertion = $derived(
    face && faces.assertions.find((row) => row.assertion_id === face.assertion_id),
  );
  const assigned = $derived(
    assertion && people.merge?.pair.find((person) => person.person_id === assertion.person_id),
  );
  const pair = $derived(people.merge?.pair.map((person) => person.person_id) ?? []);
  const blocking = $derived((face?.rejected_person_ids ?? []).filter((id) => pair.includes(id)));
  const personName = (id: string) =>
    people.merge?.pair.find((person) => person.person_id === id)?.display_name ?? "selected person";

  onDestroy(() => {
    previewController?.abort();
    imageRelease?.();
    void hiddenCache?.dispose();
    faces.dispose();
  });

  async function loadPreview() {
    const controller = (previewController = new AbortController());
    try {
      await faces.load(conflict.version);
      if (controller.signal.aborted) return;
      const current = faces.faces.find((row) => row.face_id === conflict.face_id);
      const currentAssertion =
        current && faces.assertions.find((row) => row.assertion_id === current.assertion_id);
      const owner =
        currentAssertion &&
        people.merge?.pair.find((person) => person.person_id === currentAssertion.person_id);
      if (!current || !owner || faces.error) {
        previewError = faces.error || "Preview unavailable";
        return;
      }

      let hidden = false;
      let page = await people.run(() =>
        listPersonFacePreviews(
          owner.person_id,
          { hidden, content_version_id: conflict.version, face_id: conflict.face_id },
          { session: people.session, signal: controller.signal },
        ),
      );
      if (!page.items.length && people.access?.deadline) {
        hidden = true;
        page = await people.run(() =>
          listPersonFacePreviews(
            owner.person_id,
            { hidden, content_version_id: conflict.version, face_id: conflict.face_id },
            { session: people.session, signal: controller.signal },
          ),
        );
      }
      if (controller.signal.aborted) return;
      const source = hidden
        ? (hiddenCache ??= new PhotoPreviewCache(people.session, people.onauthfailure, true))
        : cache;
      const preview = page.items[0];
      if (!preview) {
        previewError = "Preview unavailable";
        return;
      }
      const crop = await people.run(() =>
        faceCrop(
          source,
          { ...preview, box: current.box },
          { id: owner.person_id, hidden, size: 96 },
          controller.signal,
          (release) => (imageRelease = release),
        ),
      );
      if (!controller.signal.aborted) {
        image = crop;
        if (!crop) previewError = "Preview unavailable";
      }
    } catch (cause) {
      if (!controller.signal.aborted) previewError = people.failure(cause);
    } finally {
      if (!controller.signal.aborted) previewLoading = false;
    }
  }
  void loadPreview();

  async function withdraw(personID: string) {
    if (!face) return;
    const captured = { ...face };
    const name = personName(personID);
    if (
      (await confirmation!.confirm(
        `Withdraw the rejection of ${name} from ${faces.number(captured)}? The face keeps its current name.`,
        "Withdraw rejection",
      )) &&
      (await faces.withdrawRejection(captured, personID))
    )
      people.conflictCleared(conflict);
  }
</script>

<li>
  {#if faces.loading}<span role="status">Loading face…</span>
  {:else if face}<div class="conflict-evidence">
      <span class="face-crop">
        {#if image}<img
            src={image.url}
            alt={`${faces.number(face)} from ${assigned?.display_name ?? "selected person"}, rejected as ${blocking.map(personName).join(", ")}`}
            style={image.style}
          />
        {:else if previewLoading}<span role="status">Loading photo…</span>
        {:else}<span role="status">{previewError || "Preview unavailable"}</span>{/if}
      </span>
      <div class="conflict-details">
        <span
          >{faces.number(face)} from {assigned?.display_name ?? "selected person"} · rejected as {blocking
            .map(personName)
            .join(", ")}</span
        >
        {#each blocking as id (id)}<Button
            size="sm"
            disabled={faces.busy}
            onclick={() => withdraw(id)}>Withdraw rejection for {personName(id)}</Button
          >{/each}
      </div>
    </div>{/if}
  {#if faces.error}<span role="alert">{faces.error}</span>{/if}
</li>
<PhotoCullConfirmation bind:this={confirmation} />

<style>
  .conflict-evidence {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }
  .face-crop {
    width: 96px;
    height: 96px;
    flex: 0 0 auto;
    position: relative;
    overflow: hidden;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }
  .face-crop img {
    position: absolute;
    top: 50%;
    left: 50%;
    max-width: none;
  }
  .conflict-details {
    display: flex;
    flex: 1 1 auto;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-2);
    min-width: 0;
  }
  .conflict-details :global(button) {
    max-width: 100%;
    text-align: left;
    white-space: normal;
  }
</style>
