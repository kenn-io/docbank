<script lang="ts">
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import ActivityIcon from "@lucide/svelte/icons/activity";
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
    StatusDot,
    type ChipTone,
    type StatusDotStatus,
  } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { type Job } from "./generated/docbank.js";
  import { formatDate } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();

  // Refresh cadence while a durable operation is still moving.
  const POLL_MS = 2000;

  let items = $state<Job[]>([]);
  let loading = $state(true);
  let error = $state("");
  let cancelling = $state(new Set<string>());
  let now = $state(Date.now());
  let generation = 0;
  let poll: ReturnType<typeof setTimeout> | undefined;

  // Durable operations carry an ID and progress; supervised workers run for the daemon's lifetime.
  const operations = $derived(
    items
      .filter((job) => job.operation_id)
      .sort((a, b) => Number(isActive(b)) - Number(isActive(a)) || b.started_at.localeCompare(a.started_at)),
  );
  const workers = $derived(
    items
      .filter((job) => !job.operation_id)
      .sort((a, b) => Number(b.status === "failed") - Number(a.status === "failed") || a.name.localeCompare(b.name)),
  );
  const activeOperations = $derived(operations.filter(isActive).length);
  const failedWorkers = $derived(workers.filter((job) => job.status === "failed").length);
  const summary = $derived(
    [
      activeOperations === 0 ? "No operations running" : `${activeOperations} ${activeOperations === 1 ? "operation" : "operations"} running`,
      `${workers.length} ${workers.length === 1 ? "worker" : "workers"}`,
      failedWorkers > 0 ? `${failedWorkers} failed` : "",
    ].filter(Boolean).join(" · "),
  );

  onMount(() => {
    void refresh();
    return () => {
      generation += 1;
      clearTimeout(poll);
    };
  });

  async function refresh(quiet = false): Promise<void> {
    const request = ++generation;
    clearTimeout(poll);
    if (!quiet) loading = true;
    error = "";
    try {
      const next = await generated.listJobs({ session }).then((result) => result.items);
      if (request !== generation) return;
      items = next;
      now = Date.now();
      if (next.some((job) => job.operation_id && isActive(job))) {
        poll = setTimeout(() => void refresh(true), POLL_MS);
      }
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

  async function cancelPhotoImport(job: Job): Promise<void> {
    if (job.kind !== "photo-import" || !job.operation_id || !job.can_cancel) return;
    const runId = job.operation_id;
    cancelling = new Set(cancelling).add(runId);
    try {
      for (let attempt = 0; attempt < 3; attempt += 1) {
        const run = await generated.getPhotoImport(runId, { session });
        try {
          await generated.cancelPhotoImport(runId, { "If-Match": `"${run.revision}"` }, { session });
          break;
        } catch (cause) {
          if (!(cause instanceof APIError && cause.status === 412 && attempt < 2)) throw cause;
        }
      }
      await refresh();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      const next = new Set(cancelling);
      next.delete(runId);
      cancelling = next;
    }
  }

  function isActive(job: Job): boolean {
    return job.status === "running" || job.status === "queued";
  }

  const operationTitles: Record<string, string> = {
    "photo-import": "Photo import",
    place: "Storage placement",
    evacuate: "Store evacuation",
    repair: "Storage repair",
    salvage: "Storage salvage",
  };

  const workerTitles: Record<string, string> = {
    "daemon.idle-timeout": "Idle shutdown timer",
    "derive:document-events": "Document events",
    "derive:document-people": "Document people",
    "exports:write": "Export writer",
    "extract:email": "Email extraction",
    "extract:plain-text": "Plain-text extraction",
    "extract:source-metadata": "Source metadata extraction",
    "import:packages": "Package imports",
    "maintenance:auxiliary-checksums": "Auxiliary checksums",
    "maintenance:package-preflights": "Package preflight cleanup",
    "pages:render": "Page rendering",
    "probe:media-origins": "Media origin probe",
    "process:embeddings": "Embeddings",
    "process:media-continuations": "Media processing",
    "process:vector-indexes": "Vector indexes",
    "storage:pack": "Storage packing",
  };

  function sentence(value: string): string {
    const words = value.replace(/[-_.:]+/g, " ").trim();
    return words.charAt(0).toUpperCase() + words.slice(1);
  }

  function jobTitle(job: Job): string {
    if (job.operation_id) return operationTitles[job.kind ?? ""] ?? sentence(job.kind || job.name.split(":")[0] || job.name);
    if (workerTitles[job.name]) return workerTitles[job.name];
    if (job.name.startsWith("watch:")) return "Inbox watcher";
    return sentence(job.name);
  }

  function statusLabel(job: Job): string {
    if (job.cancel_requested && job.status === "running") return "Stopping";
    return sentence(job.status);
  }

  function statusTone(job: Job): ChipTone {
    if (job.cancel_requested && job.status === "running") return "warning";
    switch (job.status) {
      case "queued":
        return "neutral";
      case "running":
        return "info";
      case "completed":
        return "success";
      case "failed":
        return "danger";
      case "cancelled":
        return "canceled";
    }
  }

  function workerDot(status: Job["status"]): StatusDotStatus {
    switch (status) {
      case "running":
        return "working";
      case "queued":
        return "stale";
      case "failed":
        return "unclean";
      default:
        return "idle";
    }
  }

  function unit(job: Job): string {
    return job.kind === "photo-import" ? "groups" : "objects";
  }

  function percentLabel(job: Job): string {
    const value = percent(job);
    return value === 0 && (job.completed_objects ?? 0) > 0 ? "<1%" : `${value}%`;
  }

  function percent(job: Job): number {
    const total = job.total_objects ?? 0;
    if (total <= 0) return 0;
    return Math.min(100, Math.floor(((job.completed_objects ?? 0) / total) * 100));
  }

  const relativeFormat = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

  function relative(value: string): string {
    const parsed = Date.parse(value);
    if (Number.isNaN(parsed)) return value || "—";
    const seconds = Math.round((parsed - now) / 1000);
    const magnitude = Math.abs(seconds);
    if (magnitude < 45) return "just now";
    if (magnitude < 3600) return relativeFormat.format(Math.round(seconds / 60), "minute");
    if (magnitude < 86400) return relativeFormat.format(Math.round(seconds / 3600), "hour");
    return formatDate(value);
  }
</script>

<DetailDrawer
  width="min(620px, 100vw)"
  ariaLabel="Daemon background jobs"
  {onclose}
>
  {#snippet header()}
    <div class="drawer-heading">
      <div class="heading-copy">
        <span>Daemon activity</span>
        <strong>Background jobs</strong>
        <small>{summary}</small>
      </div>
      <div class="drawer-actions">
        <IconButton
          size="sm"
          ariaLabel="Refresh background jobs"
          disabled={loading}
          onclick={() => void refresh()}
        >
          <RefreshCwIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton size="sm" ariaLabel="Close background jobs" onclick={onclose}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    </div>
  {/snippet}

  <div class="jobs">
    {#if loading && items.length === 0}
      <div class="loading"><Spinner size={16} /> Loading background jobs…</div>
    {:else if error && items.length === 0}
      <div class="load-error">
        <p role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if items.length === 0}
      <EmptyState
        title="No background jobs"
        description="This daemon has no supervised extraction, watcher, or packing work."
      >
        {#snippet icon()}<ActivityIcon size="22" />{/snippet}
      </EmptyState>
    {:else}
      {#if error}<p class="error" role="alert">{error}</p>{/if}

      {#if operations.length > 0}
        <section class="group" aria-labelledby="jobs-operations">
          <h3 id="jobs-operations">Operations</h3>
          <ul class="operation-list" aria-live="polite">
            {#each operations as job (job.name)}
              {@const title = jobTitle(job)}
              <li>
                <Card level="default" padding="md" ariaLabel={title}>
                  <div class="op-body">
                    <div class="op-summary">
                      <div class="op-head">
                        <div class="op-title">
                          <strong>{title}</strong>
                          <Chip size="xs" uppercase={false} tone={statusTone(job)} dot={job.status === "running"}>
                            {statusLabel(job)}
                          </Chip>
                        </div>
                        {#if job.kind === "photo-import" && job.can_cancel && !job.cancel_requested}
                          <Button
                            size="sm"
                            ariaLabel={`Cancel ${title.toLowerCase()}`}
                            disabled={cancelling.has(job.operation_id ?? "")}
                            onclick={() => void cancelPhotoImport(job)}
                          >
                            Cancel
                          </Button>
                        {/if}
                      </div>

                      <p class="op-meta">
                        {#if job.destination}
                          <span class="op-path" title={job.destination}>Into {job.destination}</span>
                        {/if}
                        <time datetime={job.started_at} title={formatDate(job.started_at)}>Started {relative(job.started_at)}</time>
                        {#if job.finished_at}
                          <time datetime={job.finished_at} title={formatDate(job.finished_at)}>Finished {relative(job.finished_at)}</time>
                        {/if}
                      </p>
                    </div>

                    {#if job.total_objects !== undefined}
                      {@const done = job.completed_objects ?? 0}
                      <div class="op-progress">
                        <div
                          class="bar"
                          role="progressbar"
                          aria-label={`${title} progress`}
                          aria-valuemin="0"
                          aria-valuemax={job.total_objects}
                          aria-valuenow={done}
                          aria-valuetext={`${done} of ${job.total_objects} ${unit(job)}`}
                        >
                          <span class:done={job.status === "completed"} style={`width: ${percent(job)}%`}></span>
                        </div>
                        <div class="progress-copy">
                          <span>{done.toLocaleString()} of {job.total_objects.toLocaleString()} {unit(job)}</span>
                          <span class="pct">{percentLabel(job)}</span>
                        </div>
                      </div>
                    {/if}

                    {#if job.cancel_requested && job.status === "running"}
                      <p class="op-note">Stops after the current group finishes.</p>
                    {/if}
                    {#if job.error}
                      <p class="job-error" role="alert">{job.error}</p>
                    {/if}
                  </div>

                  {#snippet footer()}
                    <div class="op-id">
                      <span>Job ID</span>
                      <code title={job.operation_id}>{job.operation_id}</code>
                      <CopyButton text={job.operation_id ?? ""} ariaLabel="Copy job ID" title="Copy job ID" />
                    </div>
                  {/snippet}
                </Card>
              </li>
            {/each}
          </ul>
        </section>
      {/if}

      {#if workers.length > 0}
        <section class="group" aria-labelledby="jobs-workers">
          <h3 id="jobs-workers">Daemon workers</h3>
          <Card level="default" padding="none">
            <ul class="worker-list">
              {#each workers as job (job.name)}
                <li class="worker">
                  <StatusDot status={workerDot(job.status)} label={statusLabel(job)} />
                  <div class="worker-main">
                    <span class="worker-name">{jobTitle(job)}</span>
                    <code class="worker-id" title={job.name}>{job.name}</code>
                  </div>
                  <div class="worker-state">
                    {#if job.status === "failed"}
                      <Chip size="xs" uppercase={false} tone="danger">Failed</Chip>
                    {:else if job.status !== "running"}
                      <span>{statusLabel(job)}</span>
                    {/if}
                    <time
                      datetime={job.finished_at ?? job.started_at}
                      title={formatDate(job.finished_at ?? job.started_at)}
                    >
                      {job.finished_at ? `Ended ${relative(job.finished_at)}` : `Started ${relative(job.started_at)}`}
                    </time>
                  </div>
                  {#if job.error}
                    <p class="job-error worker-error" role="alert">{job.error}</p>
                  {/if}
                </li>
              {/each}
            </ul>
          </Card>
        </section>
      {/if}
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

  .heading-copy {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }

  .heading-copy span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-medium);
  }

  .heading-copy strong {
    color: var(--text-primary);
    font-size: var(--font-size-lg);
    line-height: 1.25;
  }

  .heading-copy small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .drawer-actions {
    display: flex;
    gap: var(--space-2);
    flex-shrink: 0;
  }

  .jobs {
    display: grid;
    gap: var(--space-6);
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

  .group {
    display: grid;
    gap: var(--space-4);
    min-width: 0;
  }

  .group h3 {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
  }

  .operation-list,
  .worker-list {
    display: grid;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .operation-list {
    gap: var(--space-4);
  }

  .op-body {
    display: grid;
    gap: var(--space-5);
    min-width: 0;
  }

  .op-summary {
    display: grid;
    gap: var(--space-1);
    min-width: 0;
  }

  .op-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    min-height: 26px;
  }

  .op-title {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    min-width: 0;
  }

  .op-title strong {
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-lg);
    font-weight: var(--font-weight-semibold);
    line-height: 1.25;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .op-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: var(--space-1) var(--space-5);
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    min-width: 0;
  }

  .op-path {
    overflow: hidden;
    max-width: 100%;
    color: var(--text-secondary);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .op-progress {
    display: grid;
    gap: var(--space-3);
  }

  .bar {
    height: 6px;
    overflow: hidden;
    border-radius: 999px;
    background: var(--bg-inset);
    box-shadow: inset 0 0 0 1px var(--border-muted);
  }

  .bar > span {
    display: block;
    min-width: 6px;
    height: 100%;
    border-radius: inherit;
    background: var(--accent-blue);
    transition: width 0.4s ease;
  }

  .bar > span.done {
    background: var(--accent-green);
  }

  .progress-copy {
    display: flex;
    justify-content: space-between;
    gap: var(--space-4);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-variant-numeric: tabular-nums;
  }

  .pct {
    color: var(--text-muted);
  }

  .op-note {
    margin: 0;
    color: var(--accent-amber);
    font-size: var(--font-size-sm);
  }

  .op-id {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
    margin: calc(-1 * var(--space-2)) 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .op-id span {
    flex-shrink: 0;
  }

  .op-id code,
  .worker-id {
    overflow: hidden;
    min-width: 0;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .worker {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto;
    align-items: center;
    column-gap: var(--space-4);
    padding: var(--space-4) var(--space-6);
  }

  .worker + .worker {
    border-top: 1px solid var(--border-muted);
  }

  .worker-main {
    display: grid;
    min-width: 0;
  }

  .worker-name {
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .worker-state {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    white-space: nowrap;
  }

  .worker-state time {
    color: var(--text-muted);
  }

  .job-error {
    margin: 0;
    padding: var(--space-3) var(--space-4);
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent-red) 8%, transparent);
    color: var(--accent-red);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    overflow-wrap: anywhere;
  }

  .worker-error {
    grid-column: 2 / -1;
    margin-top: var(--space-3);
  }
</style>
