<script lang="ts">
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import ActivityIcon from "@lucide/svelte/icons/activity";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Card,
    DetailDrawer,
    EmptyState,
    IconButton,
    Spinner,
    SelectDropdown,
  } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { type Job, type LaneControl } from "./generated/docbank.js";
  import { jobLanes } from "./jobs.js";
  import { formatDate } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();

  let items = $state<Job[]>([]);
  let loading = $state(true);
  let loadError = $state("");
  let actionError = $state("");
  let controls = $state<LaneControl[]>([]);
  let generation = 0;
  let poll: ReturnType<typeof setTimeout> | undefined;
  let cancelling = $state(new Set<string>());

  let updating = $state(new Set<string>());
  const lanes = $derived(jobLanes(items, controls));
  const running = $derived(
    items.filter((job) => job.status === "running").length,
  );

  onMount(() => {
    void refresh();
    return () => {
      generation = -1;
      clearTimeout(poll);
    };
  });

  async function refresh(quiet = false): Promise<void> {
    if (generation < 0) return;
    clearTimeout(poll);
    const request = ++generation;
    if (!quiet) {
      loading = true;
      actionError = "";
    }
    loadError = "";
    try {
      const next = await generated.listJobs({ session });
      if (request !== generation) return;
      items = next.items;
      controls = next.lanes ?? [];
      loadError = next.lane_controls_error ?? "";
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof APIError && cause.status === 401) {
        handleFailure(cause);
        return;
      }
      loadError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) {
        loading = false;
        poll = setTimeout(() => void refresh(true), 2000);
      }
    }
  }

  async function cancelJob(job: Job): Promise<void> {
    if (!job.operation_id || !job.can_cancel) return;
    actionError = "";
    const id = job.operation_id;
    cancelling = new Set(cancelling).add(id);
    try {
      await generated.cancelStorageOperation(id, { session });
      await refresh(true);
    } catch (cause) {
      if (generation < 0) return;
      handleFailure(cause);
      if (cause instanceof APIError && cause.status === 409) await refresh(true);
    } finally {
      if (generation >= 0) {
        const next = new Set(cancelling);
        next.delete(id);
        cancelling = next;
      }
    }
  }

  function handleFailure(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      generation = -1;
      clearTimeout(poll);
      onauthfailure(cause);
      onclose();
      return;
    }
    actionError = cause instanceof Error ? cause.message : String(cause);
  }

  async function setControl(control: LaneControl, paused: boolean, concurrency: number): Promise<void> {
    const lane = control.lane;
    if (updating.has(lane)) return;
    actionError = "";
    updating = new Set(updating).add(lane);
    try {
      await generated.setLaneControl(lane, { paused, concurrency }, { "If-Match": `"${control.revision}"` }, { session });
      await refresh(true);
    } catch (cause) {
      if (generation < 0) return;
      if (cause instanceof APIError && cause.status === 412) {
        await refresh(true);
        if (generation >= 0) actionError = "Lane settings changed elsewhere. Review the current settings and try again.";
      } else {
        handleFailure(cause);
      }
    } finally {
      if (generation >= 0) {
        const next = new Set(updating);
        next.delete(lane);
        updating = next;
      }
    }
  }

  type Lane = ReturnType<typeof jobLanes>[number];

  function statusLabel(status: Job["status"]): string {
    return status.charAt(0).toUpperCase() + status.slice(1);
  }

  function laneState(lane: Lane): { tone: string; label: string; details: string[] } {
    const paused = lane.control?.paused === true;
    const details: string[] = [];
    if (paused) details.push(lane.status ? statusLabel(lane.status) : "Idle");
    if (lane.members.length > 1) details.push(`${lane.members.length} operations`);
    if (lane.active && lane.total === undefined) details.push("Progress unavailable");
    if (paused) return { tone: "paused", label: "Paused", details };
    if (!lane.status) return { tone: "idle", label: "Idle", details };
    return { tone: lane.status, label: statusLabel(lane.status), details };
  }

  function unit(kind: string | undefined): string {
    return kind === "photo_import" ? "groups" : "objects";
  }

  function percent(completed: number, total: number): number {
    const rounded = Math.min(100, Math.round((completed * 100) / total));
    return completed < total ? Math.min(99, rounded) : rounded;
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
        <span>Daemon activity</span>
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
    {#if actionError}<p class="error" role="alert">{actionError}</p>{/if}
    {#if loading && lanes.length === 0}
      <div class="loading"><Spinner size={16} /> Loading background jobs…</div>
    {:else if lanes.length === 0}
      {#if loadError}
        <div class="load-error">
          <p role="alert">{loadError}</p>
          <Button size="sm" onclick={() => void refresh()}>Try again</Button>
        </div>
      {/if}
      <EmptyState
        title="No background jobs"
      >
        {#snippet icon()}<ActivityIcon size="22" />{/snippet}
      </EmptyState>
    {:else}
      {#if loadError}<p class="error" role="alert">{loadError}</p>{/if}
      <div class="job-list" aria-live="polite">
        {#each lanes as lane (lane.key)}
          {@const control = lane.control}
          {@const state = laneState(lane)}
          <Card level="default" padding="sm" title={lane.title} meta={control || loadError ? undefined : "Read-only"}>
            {#snippet actions()}
              {#if control?.can_set_concurrency}
                <SelectDropdown title={`Concurrency ${lane.title}`} value={String(control.concurrency)} options={[1, 2, 3, 4].map((value) => ({ value: String(value), label: `${value} ${value === 1 ? "worker" : "workers"}` }))} disabled={updating.has(lane.lane)} onchange={(value) => void setControl(control, control.paused, Number(value))} />
              {/if}
              {#if control}
                <Button size="sm" ariaLabel={`${control.paused ? "Resume" : "Pause"} ${lane.title}`} disabled={updating.has(lane.lane)} onclick={() => void setControl(control, !control.paused, control.concurrency)}>{control.paused ? "Resume" : "Pause"}</Button>
              {/if}
            {/snippet}
            <div class="lane">
              <p class="state" data-tone={state.tone}>
                <span class="state-label">{state.label}</span>
                {#each state.details as detail (detail)}<span class="state-detail">{detail}</span>{/each}
              </p>
              {#if lane.total !== undefined}
                {@const share = percent(lane.completed, lane.total)}
                <div class="progress">
                  <div class="progress-copy">
                    <span>{lane.completed} of {lane.total} {unit(lane.lane)}</span>
                    <span>{share}%</span>
                  </div>
                  <div class="progress-track" role="progressbar" aria-label={`${lane.title} progress`} aria-valuemin="0" aria-valuemax={lane.total} aria-valuenow={lane.completed}>
                    <span style:width={`${share}%`}></span>
                  </div>
                </div>
              {/if}
              {#if lane.members.length > 0}
                <ul class="operations" role="list" aria-label={`${lane.title} operations`}>
                  {#each lane.members as member (member.name)}
                    <li class="operation">
                      {#if member.operation_id}
                        <div class="operation-head">
                          <code class="operation-id">{member.operation_id}</code>
                          {#if member.can_cancel && !member.cancel_requested}
                            <Button size="sm" ariaLabel={`Cancel ${lane.title} ${member.operation_id}`} disabled={cancelling.has(member.operation_id)} onclick={() => void cancelJob(member)}>Cancel</Button>
                          {/if}
                        </div>
                      {/if}
                      <dl class="facts">
                        {#if lane.members.length > 1}
                          <div><dt>Status</dt><dd class="state" data-tone={member.status}><span class="state-label">{statusLabel(member.status)}</span></dd></div>
                        {/if}
                        <div><dt>Started</dt><dd>{formatDate(member.started_at)}</dd></div>
                        {#if member.finished_at}<div><dt>Finished</dt><dd>{formatDate(member.finished_at)}</dd></div>{/if}
                        {#if member.operation_id && member.total_objects !== undefined && member.total_objects > 0 && lane.members.length > 1}
                          <div><dt>Progress</dt><dd>{member.completed_objects ?? 0} of {member.total_objects} {unit(member.kind)}</dd></div>
                        {/if}
                      </dl>
                      {#if member.cancel_requested && (member.status === "running" || member.status === "queued")}
                        <p class="note">Cancellation requested. The job stops at its next safe point.</p>
                      {/if}
                      {#if member.error}<p class="job-error" role="alert">{member.error}</p>{/if}
                    </li>
                  {/each}
                </ul>
              {/if}
            </div>
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
    font-weight: var(--font-weight-medium);
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
    gap: var(--space-4);
  }

  .lane {
    display: grid;
    gap: var(--space-4);
    min-width: 0;
  }

  /* Status is plain text led by a tone dot, so it never reads as a control. */
  .state {
    --state-color: var(--text-muted);
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .state[data-tone="running"] {
    --state-color: var(--accent-blue);
  }

  .state[data-tone="completed"] {
    --state-color: var(--accent-green);
  }

  .state[data-tone="failed"] {
    --state-color: var(--accent-red);
  }

  .state[data-tone="paused"] {
    --state-color: var(--accent-amber);
  }

  .state-label {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--text-primary);
    font-weight: var(--font-weight-medium);
  }

  .state-label::before {
    content: "";
    box-sizing: border-box;
    width: 8px;
    height: 8px;
    flex-shrink: 0;
    border-radius: 50%;
    background: var(--state-color);
  }

  /* Waiting and inactive states use a hollow ring instead of a filled dot. */
  .state:is([data-tone="queued"], [data-tone="idle"], [data-tone="cancelled"]) .state-label::before {
    border: 1.5px solid var(--state-color);
    background: transparent;
  }

  .facts .state-label {
    color: var(--text-secondary);
    font-weight: 400;
  }

  .state-detail::before {
    content: "·";
    margin-right: var(--space-2);
  }

  .progress {
    display: grid;
    gap: var(--space-2);
  }

  .progress-copy {
    display: flex;
    justify-content: space-between;
    gap: var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }

  .progress-track {
    height: 6px;
    overflow: hidden;
    border-radius: 999px;
    background: var(--bg-inset);
    box-shadow: inset 0 0 0 1px var(--border-muted);
  }

  .progress-track > span {
    display: block;
    height: 100%;
    border-radius: inherit;
    background: var(--accent-blue);
  }

  .operations {
    display: grid;
    margin: 0;
    padding: 0;
    list-style: none;
    border-top: 1px solid var(--border-muted);
  }

  .operation {
    display: grid;
    gap: var(--space-2);
    min-width: 0;
    padding-top: var(--space-3);
  }

  .operation + .operation {
    margin-top: var(--space-3);
    border-top: 1px solid var(--border-muted);
  }

  .operation-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    min-width: 0;
  }

  .operation-id {
    min-width: 0;
    color: var(--text-primary);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    overflow-wrap: anywhere;
  }

  /* Facts always read as aligned label/value rows; the drawer is never wider than 620px. */
  .facts {
    display: grid;
    grid-template-columns: 4.5rem minmax(0, 1fr);
    align-items: baseline;
    gap: var(--space-1) var(--space-3);
    min-width: 0;
    margin: 0;
  }

  .facts > div {
    display: contents;
  }

  dt {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-medium);
  }

  dd {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .note {
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
