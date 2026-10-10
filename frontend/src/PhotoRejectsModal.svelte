<script lang="ts">
  import { Button, Modal, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import type { Photos } from "./photos.svelte.js";

  let { photos, onclose, onmove }: { photos: Photos; onclose: () => void; onmove: () => Promise<void> } = $props();
</script>

<Modal title="Move rejects to trash?" tone="danger" ariaLabel="Move rejects to trash" onclose={() => { if (!photos.trashing) onclose(); }} closeOnOverlayClick={!photos.trashing}>
  <SelectDropdown title="Rejects scope" value={photos.rejectsSelected ? "selected" : "workspace"} options={[{ value: "workspace", label: photos.hidden ? "Hidden" : "Library" }, { value: "selected", label: `Selected photos (${photos.selection.selectedIDs.size})` }]} disabled={photos.trashing || photos.rejectsLoading} onchange={value => void photos.previewRejects(value === "selected")} />
  {#if photos.rejectsLoading}<p role="status"><Spinner size={14} /> Counting rejects…</p>{/if}
  {#if photos.rejects}
    <p>{photos.rejects.photos.toLocaleString()} {photos.rejects.photos === 1 ? "photo" : "photos"} · {photos.rejects.files.toLocaleString()} {photos.rejects.files === 1 ? "file" : "files"} including sidecars</p>
    <p>{photos.rejects.unchanged.toLocaleString()} photos stay in Docbank</p>
    {#if photos.rejects.mixed.length}
      <h3>Mixed flags ({photos.rejects.mixed_count.toLocaleString()})</h3>
      <ul>{#each photos.rejects.mixed as pair}<li>{pair.members.map(member => `${member.name}: ${member.flag || "undecided"}`).join(" · ")}</li>{/each}</ul>
      {#if photos.rejects.mixed_count > photos.rejects.mixed.length}<p>And {(photos.rejects.mixed_count - photos.rejects.mixed.length).toLocaleString()} more mixed pairs.</p>{/if}
    {/if}
    {#if photos.rejects.photos > 1000 || photos.rejects.files > 1000}<p role="alert">Select fewer photos. Each move allows up to 1,000 photos and 1,000 files.</p>{/if}
    <p>You can restore these photos from Trash.</p>
  {/if}
  {#if photos.rejectsError}<p role="alert">{photos.rejectsError}</p>{/if}
  {#snippet footer()}
    <Button disabled={photos.trashing} onclick={onclose}>Keep in Docbank</Button>
    {#if !photos.rejects && !photos.rejectsLoading}<Button onclick={() => void photos.previewRejects(photos.rejectsSelected)}>Preview again</Button>{/if}
    <Button tone="danger" disabled={photos.trashing || photos.rejectsLoading || !photos.rejects?.photos || photos.rejects.photos > 1000 || photos.rejects.files > 1000} onclick={() => void onmove()}>{photos.trashing ? "Moving…" : `Move ${photos.rejects?.photos ?? 0} to trash`}</Button>
  {/snippet}
</Modal>

<style>
  ul { max-height: 220px; overflow: auto; padding-left: var(--space-5); }
  li { margin-bottom: var(--space-2); overflow-wrap: anywhere; }
</style>
