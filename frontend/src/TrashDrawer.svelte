<script lang="ts">
  import DrawerShell from "./DrawerShell.svelte";
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import FileIcon from "@lucide/svelte/icons/file";
  import FolderIcon from "@lucide/svelte/icons/folder";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import RotateCcwIcon from "@lucide/svelte/icons/rotate-ccw";
  import { Button, IconButton, Spinner } from "@kenn-io/kit-ui";
  import "./toolForm.css";
  import { APIError } from "./api-transport.js";
  import { undoTrash } from "./trashActions.js";
  import { showToast } from "./toast.svelte.js";
  import { type Node, type TrashPage } from "./generated/docbank.js";
  import { formatBytes, formatDate } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onrestored: (receipt: Node, message: string) => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onrestored, onauthfailure }: Props = $props();

  let page = $state<TrashPage | null>(null);
  let loading = $state(true);
  let error = $state("");
  let restoring = $state(0);
  let generation = 0;

  onMount(() => {
    void refresh();
    return () => {
      generation = -1;
    };
  });

  async function refresh(): Promise<void> {
    if (generation < 0) return;
    const request = ++generation;
    loading = true;
    error = "";
    try {
      const next = await generated.listTrash({ limit: 1000, offset: 0 }, { session });
      if (request !== generation) return;
      page = next;
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        onclose();
        return;
      }
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  async function restore(node: Node): Promise<void> {
    if (restoring) return;
    const requestSession = session;
    restoring = node.id;
    try {
      const { receipt, notice } = await undoTrash(requestSession, node);
      if (generation < 0 || session !== requestSession) return;
      // Restoring one item can bring back companions in other folders, so no
      // listed row is trusted until the refresh lands.
      page = null;
      onrestored(receipt, notice);
      void refresh();
    } catch (cause) {
      if (generation < 0 || session !== requestSession) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        return;
      }
      showToast(cause instanceof Error ? cause.message : String(cause), { tone: "danger" });
    } finally {
      restoring = 0;
    }
  }
</script>

<DrawerShell
  title="Trash"
  ariaLabel="Recoverable trash"
  closeLabel="Close recoverable trash"
  {onclose}
>
  {#snippet actions()}
    <IconButton
      size="sm"
      ariaLabel="Refresh recoverable trash"
      disabled={loading}
      onclick={() => void refresh()}
    >
      <RefreshCwIcon size="14" aria-hidden="true" />
    </IconButton>
  {/snippet}

  <div class="tool-body">
    {#if loading && !page}
      <p class="tool-muted"><Spinner size={14} /> Loading…</p>
    {:else if error && !page}
      <div class="tool-heading">
        <p class="tool-error" role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if page}
      {#if error}<p class="tool-error" role="alert">{error}</p>{/if}
      {#if page.items.length === 0}
        <p class="tool-muted">Trash is empty</p>
      {:else}
        <ul class="tool-list" aria-live="polite">
          {#each page.items as node (node.id)}
            <li>
              <div class="trash-item">
                <span class="trash-icon">
                  {#if node.kind === "dir"}
                    <FolderIcon size="16" aria-hidden="true" />
                  {:else}
                    <FileIcon size="16" aria-hidden="true" />
                  {/if}
                </span>
                <div class="tool-row-main">
                  <strong title={node.name}>{node.name}</strong>
                  <span
                    >Trashed {formatDate(
                      node.trashed_at ?? "",
                    )}{#if node.kind === "file"}{` · ${formatBytes(node.size)}`}{/if}{#if node.photo_asset_id}{" · "}<span
                        >{`Photo · ${node.photo_file_count} trashed file${node.photo_file_count === 1 ? "" : "s"}`}</span
                      >{/if}</span
                  >
                </div>
              </div>
              <Button size="sm" disabled={restoring !== 0} onclick={() => void restore(node)}>
                <RotateCcwIcon size="14" aria-hidden="true" />
                Restore
              </Button>
            </li>
          {/each}
        </ul>
        {#if page.total > page.items.length}
          <p class="tool-muted">
            Newest {page.items.length} of {page.total}. Run <code>docbank trash list</code> for all.
          </p>
        {/if}
      {/if}
    {/if}
  </div>
</DrawerShell>

<style>
  .trash-item {
    display: flex;
    align-items: center;
    gap: var(--space-5);
    min-width: 0;
  }

  .trash-icon {
    display: grid;
    flex-shrink: 0;
    place-items: center;
    width: 28px;
    height: 28px;
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-muted);
  }
</style>
