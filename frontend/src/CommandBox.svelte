<script lang="ts">
  import CopyValue from "./CopyValue.svelte";

  let { command, label = "command" }: { command: string; label?: string } = $props();
</script>

<div class="command">
  <CopyValue {label} value={command} />
</div>

<style>
  .command {
    box-sizing: border-box;
    width: fit-content;
    max-width: 100%;
    overflow-x: auto;
    padding: var(--space-2) var(--space-4);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    font-size: var(--font-size-sm);
  }
  /* The command never shrinks into the copy icon, which keeps its own room. */
  .command :global(.copy-value .value) {
    flex: none;
    overflow: visible;
    text-overflow: clip;
  }
  .command :global(.copy-value) {
    gap: var(--space-4);
    padding-right: var(--space-2);
  }
  .command
    :global(.copy-value.kit-button.kit-button--soft.kit-button--neutral:hover:not(:disabled)) {
    background: transparent;
  }
  /* One focus ring, on the box. */
  .command :global(.copy-value:focus-visible) {
    outline: none;
  }
  .command:has(:global(:focus-visible)) {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
  }
</style>
