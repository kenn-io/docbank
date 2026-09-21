<script lang="ts">
  import AttachmentRelations from "./AttachmentRelations.svelte";
  import type { AttachmentIdentity } from "./attachments.js";
  import type { SelectedSource } from "./selectedSource.js";
  let { session, source, onopen, onauthfailure }: {
    session: string; source: SelectedSource; onopen: (target: AttachmentIdentity) => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();
</script>

<div class="attachments-tab">
  <p>Explicit published occurrences only. Inventory completeness and current processing are separate; opening a document checks its current access and exact retained version.</p>
  <AttachmentRelations {session} {source} direction="outgoing" {onopen} {onauthfailure} />
  <AttachmentRelations {session} {source} direction="incoming" {onopen} {onauthfailure} />
</div>

<style>
  .attachments-tab { display: grid; gap: var(--space-3); }
  p { margin: 0; color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
