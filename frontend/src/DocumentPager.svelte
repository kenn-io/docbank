<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";

  let { total, offset = $bindable(0) }: { total: number; offset: number } = $props();
</script>

{#if total > 50}
  <nav class="pager" aria-label="Document pages">
    <span>{offset + 1}–{Math.min(offset + 50, total)} of {total}</span>
    <Button size="sm" disabled={offset === 0} onclick={() => (offset -= 50)}>Previous</Button>
    <Button size="sm" disabled={offset + 50 >= total} onclick={() => (offset += 50)}>Next</Button>
  </nav>
{/if}

<style>
  .pager {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
    font-size: var(--font-size-sm);
  }
</style>
