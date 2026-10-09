<script lang="ts">
  import type { Snippet } from "svelte";
  import { BottomDock, Button } from "@kenn-io/kit-ui";

  interface Props {
    selectedCount: number;
    visibleDocumentCount: number;
    truncated?: boolean;
    onclear: () => void;
    onselectvisible: () => void;
    ontags?: () => void;
    tagsDisabled?: boolean;
    oncsv?: () => void;
    context?: "live" | "snapshot" | "photos";
    wholeQueryCount?: number;
    onwholequerytags?: () => void;
    onexport?: () => void;
    onreport?: () => void;
    onexportquery?: () => void;
    photoActions?: Snippet;
    allResults?: boolean;
    onallresults?: () => void;
    loadedSelected?: boolean;
  }

  let {
    selectedCount,
    visibleDocumentCount,
    truncated = false,
    onclear,
    onselectvisible,
    ontags,
    tagsDisabled = false,
    oncsv,
    context = "live",
    wholeQueryCount = 0,
    onwholequerytags,
    onexport,
    onreport,
    onexportquery,
    photoActions,
    allResults = false,
    onallresults,
    loadedSelected = selectedCount === visibleDocumentCount,
  }: Props = $props();
</script>

<BottomDock
  open={selectedCount > 0}
  onclose={onclear}
  ariaLabel={context === "photos" ? "Selected photos" : "Selected documents"}
  initialHeight={context === "photos" ? "auto" : "126px"}
  minHeight={context === "photos" ? "min-content" : "112px"}
  maxHeight="var(--selection-dock-max-height)"
  closeTitle={context === "photos" ? "Clear selected photos" : "Clear selected documents"}
  closeAriaLabel={context === "photos" ? "Clear selected photos" : "Clear selected documents"}
  class="selection-dock"
>
  {#snippet header()}
    <div class="selection-summary">
      <strong>{allResults ? `All ${wholeQueryCount.toLocaleString()} photos selected` : `${selectedCount} selected ${context === "photos" ? (selectedCount === 1 ? "photo" : "photos") : `on this ${context === "snapshot" ? "frozen page" : "page"}`}`}</strong>
      {#if context === "snapshot"}
        <span>Visible selection only · whole query has {wholeQueryCount} documents</span>
      {:else if truncated && context !== "photos"}<span>More results exist beyond this page</span>{/if}
    </div>
  {/snippet}

  <div class="selection-actions">
    <Button
      size="sm"
      disabled={loadedSelected && !allResults}
      onclick={onselectvisible}
    >{context === "photos" ? "Select loaded photos" : "Select visible documents"}</Button>
    <Button size="sm" onclick={onclear}>Clear selection</Button>
    {#if context === "photos" && onallresults && !allResults && loadedSelected && wholeQueryCount > visibleDocumentCount}<Button size="sm" tone="info" onclick={onallresults}>Select all {wholeQueryCount.toLocaleString()} photos</Button>{/if}
    {#if photoActions}{@render photoActions()}{/if}
    {#if ontags}
      <Button size="sm" disabled={tagsDisabled} onclick={ontags}>{context === "snapshot" ? "Tag visible selection" : "Edit tags"}</Button>
    {/if}
    {#if context === "snapshot" && onwholequerytags}
      <Button size="sm" tone="info" disabled={tagsDisabled} onclick={onwholequerytags}>Tag whole query</Button>
    {/if}
    {#if oncsv}<Button size="sm" onclick={oncsv}>Export page CSV</Button>{/if}
    {#if onreport}<Button size="sm" onclick={onreport}>Report selected documents</Button>{/if}
    {#if onexport}<Button size="sm" onclick={onexport}>Export selection</Button>{/if}
    {#if context === "snapshot" && onexportquery}<Button size="sm" onclick={onexportquery}>Export frozen query</Button>{/if}
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
