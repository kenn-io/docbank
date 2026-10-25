<script lang="ts">
  import type { Snippet } from "svelte";
  import { Chip } from "@kenn-io/kit-ui";
  import FileTypeIcon, { fileTypeLabel } from "./FileTypeIcon.svelte";
  import { formatBytes } from "./format.js";
  let {
    name,
    kind = "file",
    mime,
    size,
    protected: protectedItem = false,
    onhistory,
    actions,
  }: {
    name: string;
    kind?: string;
    mime?: string;
    size?: number;
    protected?: boolean;
    onhistory?: () => void;
    actions?: Snippet;
  } = $props();
</script>

<header class="inspector-header">
  <div class="file-tile"><FileTypeIcon {kind} {mime} size={22} /></div>
  <div class="heading">
    <div class="title">
      <h2>{name}</h2>
      {#if protectedItem && onhistory}<Chip
          size="xs"
          tone="success"
          uppercase={false}
          interactive
          ariaLabel="Protected, open Audit history"
          onclick={onhistory}>Protected</Chip
        >{/if}
    </div>
    <p>
      {fileTypeLabel(kind, mime)}{#if kind !== "dir" && size !== undefined}{" · "}{formatBytes(
          size,
        )}{/if}
    </p>
  </div>
  {#if actions}<div class="inspector-actions">{@render actions()}</div>{/if}
</header>

<style>
  .inspector-header {
    display: grid;
    grid-template-columns: 40px minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-5);
  }
  .file-tile {
    display: grid;
    place-items: center;
    width: 40px;
    height: 40px;
    border-radius: var(--radius-lg);
    background: var(--bg-inset);
  }
  .file-tile :global(.file-type-icon) {
    display: flex;
    align-items: center;
    justify-content: center;
  }
  .heading {
    min-width: 0;
  }
  .title {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }
  h2 {
    margin: 0;
    min-width: 0;
    overflow-wrap: anywhere;
    font-size: var(--font-size-lg);
    font-weight: var(--font-weight-semibold);
    line-height: 1.3;
  }
  p {
    margin: var(--space-1) 0 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .inspector-actions {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    justify-content: flex-end;
  }
</style>
