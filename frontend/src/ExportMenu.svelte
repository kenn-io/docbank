<script lang="ts">
  import { Menu, MenuTrigger, MenuContent, MenuItem, Tooltip } from "@kenn-io/kit-ui";
  import DownloadIcon from "@lucide/svelte/icons/download";
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  let {
    downloadDisabled = false,
    ondownload,
    onreport,
    onbates,
  }: {
    downloadDisabled?: boolean;
    ondownload: () => void;
    onreport: () => void;
    onbates: () => void;
  } = $props();
</script>

<Menu align="end">
  <MenuTrigger ariaLabel="Export"
    ><DownloadIcon size="14" aria-hidden="true" />Export<ChevronDownIcon
      size="14"
      aria-hidden="true"
    /></MenuTrigger
  >
  <MenuContent ariaLabel="Export">
    <div class="export-options">
      <MenuItem disabled={downloadDisabled} onselect={ondownload}>
        <Tooltip
          class="export-help"
          text="Download documents and their metadata in a ZIP file."
          align="end">Download files…</Tooltip
        >
      </MenuItem>
      <MenuItem onselect={onreport}>
        <Tooltip
          class="export-help"
          text="Count search-term hits across documents and date ranges."
          align="end">Search term report…</Tooltip
        >
      </MenuItem>
      <MenuItem onselect={onbates}>
        <Tooltip
          class="export-help"
          text="Stamp sequential page numbers on PDFs for a legal production."
          align="end">Bates-numbered PDFs…</Tooltip
        >
      </MenuItem>
    </div>
  </MenuContent>
</Menu>

<style>
  .export-options {
    display: grid;
  }
  :global(.export-help) {
    pointer-events: none;
  }
</style>
