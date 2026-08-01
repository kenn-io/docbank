<script lang="ts">
  import { onMount } from "svelte";
  import ArchiveIcon from "@lucide/svelte/icons/archive";
  import DatabaseIcon from "@lucide/svelte/icons/database";
  import HardDriveIcon from "@lucide/svelte/icons/hard-drive";
  import ServerIcon from "@lucide/svelte/icons/server";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Card,
    Chip,
    DetailDrawer,
    IconButton,
    Spinner,
  } from "@kenn-io/kit-ui";
  import {
    APIError,
    storageStatus,
    type StorageStatus,
  } from "./api.js";
  import { formatBytes } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();

  let status = $state<StorageStatus | null>(null);
  let loading = $state(true);
  let error = $state("");
  let generation = 0;

  const storedPayload = $derived(
    status ? status.loose_bytes + status.pack_stored_bytes : 0,
  );
  const deadPackedBytes = $derived(status?.dead_packed_bytes ?? 0);
  const deadPercent = $derived(
    status && status.pack_stored_bytes > 0
      ? Math.round((deadPackedBytes * 100) / status.pack_stored_bytes)
      : 0,
  );
  const stores = $derived(status?.stores ?? []);

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
      const next = await storageStatus(session, true);
      if (request !== generation) return;
      status = next;
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
</script>

<DetailDrawer
  width="min(760px, 100vw)"
  ariaLabel="Physical storage status"
  {onclose}
>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>PHYSICAL STORAGE</span>
        <strong>Loose and packed content</strong>
        <small>
          {#if status}
            {status.loose_blobs} loose file{status.loose_blobs === 1 ? "" : "s"}
            · {status.packed_blobs} live packed object{status.packed_blobs === 1 ? "" : "s"}
            · {formatBytes(storedPayload)} stored payload
          {:else}
            Current daemon inventory
          {/if}
        </small>
      </div>
      <div class="drawer-actions">
        <IconButton
          size="sm"
          ariaLabel="Refresh storage status"
          disabled={loading}
          onclick={() => void refresh()}
        >
          <RefreshCwIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton size="sm" ariaLabel="Close storage status" onclick={onclose}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    </div>
  {/snippet}

  <div class="storage">
    {#if loading && !status}
      <div class="loading"><Spinner size={16} /> Reading storage inventory…</div>
    {:else if error && !status}
      <div class="load-error">
        <p role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if status}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <div class="storage-grid" aria-live="polite">
        <Card
          level="default"
          padding="sm"
          eyebrow="LOOSE CONTENT"
          title={formatBytes(status.loose_bytes)}
        >
          {#snippet actions()}<HardDriveIcon size="18" aria-hidden="true" />{/snippet}
          <p>
            {status.loose_blobs} physical file{status.loose_blobs === 1 ? "" : "s"}
            stored as raw or zstd. This inventory can include untracked or
            redundant loose files.
          </p>
        </Card>

        <Card
          level="default"
          padding="sm"
          eyebrow="LIVE PACKED CONTENT"
          title={formatBytes(status.packed_stored_bytes)}
        >
          {#snippet actions()}<ArchiveIcon size="18" aria-hidden="true" />{/snippet}
          <dl>
            <div><dt>Objects</dt><dd>{status.packed_blobs}</dd></div>
            <div><dt>Raw bytes</dt><dd>{formatBytes(status.packed_raw_bytes)}</dd></div>
          </dl>
        </Card>

        <Card
          level="default"
          padding="sm"
          eyebrow="PACK FILES"
          title={formatBytes(status.pack_stored_bytes)}
        >
          {#snippet actions()}<DatabaseIcon size="18" aria-hidden="true" />{/snippet}
          <p>
            {status.packs} immutable pack{status.packs === 1 ? "" : "s"} contain{status.packs === 1 ? "s" : ""}
            live and logically dead stored payload.
          </p>
        </Card>

        <Card
          level="default"
          padding="sm"
          eyebrow="PENDING REPACK"
          title={formatBytes(status.dead_packed_bytes)}
        >
          {#snippet actions()}
            <Chip
              size="xs"
              tone={deadPackedBytes > 0 ? "warning" : "success"}
            >
              {deadPackedBytes > 0 ? `${deadPercent}% of packs` : "Compact"}
            </Chip>
          {/snippet}
          <p>
            {#if status.dead_packed_bytes > 0}
              Logically dead payload still occupies immutable pack files.
            {:else}
              No stored pack payload is waiting to be reclaimed.
            {/if}
          </p>
        </Card>
      </div>

      <section class="stores" aria-label="Content stores">
        <div class="section-heading">
          <div>
            <span>CONTENT STORES</span>
            <strong>{stores.length} physical location{stores.length === 1 ? "" : "s"}</strong>
          </div>
          <ServerIcon size="18" aria-hidden="true" />
        </div>
        {#if stores.length === 0}
          <p class="empty">No store authority was reported.</p>
        {:else}
          <div class="store-list">
            {#each stores as store (store.id)}
              <article class="store">
                <div class="store-title">
                  <div>
                    <strong>{store.name}</strong>
                    <span>{store.role} · {store.kind}</span>
                  </div>
                  <Chip
                    size="xs"
                    tone={store.state === "online" ? "success" : store.state === "fenced" ? "danger" : "warning"}
                  >
                    {store.state}
                  </Chip>
                </div>
                <dl>
                  <div>
                    <dt>Authority</dt>
                    <dd>{store.authoritative_objects} objects · {formatBytes(store.logical_bytes)}</dd>
                  </div>
                  <div>
                    <dt>Stored</dt>
                    <dd>{formatBytes(store.stored_bytes)} · {store.pack_count} packs</dd>
                  </div>
                  <div>
                    <dt>Sole copies</dt>
                    <dd>{store.sole_authority_objects}</dd>
                  </div>
                  <div>
                    <dt>Live documents</dt>
                    <dd>{store.affected_documents}</dd>
                  </div>
                </dl>
                {#if store.unreadable_objects > 0}
                  <p class="store-warning">
                    {store.unreadable_objects} object{store.unreadable_objects === 1 ? "" : "s"}
                    currently have no readable alternative.
                  </p>
                {/if}
              </article>
            {/each}
          </div>
        {/if}
      </section>

      <aside class="maintenance-note">
        <strong>This view never changes storage.</strong>
        <p>
          Dead packed bytes are not reclaimed space. Run
          <code>docbank storage repack</code> explicitly when you want Docbank
          to rewrite eligible sparse packs and retire their old files.
        </p>
      </aside>
    {/if}
  </div>
</DetailDrawer>

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
    flex-shrink: 0;
  }

  .storage {
    display: grid;
    gap: var(--space-5);
    padding: var(--space-5);
  }

  .loading {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .load-error {
    display: grid;
    justify-items: start;
    gap: var(--space-4);
  }

  .load-error p,
  .error {
    margin: 0;
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }

  .storage-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-3);
  }

  .storage-grid p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.45;
  }

  dl {
    display: grid;
    gap: var(--space-2);
    margin: 0;
  }

  dl > div {
    display: flex;
    justify-content: space-between;
    gap: var(--space-3);
  }

  dt {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  dd {
    margin: 0;
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
  }

  .maintenance-note {
    padding: var(--space-4);
    border: 1px solid color-mix(in srgb, var(--accent-amber) 35%, var(--border-default));
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-amber) 7%, var(--bg-surface));
  }

  .stores {
    display: grid;
    gap: var(--space-3);
    margin-top: var(--space-5);
  }

  .section-heading,
  .store-title {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .section-heading > div,
  .store-title > div {
    display: grid;
    min-width: 0;
  }

  .section-heading span,
  .store-title span,
  .store dt {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .store-list {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-3);
  }

  .store {
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    padding: var(--space-4);
  }

  .store dl {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-3);
    margin: var(--space-4) 0 0;
  }

  .store dl div {
    display: grid;
    gap: var(--space-1);
  }

  .store dd {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .store-warning {
    margin: var(--space-3) 0 0;
    color: var(--color-warning-text, var(--text-primary));
    font-size: var(--font-size-sm);
  }

  .empty {
    color: var(--text-muted);
  }

  @media (max-width: 640px) {
    .store-list,
    .store dl {
      grid-template-columns: 1fr;
    }
  }

  .maintenance-note strong {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .maintenance-note p {
    margin: var(--space-2) 0 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }

  .maintenance-note code {
    color: var(--text-primary);
    font-family: var(--font-mono);
  }

  @media (max-width: 640px) {
    .storage-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
