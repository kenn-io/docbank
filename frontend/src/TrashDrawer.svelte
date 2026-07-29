<script lang="ts">
  import { onMount } from "svelte";
  import FileIcon from "@lucide/svelte/icons/file";
  import FolderIcon from "@lucide/svelte/icons/folder";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import RotateCcwIcon from "@lucide/svelte/icons/rotate-ccw";
  import Trash2Icon from "@lucide/svelte/icons/trash-2";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Card,
    Chip,
    CopyButton,
    DetailDrawer,
    EmptyState,
    IconButton,
    Spinner,
  } from "@kenn-io/kit-ui";
  import RestoreNodeModal from "./RestoreNodeModal.svelte";
  import { APIError, trashRoots, type Node, type TrashPage } from "./api.js";
  import { formatBytes, formatDate } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onrestored: (receipt: Node) => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onrestored, onauthfailure }: Props = $props();

  let page = $state<TrashPage | null>(null);
  let loading = $state(true);
  let error = $state("");
  let restoreTarget = $state<Node | null>(null);
  let restored = $state<Node | null>(null);
  let generation = 0;

  onMount(() => {
    void refresh();
    return () => {
      generation += 1;
    };
  });

  async function refresh(): Promise<void> {
    const request = ++generation;
    loading = true;
    error = "";
    try {
      const next = await trashRoots(session);
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

  function handleRestored(receipt: Node): void {
    restoreTarget = null;
    restored = receipt;
    if (page) {
      page = {
        ...page,
        items: page.items.filter((item) => item.id !== receipt.id),
        total: Math.max(0, page.total - 1),
      };
    }
    onrestored(receipt);
  }
</script>

<DetailDrawer width="min(720px, 100vw)" ariaLabel="Recoverable trash" {onclose}>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>RECOVERABLE TRASH</span>
        <strong>Trashed documents</strong>
        <small>
          {#if page}
            {page.total} restorable root{page.total === 1 ? "" : "s"}
          {:else}
            Items removed from the live tree
          {/if}
        </small>
      </div>
      <div class="drawer-actions">
        <IconButton
          size="sm"
          ariaLabel="Refresh recoverable trash"
          disabled={loading}
          onclick={() => void refresh()}
        >
          <RefreshCwIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton size="sm" ariaLabel="Close recoverable trash" onclick={onclose}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    </div>
  {/snippet}

  <div class="trash">
    {#if restored}
      <aside class="restored" role="status">
        <RotateCcwIcon size="16" aria-hidden="true" />
        <div>
          <strong>Restored {restored.name}</strong>
          <span>{restored.path}</span>
        </div>
      </aside>
    {/if}
    {#if loading && !page}
      <div class="loading"><Spinner size={16} /> Reading recoverable trash…</div>
    {:else if error && !page}
      <div class="load-error">
        <p role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if page}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      {#if page.items.length === 0}
        <EmptyState
          title="Trash is empty"
          description="Documents moved out of the live tree will remain recoverable here until an explicit trash-empty operation."
        >
          {#snippet icon()}<Trash2Icon size="22" />{/snippet}
        </EmptyState>
      {:else}
        <div class="trash-list" aria-live="polite">
          {#each page.items as node (node.id)}
            <Card level="default" padding="sm">
              <div class="trash-item">
                <div class="trash-icon">
                  {#if node.kind === "dir"}
                    <FolderIcon size="18" aria-hidden="true" />
                  {:else}
                    <FileIcon size="18" aria-hidden="true" />
                  {/if}
                </div>
                <div class="trash-summary">
                  <div>
                    <strong>{node.name}</strong>
                    <Chip size="xs" tone="muted">
                      {node.kind === "dir" ? "Folder" : "Document"}
                    </Chip>
                  </div>
                  <span>Trashed {formatDate(node.trashed_at ?? "")}</span>
                  <dl>
                    <div>
                      <dt>Stable node</dt>
                      <dd>
                        <code>id:{node.id}</code>
                        <CopyButton text={String(node.id)} ariaLabel={`Copy node ID ${node.id}`} />
                      </dd>
                    </div>
                    <div><dt>Revision</dt><dd>{node.revision}</dd></div>
                    {#if node.kind === "file"}
                      <div><dt>Size</dt><dd>{formatBytes(node.size)}</dd></div>
                    {/if}
                  </dl>
                </div>
                <Button
                  size="sm"
                  tone="info"
                  surface="soft"
                  onclick={() => (restoreTarget = node)}
                >
                  <RotateCcwIcon size="14" aria-hidden="true" />
                  Restore
                </Button>
              </div>
            </Card>
          {/each}
        </div>
        {#if page.total > page.items.length}
          <p class="bounded">
            Showing the newest {page.items.length} of {page.total} restorable roots.
            Use the CLI or API for the complete listing.
          </p>
        {/if}
      {/if}
      <aside class="retention-note">
        <strong>Trash is recoverable, not reclaimed space.</strong>
        <p>
          Restore returns the same stable node and retained content. Only an
          explicit <code>docbank trash empty</code> operation removes tree
          metadata; content reclamation remains a separate garbage-collection
          decision.
        </p>
      </aside>
    {/if}
  </div>
</DetailDrawer>

{#if restoreTarget}
  <RestoreNodeModal
    {session}
    node={restoreTarget}
    onclose={() => (restoreTarget = null)}
    onrestored={handleRestored}
    {onauthfailure}
  />
{/if}

<style>
  .drawer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }

  .drawer-heading > div:first-child {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .drawer-heading span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-bold);
    letter-spacing: var(--letter-spacing-label, 0.04em);
  }

  .drawer-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-lg);
  }

  .drawer-heading small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .drawer-actions {
    display: flex;
    gap: var(--space-2);
  }

  .trash {
    display: grid;
    gap: var(--space-4);
    padding: var(--space-5);
  }

  .trash-list {
    display: grid;
    gap: var(--space-3);
  }

  .trash-item {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-4);
  }

  .trash-icon {
    display: grid;
    place-items: center;
    width: 36px;
    height: 36px;
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-blue) 10%, var(--bg-inset));
    color: var(--accent-blue);
  }

  .trash-summary {
    display: grid;
    min-width: 0;
    gap: var(--space-2);
  }

  .trash-summary > div:first-child {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }

  .trash-summary strong {
    overflow: hidden;
    color: var(--text-primary);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .trash-summary > span,
  .bounded {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  dl {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2) var(--space-4);
    margin: 0;
  }

  dl > div {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  dt {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  dd {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .restored,
  .retention-note {
    padding: var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .restored {
    display: flex;
    align-items: start;
    gap: var(--space-3);
    border-color: color-mix(in srgb, var(--accent-blue) 35%, var(--border-default));
    color: var(--accent-blue);
  }

  .restored > div {
    display: grid;
    min-width: 0;
    gap: var(--space-1);
  }

  .restored strong {
    color: var(--text-primary);
  }

  .restored span {
    overflow-wrap: anywhere;
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }

  .retention-note strong {
    color: var(--text-primary);
  }

  .retention-note p,
  .load-error p,
  .error,
  .bounded {
    margin: 0;
  }

  .retention-note p {
    margin-top: var(--space-2);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }

  .loading,
  .load-error {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .load-error {
    justify-content: space-between;
    padding: var(--space-4);
    border: 1px solid color-mix(in srgb, var(--accent-red) 30%, transparent);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-red) 8%, transparent);
  }

  .error {
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }

  @media (max-width: 640px) {
    .trash-item {
      grid-template-columns: auto minmax(0, 1fr);
    }

    .trash-item > :global(button) {
      grid-column: 1 / -1;
      justify-self: stretch;
    }
  }
</style>
