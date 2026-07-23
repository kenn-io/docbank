<script lang="ts">
  import { onMount } from "svelte";
  import ActivityIcon from "@lucide/svelte/icons/activity";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Card,
    Chip,
    DetailDrawer,
    EmptyState,
    IconButton,
    Spinner,
    type ChipTone,
  } from "@kenn-io/kit-ui";
  import { APIError, listJobs, type Job } from "./api.js";
  import { formatDate } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();

  let items = $state<Job[]>([]);
  let loading = $state(true);
  let error = $state("");
  let generation = 0;

  const running = $derived(
    items.filter((job) => job.status === "running").length,
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
    try {
      const next = await listJobs(session);
      if (request !== generation) return;
      items = next;
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

  function statusTone(status: Job["status"]): ChipTone {
    switch (status) {
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
</script>

<DetailDrawer
  width="min(620px, 100vw)"
  ariaLabel="Daemon background jobs"
  {onclose}
>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>DAEMON ACTIVITY</span>
        <strong>Background jobs</strong>
        <small>{running} running · {items.length} total</small>
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
      <div class="job-list" aria-live="polite">
        {#each items as job (job.name)}
          <Card
            level="default"
            padding="sm"
            eyebrow="BACKGROUND JOB"
            title={job.name}
          >
            {#snippet actions()}
              <Chip size="xs" tone={statusTone(job.status)} dot={job.status === "running"}>
                {job.status}
              </Chip>
            {/snippet}
            <dl>
              <div><dt>Started</dt><dd>{formatDate(job.started_at)}</dd></div>
              <div>
                <dt>Finished</dt>
                <dd>{job.finished_at ? formatDate(job.finished_at) : "Still running"}</dd>
              </div>
            </dl>
            {#if job.error}
              <p class="job-error" role="alert">{job.error}</p>
            {/if}
          </Card>
        {/each}
      </div>
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

  .jobs {
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

  .job-list {
    display: grid;
    gap: var(--space-3);
  }

  dl {
    display: grid;
    gap: var(--space-3);
    margin: 0;
  }

  dl > div {
    display: grid;
    grid-template-columns: 72px minmax(0, 1fr);
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
    font-size: var(--font-size-sm);
  }

  .job-error {
    margin: 0;
    padding: var(--space-3);
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent-red) 8%, transparent);
    color: var(--accent-red);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    overflow-wrap: anywhere;
  }
</style>
