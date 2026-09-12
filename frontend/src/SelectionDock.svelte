<script lang="ts">
  import { BottomDock, Button } from "@kenn-io/kit-ui";

  interface Props {
    selectedCount: number;
    visibleDocumentCount: number;
    truncated: boolean;
    onclear: () => void;
    onselectvisible: () => void;
    ontags?: () => void;
    tagsDisabled?: boolean;
  }

  let {
    selectedCount,
    visibleDocumentCount,
    truncated,
    onclear,
    onselectvisible,
    ontags,
    tagsDisabled = false,
  }: Props = $props();
</script>

<BottomDock
  open={selectedCount > 0}
  onclose={onclear}
  ariaLabel="Selected documents"
  initialHeight="126px"
  minHeight="112px"
  maxHeight="var(--selection-dock-max-height)"
  closeTitle="Clear selected documents"
  closeAriaLabel="Clear selected documents"
  class="selection-dock"
>
  {#snippet header()}
    <div class="selection-summary">
      <strong>{selectedCount} selected on this page</strong>
      {#if truncated}<span>More results exist beyond this page</span>{/if}
    </div>
  {/snippet}

  <div class="selection-actions">
    <Button
      size="sm"
      disabled={selectedCount === visibleDocumentCount}
      onclick={onselectvisible}
    >Select visible documents</Button>
    <Button size="sm" onclick={onclear}>Clear selection</Button>
    {#if ontags}
      <Button size="sm" disabled={tagsDisabled} onclick={ontags}>Edit tags</Button>
    {/if}
  </div>
</BottomDock>

<style>
  :global(.selection-dock) {
    position: sticky;
    bottom: 0;
    z-index: 20;
  }

  :global(html:has(.selection-dock)) {
    --selection-dock-max-height: 220px;
    scroll-padding-bottom: var(--selection-dock-max-height);
  }

  .selection-summary {
    display: flex;
    align-items: baseline;
    gap: var(--space-3);
    min-width: 0;
  }

  .selection-summary strong {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .selection-summary span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .selection-actions {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-3);
    padding: var(--space-4) var(--space-5);
  }

  @media (max-width: 640px) {
    .selection-summary {
      align-items: flex-start;
      flex-direction: column;
      gap: var(--space-1);
    }
  }
</style>
