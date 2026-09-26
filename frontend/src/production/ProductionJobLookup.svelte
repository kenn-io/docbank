<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import { loadProductionJobStatus, validProductionJobID } from "./jobStatus.js";
  import type { ProductionJobStatus } from "./api.js";

  let { session, setID, onauthfailure }: {
    session: string; setID: string; onauthfailure: (cause: unknown) => void;
  } = $props();
  let jobID = $state("");
  let status = $state<ProductionJobStatus | null>(null);
  let loading = $state(false);
  let error = $state("");
  let controller = new AbortController();

  onDestroy(() => controller.abort());

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
</script>

<section class="job-lookup" aria-labelledby="production-job-heading">
  <div class="section-heading"><div><span>RETAINED JOB</span><strong id="production-job-heading">Production job status</strong></div></div>
  <p>Enter a job ID from a production started through the CLI or MCP. Status is scoped to this set.</p>
  <form onsubmit={event => { event.preventDefault(); void lookup(); }}>
    <label for="production-job-id">Job ID
      <input id="production-job-id" type="text" bind:value={jobID} oninput={changeID}
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
