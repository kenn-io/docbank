<script module lang="ts">
  export type AddChoice = "files" | "folder" | "mailbox" | "loadFile";
</script>

<script lang="ts">
  import PlusIcon from "@lucide/svelte/icons/plus";
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  import { Menu, MenuTrigger, MenuContent, MenuItem, MenuSeparator } from "@kenn-io/kit-ui";

  let {
    disabledReason = "",
    onselect,
  }: { disabledReason?: string; onselect: (choice: AddChoice) => void } = $props();

  const choices = [
    { id: "files", label: "Upload files" },
    { id: "folder", label: "Upload folder" },
    { id: "mailbox", label: "Import mailbox" },
    { id: "loadFile", label: "Import load files" },
  ] as const;
</script>

<Menu align="end">
  <MenuTrigger class="document-add-trigger"
    ><PlusIcon size="14" />Add<ChevronDownIcon size="12" /></MenuTrigger
  >
  <MenuContent ariaLabel="Add documents">
    {#each choices as choice, index}
      {#if index === 2}<MenuSeparator />{/if}
      <MenuItem disabled={!!disabledReason} onselect={() => onselect(choice.id)}
        ><span title={disabledReason || undefined}>{choice.label}</span></MenuItem
      >
    {/each}
  </MenuContent>
</Menu>

<style>
  :global(.kit-menu .kit-menu__trigger.document-add-trigger) {
    min-height: 26px;
    border-color: var(--accent-blue);
    background: var(--accent-blue);
    color: var(--bg-surface);
    font-weight: var(--font-weight-medium);
  }

  :global(.kit-menu .kit-menu__trigger.document-add-trigger:hover),
  :global(.kit-menu .kit-menu__trigger.document-add-trigger[aria-expanded="true"]) {
    background: color-mix(in srgb, var(--accent-blue) 85%, black);
    color: var(--bg-surface);
  }
</style>
