<script lang="ts">
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import { Button, IconButton, Spinner, SelectDropdown, Table } from "@kenn-io/kit-ui";
  import DrawerShell from "./DrawerShell.svelte";
  import { APIError } from "./api-transport.js";
  import { type Job, type LaneControl } from "./generated/docbank.js";
  import { jobLanes, laneTitle } from "./jobs.js";
  import { formatBytes } from "./format.js";

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
  const visible = $derived(lanes.filter((lane) => lane.visible));
  const busy = $derived(lanes.filter((lane) => lane.active).length);

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
      loadError = "";
    }
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

  async function setControl(
    control: LaneControl,
    paused: boolean,
    concurrency: number,
  ): Promise<void> {
    const lane = control.lane;
    if (updating.has(lane)) return;
    actionError = "";
    updating = new Set(updating).add(lane);
    try {
      await generated.setLaneControl(
        lane,
        { paused, concurrency },
        { "If-Match": `"${control.revision}"` },
        { session },
      );
      await refresh(true);
    } catch (cause) {
      if (generation < 0) return;
      if (cause instanceof APIError && cause.status === 412) {
        await refresh(true);
        if (generation >= 0) actionError = "Worker settings changed elsewhere. Try again.";
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

  function laneState(lane: Lane): string {
    if (lane.control?.paused) return "Paused";
    if (lane.active) return "Running";
    if (lane.members.some((member) => member.status === "queued")) return "Queued";
    if (lane.failed) return "Failed";
    if (lane.members.find((member) => member.status !== "queued")?.error) return "Waiting";
    return lane.status && lane.status !== "running" ? statusLabel(lane.status) : "Idle";
  }

  function unit(lane: string): string {
    return lane === "photo_import" ? "groups" : lane === "photo_checkout" ? "files" : "objects";
  }

  function percent(completed: number, total: number): number {
    const rounded = Math.min(100, Math.round((completed * 100) / total));
    return completed < total ? Math.min(99, rounded) : rounded;
  }
</script>

{#snippet laneControls(lane: Lane)}
  {@const control = lane.control}
  {#if control?.can_set_concurrency}
    <div class="worker-count">
      <SelectDropdown
        title={`Workers for ${lane.title}`}
        value={String(control.concurrency)}
        options={[1, 2, 3, 4].map((value) => ({
          value: String(value),
          label: `${value} ${value === 1 ? "worker" : "workers"}`,
        }))}
        disabled={updating.has(lane.lane)}
        onchange={(value) => void setControl(control, control.paused, Number(value))}
      />
    </div>
  {/if}
  {#if control}
    <Button
      size="sm"
      ariaLabel={`${control.paused ? "Resume" : "Pause"} ${lane.title}`}
      disabled={updating.has(lane.lane)}
      onclick={() => void setControl(control, !control.paused, control.concurrency)}
      >{control.paused ? "Resume" : "Pause"}</Button
    >
  {/if}
{/snippet}

{#snippet cancellation(member: Job, title: string)}
  {#if member.cancel_requested && member.can_cancel}
    <span class="muted">Stopping…</span>
  {:else if member.can_cancel}
    <Button
      size="sm"
      ariaLabel={`Cancel ${title} ${member.operation_id}`}
      disabled={cancelling.has(member.operation_id!)}
      onclick={() => void cancelJob(member)}>Cancel</Button
    >
  {/if}
{/snippet}

<DrawerShell title="Running now" closeLabel="Close background jobs" {onclose}>
  {#snippet actions()}
    {#if busy > 0}<span class="busy-count">{busy} running</span>{/if}
    <IconButton
      size="sm"
      ariaLabel="Refresh background jobs"
      disabled={loading}
      onclick={() => void refresh()}
    >
      <RefreshCwIcon size="14" aria-hidden="true" />
    </IconButton>
  {/snippet}

  <div class="jobs">
    {#if actionError}<p class="error" role="alert">{actionError}</p>{/if}
    {#if loading && lanes.length === 0}
      <div class="loading"><Spinner size={16} /> Loading…</div>
    {:else if loadError && lanes.length === 0}
      <div class="load-error">
        <p class="error" role="alert">{loadError}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else}
      {#if loadError}<p class="error" role="alert">{loadError}</p>{/if}
      <div class="job-list" aria-live="polite">
        {#if visible.length === 0 && !loadError}
          <p class="quiet">Nothing running</p>
        {/if}
        {#each visible as lane (lane.key)}
          {@const state = laneState(lane)}
          <section class="job" aria-label={lane.title}>
            <div class="job-row">
              <strong class="job-name">{lane.title}</strong>
              {#if state !== "Running"}<span class="state" data-state={state}>{state}</span>{/if}
              <div class="controls">
                {@render laneControls(lane)}
                {#if lane.members.filter((member) => member.can_cancel).length === 1}
                  {@render cancellation(
                    lane.members.find((member) => member.can_cancel)!,
                    lane.title,
                  )}
                {/if}
              </div>
            </div>
            {#if lane.total !== undefined}
              {@const share = percent(lane.completed, lane.total)}
              <div class="progress">
                <span>{lane.completed} of {lane.total} {unit(lane.lane)}</span>
                <div
                  class="progress-track"
                  role="progressbar"
                  aria-label={`${lane.title} progress`}
                  aria-valuemin="0"
                  aria-valuemax={lane.total}
                  aria-valuenow={lane.completed}
                >
                  <span style:width={`${share}%`}></span>
                </div>
              </div>
            {/if}
            {#each lane.members.filter((member) => member.busy || member.status === "queued" || member.status === "failed" || member.error) as member (member.name)}
              {#if member.error || member.total_bytes || member.checkout_results || (member.kind && member.kind !== lane.lane) || lane.members.filter((member) => member.can_cancel).length > 1}
                <div class="operation">
                  {#if member.operation_id}
                    <div class="operation-row">
                      {#if member.kind && member.kind !== lane.lane}
                        <span>{laneTitle(member.kind)}</span>
                      {/if}
                      {#if lane.members.filter((member) => member.can_cancel).length > 1}
                        <code>{member.operation_id}</code>
                      {/if}
                      {#if member.total_bytes}
                        <span
                          >Verified {formatBytes(member.processed_bytes ?? 0)} of {formatBytes(
                            member.total_bytes,
                          )}</span
                        >
                      {/if}
                      {#if member.checkout_results}
                        <span
                          >{Object.entries(member.checkout_results)
                            .map(([state, count]) => `${count} ${state}`)
                            .join(", ")}</span
                        >
                      {/if}
                      {#if lane.members.filter((member) => member.can_cancel).length > 1}
                        {@render cancellation(member, lane.title)}
                      {/if}
                    </div>
                  {/if}
                  {#if member.error}<p class="error" role="alert">{member.error}</p>{/if}
                </div>
              {/if}
            {/each}
          </section>
        {/each}
      </div>
      {#if lanes.length > 0}
        <Table bare unsorted ariaLabel="All workers" class="workers">
          <thead
            ><tr
              ><th scope="col">Worker</th><th scope="col">Status</th><th scope="col"
                ><span class="kit-sr-only">Controls</span></th
              ></tr
            ></thead
          >
          <tbody>
            {#each lanes as lane (lane.key)}
              <tr>
                <th scope="row">{lane.title}</th>
                <td
                  ><span class="state" data-state={laneState(lane)}>{laneState(lane)}</span>
                  {#if !lane.visible}
                    {#each lane.members.filter((member) => member.error) as member (member.name)}
                      <p class="error">{member.error}</p>
                    {/each}
                  {/if}
                </td>
                <td
                  >{#if !lane.visible}<div class="controls">
                      {@render laneControls(lane)}
                      {#each lane.members.filter((member) => member.can_cancel) as member (member.name)}
                        {@render cancellation(member, lane.title)}
                      {/each}
                    </div>{/if}</td
                >
              </tr>
            {/each}
          </tbody>
        </Table>
      {/if}
    {/if}
  </div>
</DrawerShell>

<style>
  .job-row,
  .controls,
  .operation-row,
  .progress {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
  }

  .controls {
    margin-left: auto;
    flex-shrink: 0;
  }
  .busy-count,
  .state,
  .muted {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .jobs {
    padding: var(--space-4) var(--space-5);
    display: grid;
    align-content: start;
    gap: var(--space-4);
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
    gap: var(--space-3);
  }
  .quiet {
    margin: 0;
    padding-block: var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .job-list {
    min-width: 0;
  }
  .job {
    padding-block: var(--space-3);
    border-bottom: 1px solid var(--border-muted);
  }
  .job:first-child {
    padding-top: 0;
  }
  .job-name {
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-medium);
  }
  .state {
    white-space: nowrap;
  }
  .state[data-state="Failed"],
  .error {
    color: var(--accent-red);
  }
  .state[data-state="Paused"] {
    color: var(--accent-amber);
  }
  .error {
    margin: 0;
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
  }
  .progress {
    margin-top: var(--space-2);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-variant-numeric: tabular-nums;
  }
  .progress-track {
    height: 4px;
    flex: 1;
    border-radius: 999px;
    overflow: hidden;
    background: var(--bg-inset);
  }
  .progress-track > span {
    display: block;
    height: 100%;
    background: var(--accent-blue);
  }
  .operation {
    margin-top: var(--space-2);
  }
  .operation-row {
    justify-content: flex-end;
    flex-wrap: wrap;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  code {
    margin-right: auto;
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
  }
  .jobs :global(.workers) {
    width: 100%;
    border-collapse: collapse;
  }
  th,
  td {
    padding: var(--space-2) 0;
    border-bottom: 1px solid var(--border-muted);
    text-align: left;
  }
  th {
    font-weight: var(--font-weight-medium);
    font-size: var(--font-size-sm);
  }
  thead th {
    color: var(--text-muted);
  }
  tbody tr {
    height: 2.25rem;
  }
  td:last-child {
    text-align: right;
  }
  td .controls {
    justify-content: flex-end;
  }
  .worker-count {
    width: 100px;
  }
  .worker-count :global(.kit-select-dropdown) {
    min-width: 0;
  }
  @media (max-width: 640px) {
    .job-row {
      flex-wrap: wrap;
    }
    .controls {
      gap: var(--space-2);
    }
    .jobs {
      padding-inline: var(--space-3);
    }
    .busy-count {
      white-space: nowrap;
    }
  }
</style>
