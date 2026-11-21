<script lang="ts">
  import { onMount } from "svelte";
  import { Button } from "@kenn-io/kit-ui";
  import FileUpIcon from "@lucide/svelte/icons/file-up";
  import FolderOpenIcon from "@lucide/svelte/icons/folder-open";
  import type { PickerKind } from "./localFiles.js";

  let {
    disabled = false,
    initialKind,
    filesLabel = "Choose local files",
    folderLabel = "Choose local folder",
    onchoose,
    onopen,
  }: {
    disabled?: boolean;
    initialKind?: PickerKind;
    filesLabel?: string;
    folderLabel?: string;
    onchoose: (files: File[], kind: PickerKind) => void | Promise<void>;
    onopen?: () => void;
  } = $props();
  let filesInput: HTMLInputElement;
  let folderInput: HTMLInputElement;

  export function choose(kind: PickerKind): void {
    if (disabled) return;
    (kind === "folder" ? folderInput : filesInput).click();
  }

  function selected(event: Event, kind: PickerKind): void {
    const input = event.currentTarget as HTMLInputElement;
    const files = Array.from(input.files ?? []);
    input.value = "";
    if (!disabled && files.length) void onchoose(files, kind);
  }

  onMount(() => {
    if (initialKind) choose(initialKind);
  });
</script>

<input
  bind:this={filesInput}
  type="file"
  multiple
  hidden
  aria-label={filesLabel}
  onchange={(event) => selected(event, "files")}
/>
<input
  bind:this={folderInput}
  type="file"
  multiple
  webkitdirectory
  hidden
  aria-label={folderLabel}
  onchange={(event) => selected(event, "folder")}
/>
<Button
  size="sm"
  surface="soft"
  {disabled}
  onclick={() => {
    onopen?.();
    choose("files");
  }}><FileUpIcon size="14" />Choose files</Button
>
<Button
  size="sm"
  surface="soft"
  {disabled}
  onclick={() => {
    onopen?.();
    choose("folder");
  }}><FolderOpenIcon size="14" />Choose folder</Button
>
