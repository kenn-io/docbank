<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Checkbox, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import { loadProductionJobStatus, validProductionJobID } from "./jobStatus.js";
  import { admitCurrentProductionJob } from "./jobAdmission.js";
  import type { ProductionDraft, ProductionJobStatus, ProductionSet } from "./api.js";

  let { session, setID, set, draft, onauthfailure, onrefresh }: {
    session: string; setID: string; set?: ProductionSet; draft?: ProductionDraft;
    onauthfailure: (cause: unknown) => void; onrefresh?: () => void;
  } = $props();
  let jobID = $state("");
  let status = $state<ProductionJobStatus | null>(null);
  let loading = $state(false);
  let error = $state("");
  let controller = new AbortController();
  let admitController = new AbortController();
  let confirmed = $state(false);
  let admitting = $state(false);
  let admitError = $state("");
  let pending = $state<{ jobID: string; operationID: string; setID: string; revision: number; etag: number } | null>(null);

  onDestroy(() => { controller.abort(); admitController.abort(); });

  function changeID(): void {
    controller.abort();
    status = null;
    loading = false;
    error = "";
  }

  async function lookup(): Promise<void> {
    const requested = jobID.trim().toLowerCase();
    if (loading || !validProductionJobID(requested)) return;
    controller.abort();
    controller = new AbortController();
    const signal = controller.signal;
    loading = true;
    error = "";
    status = null;
    try {
      const result = await loadProductionJobStatus(session, setID, requested, signal);
      if (!signal.aborted) status = result;
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!signal.aborted) loading = false;
    }
  }

  async function start(): Promise<void> {
    if (admitting || !set || !draft || (!confirmed && !pending)) return;
    if (pending && (pending.setID !== set.id || pending.revision !== draft.revision ||
        pending.etag !== draft.etag || draft.state !== "finalized")) {
      admitError = "The selected finalized revision changed. Check the previous job status before starting another.";
      return;
    }
    if (draft.state !== "finalized") return;
    admitController.abort();
    admitController = new AbortController();
    const signal = admitController.signal;
    admitting = true;
    admitError = "";
    try {
      pending ??= { jobID: crypto.randomUUID(), operationID: crypto.randomUUID(),
        setID: set.id, revision: draft.revision, etag: draft.etag };
      const result = await admitCurrentProductionJob(session, set, draft,
        pending.jobID, pending.operationID, signal);
      if (signal.aborted) return;
      jobID = result.job_id;
      status = result;
      pending = null;
      confirmed = false;
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else admitError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!signal.aborted) admitting = false;
    }
  }

  async function checkPending(): Promise<void> {
    if (!pending || admitting) return;
    controller.abort();
    controller = new AbortController();
    const signal = controller.signal;
    loading = true;
    try {
      const result = await loadProductionJobStatus(session, setID, pending.jobID, signal);
      if (signal.aborted) return;
      if (result.revision !== pending.revision) {
        throw new Error("The pending job status disagreed with its finalized revision.");
      }
      jobID = result.job_id;
      status = result;
      pending = null;
      admitError = "";
      confirmed = false;
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else admitError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!signal.aborted) loading = false;
    }
  }
</script>

<section class="job-lookup" aria-labelledby="production-job-heading">
  <div class="section-heading"><div><span>RETAINED JOB</span><strong id="production-job-heading">Production job status</strong></div></div>
  <p>Job status is scoped to this set. Start a finalized revision here or enter a job ID from another client.</p>
  {#if pending || set && draft?.state === "finalized"}
    <div class="job-admission">
      <strong>{pending ? `Job attempt for finalized revision ${pending.revision}` : `Run finalized revision ${draft?.revision}`}</strong>
      {#if !pending}
        <Checkbox checked={confirmed} onchange={checked => { confirmed = checked; }}
          disabled={admitting} label="I am ready to run this finalized revision." />
      {/if}
      {#if draft?.state === "finalized"}
        <Button size="sm" disabled={admitting || loading || (!confirmed && !pending)} onclick={() => void start()}>
          {admitting ? "Starting…" : pending ? "Retry start" : "Start production job"}
        </Button>
      {/if}
      {#if admitError}<p class="error" role="alert">{admitError}</p>{/if}
      {#if pending}
        <p>Job ID for this attempt: <code>{pending.jobID}</code></p>
        <Button size="sm" surface="soft" disabled={loading || admitting} onclick={() => void checkPending()}>Check job status</Button>
      {/if}
      {#if admitError && onrefresh}<Button size="sm" surface="soft" onclick={onrefresh}>Refresh revision</Button>{/if}
    </div>
  {/if}
  <form onsubmit={event => { event.preventDefault(); void lookup(); }}>
    <label for="production-job-id">Job ID
      <input id="production-job-id" type="text" bind:value={jobID} oninput={changeID} disabled={!!pending || admitting}
        autocomplete="off" spellcheck="false" placeholder="xxxxxxxx-xxxx-4xxx-xxxx-xxxxxxxxxxxx" />
    </label>
    <Button size="sm" disabled={loading || !validProductionJobID(jobID.trim().toLowerCase())}
      type="submit">Look up job</Button>
  </form>
  {#if loading}<p class="loading" role="status"><Spinner size={14} /> Loading exact job status…</p>{/if}
  {#if error}<p class="error" role="alert">{error}</p>{/if}
  {#if status}
    <div class="job-status" role="region" aria-label={`Status for production job ${status.job_id}`}>
      <div class="status-heading"><strong>{status.state}</strong>
        <Button size="sm" surface="soft" disabled={loading} onclick={() => void lookup()}>Refresh status</Button></div>
      <dl>
        <div><dt>Job ID</dt><dd><code>{status.job_id}</code></dd></div>
        <div><dt>Revision</dt><dd>{status.revision}</dd></div>
        <div><dt>Revision digest</dt><dd><code>{status.revision_sha256}</code></dd></div>
        {#if status.receipt_sha256}
          <div><dt>Verified receipt</dt><dd><code>{status.receipt_sha256}</code></dd></div>
        {/if}
      </dl>
    </div>
  {/if}
</section>

<style>
  .job-lookup{display:grid;gap:var(--space-3);padding:var(--space-4);border:1px solid var(--border-muted);border-radius:var(--radius-lg);background:var(--bg-inset)}
  .job-admission{display:grid;justify-items:start;gap:var(--space-2);padding:var(--space-3);border:1px solid var(--border-muted);border-radius:var(--radius-md)}
  .section-heading,.status-heading{display:flex;align-items:center;justify-content:space-between;gap:var(--space-3);flex-wrap:wrap}
  .section-heading div{display:grid;gap:var(--space-1)}
  .section-heading span{font-size:var(--font-size-xs);font-weight:var(--font-weight-bold);color:var(--text-muted)}
  .section-heading strong{font-size:var(--font-size-lg)}
  p{margin:0;font-size:var(--font-size-sm);color:var(--text-secondary)}
  form{display:flex;align-items:end;gap:var(--space-2);flex-wrap:wrap}
  label{display:grid;gap:var(--space-1);min-width:min(100%,320px);font-size:var(--font-size-sm)}
  input{width:100%;box-sizing:border-box;padding:var(--space-2);border:1px solid var(--border-muted);border-radius:var(--radius-md);background:var(--bg-raised);color:var(--text-primary);font:inherit}
  .loading{display:flex;align-items:center;gap:var(--space-2)}
  .error{color:var(--accent-red)}
  .job-status{display:grid;gap:var(--space-2);padding:var(--space-3);border:1px solid var(--border-default);border-radius:var(--radius-md);background:var(--bg-raised)}
  .status-heading strong{text-transform:capitalize}
  dl{display:grid;gap:var(--space-2);margin:0}
  dl div{display:grid;gap:var(--space-1)}
  dt{font-size:var(--font-size-xs);color:var(--text-muted)}
  dd{margin:0;font-size:var(--font-size-sm);overflow-wrap:anywhere}
</style>
