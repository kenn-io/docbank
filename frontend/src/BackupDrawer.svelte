<script lang="ts">
  import { onMount } from "svelte";
  import ArchiveIcon from "@lucide/svelte/icons/archive";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
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
  import {
    APIError,
    backupSnapshots,
    type BackupSnapshot,
    type BackupSnapshotList,
  } from "./api.js";
  import { formatBytes, formatDate } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();

  let report = $state<BackupSnapshotList | null>(null);
  let loading = $state(true);
  let error = $state("");
  let unconfigured = $state(false);
  let generation = 0;

  const snapshots = $derived(
    report
      ? [...report.items].sort((left, right) =>
          right.created_at.localeCompare(left.created_at),
        )
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
      const next = await backupSnapshots(session);
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
      if (request === generation) loading = false;
    }
  }

  function snapshotKind(snapshot: BackupSnapshot): "Full" | "Incremental" {
    return snapshot.parent_id ? "Incremental" : "Full";
  }
</script>

<DetailDrawer
  width="min(760px, 100vw)"
  ariaLabel="Backup snapshots"
  {onclose}
>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>RECOVERY POINTS</span>
        <strong>Backup snapshots</strong>
        <small>
          {#if report}
            {report.items.length} immutable snapshot{report.items.length === 1 ? "" : "s"}
          {:else}
            Configured repository history
          {/if}
        </small>
      </div>
      <div class="drawer-actions">
        <IconButton
          size="sm"
          ariaLabel="Refresh backup snapshots"
          disabled={loading}
          onclick={() => void refresh()}
        >
          <RefreshCwIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton size="sm" ariaLabel="Close backup snapshots" onclick={onclose}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    </div>
  {/snippet}

  <div class="backups">
    {#if loading && !report}
      <div class="loading"><Spinner size={16} /> Reading backup history…</div>
    {:else if unconfigured}
      <EmptyState
        title="No backup repository configured"
        description="Set [backup] repo in config.toml, then initialize and create snapshots from the CLI."
      >
        {#snippet icon()}<ArchiveIcon size="22" />{/snippet}
      </EmptyState>
    {:else if error && !report}
      <div class="load-error">
        <p role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if report}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <section class="repository" aria-label="Backup repository identity">
        <div>
          <span>CONFIGURED REPOSITORY</span>
          <strong>{report.repository.path}</strong>
        </div>
        <div class="repository-id">
          <code>{report.repository.id}</code>
          <CopyButton
            text={report.repository.id}
            ariaLabel="Copy backup repository ID"
          />
        </div>
      </section>

      {#if snapshots.length === 0}
        <EmptyState
          title="No snapshots yet"
          description="The repository is initialized, but it does not contain a recovery point."
        >
          {#snippet icon()}<ArchiveIcon size="22" />{/snippet}
        </EmptyState>
      {:else}
        <div class="snapshot-list" aria-live="polite">
          {#each snapshots as snapshot (snapshot.id)}
            <Card
              level="default"
              padding="sm"
              eyebrow={formatDate(snapshot.created_at)}
              title={snapshot.tag || "Untagged snapshot"}
            >
              {#snippet actions()}
                <Chip size="xs" tone={snapshot.parent_id ? "info" : "neutral"}>
                  {snapshotKind(snapshot)}
                </Chip>
              {/snippet}
              <div class="snapshot-id">
                <code>{snapshot.id}</code>
                <CopyButton
                  text={snapshot.id}
                  ariaLabel={`Copy snapshot ID ${snapshot.id}`}
                />
              </div>
              <dl>
                <div><dt>Tree</dt><dd>{snapshot.files} files · {snapshot.nodes} nodes</dd></div>
                <div><dt>Content</dt><dd>{snapshot.blobs} blobs · {formatBytes(snapshot.blob_bytes)}</dd></div>
                <div>
                  <dt>Added</dt>
                  <dd>
                    {formatBytes(snapshot.bytes_added)} · {snapshot.packs_added}
                    {snapshot.packs_added === 1 ? "pack" : "packs"}
                  </dd>
                </div>
              </dl>
            </Card>
          {/each}
        </div>
      {/if}

      <aside class="recovery-note">
        <strong>Snapshot presence is not recovery proof.</strong>
        <p>
          This view lists immutable manifests only. Run
          <code>docbank backup verify</code> to prove repository bytes, and
          restore into a separate vault to rehearse recovery.
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

  .drawer-heading span,
  .repository span {
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

  .backups {
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

  .repository {
    display: grid;
    gap: var(--space-3);
    padding: var(--space-4);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-lg);
    background: var(--bg-inset);
  }

  .repository > div:first-child {
    display: grid;
    gap: var(--space-1);
    min-width: 0;
  }

  .repository strong {
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .repository-id,
  .snapshot-id {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }

  code {
    overflow-wrap: anywhere;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .snapshot-list {
    display: grid;
    gap: var(--space-3);
  }

  .snapshot-id {
    margin-bottom: var(--space-4);
  }

  dl {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--space-3);
    margin: 0;
  }

  dl > div {
    display: grid;
    gap: var(--space-1);
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
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .recovery-note {
    padding: var(--space-4);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-lg);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .recovery-note strong {
    color: var(--text-primary);
  }

  .recovery-note p {
    margin: var(--space-2) 0 0;
  }

  .recovery-note code {
    margin: 0 var(--space-1);
    color: var(--text-primary);
  }

  @media (max-width: 640px) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
