<script lang="ts">
  import type { Component } from "svelte";
  import { Button } from "@kenn-io/kit-ui";
  import { formatBytes } from "./format.js";

  let {
    accept,
    label,
    file,
    disabled = false,
    icon: Icon,
    onchange,
  }: {
    accept: string;
    label: string;
    file: File | null;
    disabled?: boolean;
    icon: Component<{ size?: string | number; "aria-hidden"?: boolean | "true" }>;
    onchange: (event: Event) => void;
  } = $props();
  let input = $state<HTMLInputElement>();
</script>

<div class="chooser">
  <input bind:this={input} type="file" hidden {accept} aria-label={label} {disabled} {onchange} />
  <Button {disabled} onclick={() => input?.click()}
    ><Icon size="14" aria-hidden="true" /> {file ? "Choose another" : label}</Button
  >
  {#if file}<span class="file"
      ><strong title={file.name}>{file.name}</strong><span>{formatBytes(file.size)}</span></span
    >{/if}
</div>

<style>
  .chooser {
    display: flex;
    align-items: center;
    gap: var(--space-5);
    min-width: 0;
  }
  .file {
    display: flex;
    align-items: baseline;
    gap: var(--space-4);
    min-width: 0;
  }
  .file strong {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .file span {
    flex-shrink: 0;
    color: var(--text-muted);
  }
</style>
