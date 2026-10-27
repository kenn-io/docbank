<script lang="ts">
  import { Spinner } from "@kenn-io/kit-ui";
  import AttachmentRelations from "./AttachmentRelations.svelte";
  import type { AttachmentIdentity } from "./attachments.js";
  import type { SelectedSource } from "./selectedSource.js";
  let {
    session,
    source,
    onopen,
    onauthfailure,
  }: {
    session: string;
    source: SelectedSource;
    onopen: (target: AttachmentIdentity) => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();
  let outgoingTotal = $state<number>();
  let incomingTotal = $state<number>();
  let outgoingBusy = $state(false);
  let incomingBusy = $state(false);
</script>

<div class="attachments-tab">
  {#if outgoingBusy || incomingBusy}<p role="status"><Spinner size={14} /> Loading…</p>{/if}
  <AttachmentRelations
    {session}
    {source}
    direction="outgoing"
    bind:total={outgoingTotal}
    bind:busy={outgoingBusy}
    {onopen}
    {onauthfailure}
  />
  <AttachmentRelations
    {session}
    {source}
    direction="incoming"
    bind:total={incomingTotal}
    bind:busy={incomingBusy}
    {onopen}
    {onauthfailure}
  />
  {#if outgoingTotal === 0 && incomingTotal === 0}<p>No published attachments</p>{/if}
</div>

<style>
  .attachments-tab {
    display: grid;
    gap: var(--space-3);
  }
  p {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
