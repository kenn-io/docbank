<script lang="ts">
  import DrawerShell from "./DrawerShell.svelte";
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import { Button, Chip, IconButton, Spinner, Table } from "@kenn-io/kit-ui";
  import InspectorDetails, { type InspectorFact } from "./InspectorDetails.svelte";
  import { APIError } from "./api-transport.js";
  import { type StorageStatus } from "./generated/docbank.js";
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

  const deadPackedBytes = $derived(status?.dead_packed_bytes ?? 0);
  const deadPercent = $derived(
    status && status.pack_stored_bytes > 0
      ? Math.round((deadPackedBytes * 100) / status.pack_stored_bytes)
      : 0,
  );
  const stores = $derived(status?.stores ?? []);
  const totalBytes = $derived(status ? status.loose_bytes + status.pack_stored_bytes : 0);
  const fileCount = $derived(
    status ? Math.max(0, ...stores.map((store) => store.authoritative_objects)) : 0,
  );
  const tiles = $derived([
    { label: "Total on disk", value: formatBytes(totalBytes) },
    { label: "Files stored", value: String(fileCount) },
    ...(deadPackedBytes > 0
      ? [
          {
            label: "Reclaimable",
            value: `${formatBytes(deadPackedBytes)} (${deadPercent}% of packs)`,
          },
        ]
      : []),
  ]);
  const internals = $derived<InspectorFact[]>(
    status
      ? [
          {
            label: "Loose files",
            value: `${status.loose_blobs} · ${formatBytes(status.loose_bytes)}`,
          },
          {
            label: "Packed",
            value: `${status.packed_blobs} · ${formatBytes(status.packed_raw_bytes)} raw · ${formatBytes(status.packed_stored_bytes)} stored`,
          },
          {
            label: "Pack files",
            value: `${status.packs} · ${formatBytes(status.pack_stored_bytes)}`,
          },
          { label: "Waiting to compact", value: formatBytes(status.dead_packed_bytes) },
        ]
      : [],
  );

  function storeTone(state: string): "success" | "danger" | "warning" {
    return state === "online" ? "success" : state === "fenced" ? "danger" : "warning";
  }

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
      const next = await generated.storageStatus({ refresh: true }, { session });
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

<DrawerShell
  title="Disk use"
  ariaLabel="Physical storage status"
  closeLabel="Close storage status"
  {onclose}
>
  {#snippet actions()}
    <IconButton
      size="sm"
      ariaLabel="Refresh storage status"
      disabled={loading}
      onclick={() => void refresh()}
    >
      <RefreshCwIcon size="14" aria-hidden="true" />
    </IconButton>
  {/snippet}

  <div class="storage">
    {#if loading && !status}
      <div class="loading"><Spinner size={16} /> Reading disk use…</div>
    {:else if error && !status}
      <div class="load-error">
        <p role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if status}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <div class="tiles" aria-label="Disk use summary" role="group" aria-live="polite">
        {#each tiles as tile (tile.label)}
          <div class="tile">
            <span class="label">{tile.label}</span>
            <span class="value">{tile.value}</span>
          </div>
        {/each}
      </div>

      <section class="locations" aria-label="Storage locations">
        <h3>Locations</h3>
        {#if stores.length === 0}
          <p class="empty">No storage locations</p>
        {:else}
          <Table bare unsorted ariaLabel="Storage locations">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Kind</th>
                <th scope="col" class="num">Size</th>
                <th scope="col" class="num">Objects</th>
                <th scope="col" class="num">Only copy</th>
                <th scope="col">Status</th>
              </tr>
            </thead>
            <tbody>
              {#each stores as store (store.id)}
                <tr>
                  <th scope="row">{store.name}</th>
                  <td>{store.kind}</td>
                  <td class="num">{formatBytes(store.stored_bytes)}</td>
                  <td class="num">{store.authoritative_objects}</td>
                  <td class="num">{store.sole_authority_objects}</td>
                  <td><Chip size="xs" tone={storeTone(store.state)}>{store.state}</Chip></td>
                </tr>
                {#if store.unreadable_objects > 0}
                  <tr class="warning-row">
                    <td colspan="6" class="store-warning">
                      {store.unreadable_objects} object{store.unreadable_objects === 1 ? "" : "s"}
                      currently have no readable alternative.
                    </td>
                  </tr>
                {/if}
              {/each}
            </tbody>
          </Table>
        {/if}
      </section>

      <section class="internals" aria-label="Storage internals">
        <h3>Storage</h3>
        <InspectorDetails label="Storage breakdown" facts={internals} />
      </section>
    {/if}
  </div>
</DrawerShell>

<style>
  .storage {
    display: grid;
    align-content: start;
    gap: var(--space-6);
    padding: var(--space-5);
    font-size: var(--font-size-sm);
  }

  .loading {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
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
  }

  h3 {
    margin: 0;
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-semibold);
  }

  .tiles {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-4);
  }

  .tile {
    display: grid;
    gap: var(--space-1);
    min-width: 9rem;
    padding: var(--space-4) var(--space-5);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .tile .label,
  th {
    color: var(--text-muted);
    font-weight: var(--font-weight-normal);
  }

  .tile .value {
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .locations,
  .internals {
    display: grid;
    gap: var(--space-3);
  }

  .locations :global(table) {
    width: 100%;
    border-collapse: collapse;
  }

  th,
  td {
    padding: var(--space-3) var(--space-4) var(--space-3) 0;
    border-bottom: 1px solid var(--border-muted);
    text-align: left;
    vertical-align: middle;
  }

  tbody th {
    color: var(--text-primary);
    font-weight: var(--font-weight-medium);
  }

  td {
    color: var(--text-secondary);
  }

  .num {
    font-variant-numeric: tabular-nums;
    text-align: right;
  }

  th.num,
  td.num {
    padding-right: var(--space-5);
  }

  .warning-row td {
    padding-top: 0;
  }

  .store-warning {
    color: var(--color-warning-text, var(--text-primary));
  }

  .empty {
    margin: 0;
    color: var(--text-muted);
  }

  .internals :global(.inspector-details) {
    grid-template-columns: 11rem minmax(0, 1fr);
  }

  @media (max-width: 640px) {
    .internals :global(.inspector-details) {
      grid-template-columns: 1fr;
      gap: var(--space-1);
    }
  }
</style>
