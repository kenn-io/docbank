<script lang="ts">
  import type { Snippet } from "svelte";
  import IconAction from "./IconAction.svelte";
  import XIcon from "@lucide/svelte/icons/x";

  let {
    title,
    actions,
    onclose,
    closeLabel = "Close",
  }: {
    title: string;
    actions?: Snippet;
    onclose: () => void;
    closeLabel?: string;
  } = $props();
</script>

<div class="heading">
  <h2>{title}</h2>
  <div class="actions">
    {@render actions?.()}
    <IconAction size="sm" ariaLabel={closeLabel} tip="Close (Esc)" onclick={onclose}>
      <XIcon size="16" aria-hidden="true" />
    </IconAction>
  </div>
</div>

<style>
  .heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }
  h2 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-shrink: 0;
  }
</style>
