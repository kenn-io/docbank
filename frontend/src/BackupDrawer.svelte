<script lang="ts">
  import DrawerShell from "./DrawerShell.svelte";
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import { Button, Chip, CopyButton, IconButton, Spinner, Table } from "@kenn-io/kit-ui";
  import CommandBox from "./CommandBox.svelte";
  import InspectorDetails from "./InspectorDetails.svelte";
  import { APIError } from "./api-transport.js";
  import { type BackupSnapshot, type BackupSnapshotList } from "./generated/docbank.js";
  import { formatBytes, formatDate } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();

  let report = $state<BackupSnapshotList | null>(null);
  let loading = $state(true);
  let loaded = $state(false);
  let error = $state("");
  let unconfigured = $state(false);
  let generation = 0;

  const snapshots = $derived(
    report
      ? [...report.items].sort((left, right) => right.created_at.localeCompare(left.created_at))
      : [],
  );

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
    unconfigured = false;
    try {
      const next = await generated.listBackupSnapshots(undefined, { session });
      if (request !== generation) return;
      report = next;
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        onclose();
        return;
      }
      if (
        cause instanceof APIError &&
        cause.code === "backup_repository" &&
        cause.message.includes("no backup repository configured")
      ) {
        report = null;
        unconfigured = true;
        return;
      }
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) {
        loading = false;
        loaded = true;
      }
    }
  }

  function snapshotKind(snapshot: BackupSnapshot): "Full" | "Incremental" {
    return snapshot.parent_id ? "Incremental" : "Full";
  }
</script>

<DrawerShell
  title="Backups"
  ariaLabel="Backup snapshots"
  closeLabel="Close backup snapshots"
  {onclose}
>
  {#snippet actions()}
    {#if loaded && !unconfigured}
      <IconButton
        size="sm"
        ariaLabel="Refresh backup snapshots"
        disabled={loading}
        onclick={() => void refresh()}
      >
        <RefreshCwIcon size="14" aria-hidden="true" />
      </IconButton>
    {/if}
  {/snippet}

  <div class="backups">
    {#if loading && !report}
      <div class="loading"><Spinner size={16} /> Reading backups…</div>
    {:else if unconfigured}
      <div class="empty">
        <p>No backup repository. Set <code>[backup] repo</code> in config.toml, then run</p>
        <CommandBox label="backup init command" command="docbank backup init" />
      </div>
    {:else if error && !report}
      <div class="load-error">
        <p role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if report?.repository}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <div class="repository">
        <InspectorDetails
          label="Backup repository"
          facts={[
            { label: "Repository", value: report.repository.path },
            { label: "ID", value: report.repository.id, copy: true },
          ]}
        />
      </div>

      {#if snapshots.length === 0}
        <div class="empty">
          <p>No snapshots yet. Run</p>
          <CommandBox label="backup create command" command="docbank backup create" />
        </div>
      {:else}
        <Table bare unsorted ariaLabel="Backup snapshots" class="snapshots">
          <thead
            ><tr
              ><th scope="col">Snapshot</th><th scope="col">Type</th><th scope="col">Content</th><th
                scope="col">Added</th
              ><th scope="col"><span class="kit-sr-only">Copy ID</span></th></tr
            ></thead
          >
          <tbody aria-live="polite">
            {#each snapshots as snapshot (snapshot.id)}
              <tr>
                <th scope="row">
                  <div class="stack">
                    <span class="tag">{snapshot.tag || "Untagged snapshot"}</span>
                    <span class="muted">{formatDate(snapshot.created_at)}</span>
                  </div>
                </th>
                <td>
                  <Chip size="xs" tone={snapshot.parent_id ? "info" : "neutral"}>
                    {snapshotKind(snapshot)}
                  </Chip>
                </td>
                <td class="num">
                  {snapshot.files}
                  {snapshot.files === 1 ? "file" : "files"} · {formatBytes(snapshot.blob_bytes)}
                </td>
                <td class="num">{formatBytes(snapshot.bytes_added)}</td>
                <td class="copy">
                  <CopyButton text={snapshot.id} ariaLabel={`Copy snapshot ID ${snapshot.id}`} />
                </td>
              </tr>
            {/each}
          </tbody>
        </Table>
      {/if}
    {/if}
  </div>
</DrawerShell>

<style>
  .backups {
    display: grid;
    align-content: start;
    gap: var(--space-5);
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

  .empty {
    display: grid;
    justify-items: start;
    gap: var(--space-3);
  }

  .empty p {
    margin: 0;
    color: var(--text-secondary);
  }

  code {
    font-size: var(--font-size-sm);
  }

  .repository :global(.inspector-details > div) {
    grid-template-columns: 7rem minmax(0, 1fr);
  }

  .backups :global(.snapshots) {
    width: 100%;
    border-collapse: collapse;
  }

  th,
  td {
    padding: var(--space-3) var(--space-3) var(--space-3) 0;
    border-bottom: 1px solid var(--border-muted);
    text-align: left;
    font-weight: var(--font-weight-normal);
    vertical-align: middle;
  }

  thead th {
    padding-block: var(--space-2);
    color: var(--text-muted);
  }

  .stack {
    display: grid;
    gap: var(--space-1);
  }

  .tag {
    color: var(--text-primary);
    font-weight: var(--font-weight-medium);
  }

  .muted {
    color: var(--text-muted);
  }

  .num {
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .copy {
    width: 1%;
    padding-right: 0;
    text-align: right;
  }
</style>
