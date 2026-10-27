<script lang="ts">
  import type { Snippet } from "svelte";
  import { BottomDock, Button } from "@kenn-io/kit-ui";

  interface Props {
    selectedCount: number;
    retainOpen?: boolean;
    outsideCount?: number;
    selectVisible?: boolean;
    visibleDocumentCount: number;
    truncated?: boolean;
    onclear: () => void;
    onselectvisible: () => void;
    ontags?: () => void;
    tagsDisabled?: boolean;
    ontrash?: () => void;
    trashDisabled?: boolean;
    oncsv?: () => void;
    context?: "live" | "snapshot" | "photos";
    wholeQueryCount?: number;
    onexport?: () => void;
    exportDisabled?: boolean;
    exportHelp?: string;
    onreport?: () => void;
    photoActions?: Snippet;
    allResults?: boolean;
    onallresults?: () => void;
    loadedSelected?: boolean;
  }

  let {
    selectedCount,
    retainOpen = false,
    outsideCount = 0,
    selectVisible = false,
    visibleDocumentCount,
    truncated = false,
    onclear,
    onselectvisible,
    ontags,
    tagsDisabled = false,
    ontrash,
    trashDisabled = false,
    oncsv,
    context = "live",
    wholeQueryCount = 0,
    onexport,
    exportDisabled = false,
    exportHelp = "",
    onreport,
    photoActions,
    allResults = false,
    onallresults,
    loadedSelected,
  }: Props = $props();
  const visibleSelected = $derived(loadedSelected ?? selectedCount === visibleDocumentCount);
</script>

<BottomDock
  open={selectedCount > 0 || retainOpen}
  onclose={onclear}
  ariaLabel={context === "photos" ? "Selected photos" : "Selected documents"}
  initialHeight="auto"
  minHeight="min-content"
  maxHeight="var(--selection-dock-max-height)"
  closeTitle={context === "photos" ? "Clear selected photos" : "Clear selected documents"}
  closeAriaLabel={context === "photos" ? "Clear selected photos" : "Clear selected documents"}
  class="selection-dock"
>
  {#snippet header()}
    <div class="selection-summary">
      <strong
        >{allResults && selectedCount > 0
          ? `All ${wholeQueryCount.toLocaleString()} photos selected`
          : `${selectedCount} selected ${context === "photos" ? (selectedCount === 1 ? "photo" : "photos") : context === "snapshot" ? "" : "on this page"}`}</strong
      >
      {#if outsideCount}<span>{outsideCount} outside this view</span>{/if}
      {#if truncated && context !== "photos"}<span>More results exist beyond this page</span>{/if}
    </div>
  {/snippet}

  <div class="selection-actions">
    <Button
      size="sm"
      disabled={(context === "photos" && retainOpen && selectedCount === 0) ||
        (visibleSelected && selectedCount === visibleDocumentCount && !allResults)}
      onclick={onselectvisible}
      >{context === "photos"
        ? selectVisible
          ? "Select visible photos"
          : "Select loaded photos"
        : "Select visible documents"}</Button
    >
    <Button size="sm" disabled={context === "photos" && selectedCount === 0} onclick={onclear}
      >Clear selection</Button
    >
    {#if context === "photos" && onallresults && !allResults && visibleSelected && wholeQueryCount > 0}<Button
        size="sm"
        tone="info"
        onclick={onallresults}>Select all {wholeQueryCount.toLocaleString()} photos</Button
      >{/if}
    {#if photoActions}{@render photoActions()}{/if}
    {#if ontags}
      <Button size="sm" disabled={tagsDisabled || selectedCount === 0} onclick={ontags}
        >Tag selected…</Button
      >
    {/if}
    {#if ontrash}<Button size="sm" tone="danger" disabled={trashDisabled} onclick={ontrash}
        >Move to trash</Button
      >{/if}
    {#if oncsv}<Button size="sm" onclick={oncsv}>Export page CSV</Button>{/if}
    {#if onreport && selectedCount > 0}<Button size="sm" onclick={onreport}
        >Report selected documents</Button
      >{/if}
    {#if onexport}<Button
        size="sm"
        disabled={exportDisabled || selectedCount === 0}
        onclick={onexport}>Export selection</Button
      >{/if}
    {#if exportHelp}<span>{exportHelp}</span>{/if}
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
