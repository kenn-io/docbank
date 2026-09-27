<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Checkbox, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import type { ProductionDraft, ProductionSet } from "./api.js";
  import { forkCurrentProductionDraft, StaleProductionForkError, type ForkAttempt } from "./forkDraft.js";

  let { session, set, draft, onrefresh, onauthfailure }: {
    session: string; set: ProductionSet; draft: ProductionDraft;
    onrefresh: () => void; onauthfailure: (cause: unknown) => void;
  } = $props();
  let confirmed = $state(false);
  let busy = $state(false);
  let error = $state("");
  let createdRevision = $state(0);
  let pending = $state<{ set: ProductionSet; draft: ProductionDraft; attempt: ForkAttempt } | null>(null);
  let controller = new AbortController();

  onDestroy(() => controller.abort());

  async function fork(): Promise<void> {
    if (busy || createdRevision || !pending && !confirmed) return;
    controller.abort();
    controller = new AbortController();
    const signal = controller.signal;
    busy = true;
    error = "";
    const replay = !!pending;
    pending ??= { set: { ...set }, draft: { ...draft }, attempt: {
      set_id: set.id, revision: draft.revision, etag: draft.etag, operation_id: crypto.randomUUID(),
    } };
    try {
      const result = await forkCurrentProductionDraft(session, pending.set, pending.draft, pending.attempt, signal, replay);
      signal.throwIfAborted();
      createdRevision = result.revision;
      onrefresh();
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof StaleProductionForkError || cause instanceof APIError && cause.status === 409) {
        pending = null;
        confirmed = false;
        error = cause instanceof StaleProductionForkError ? cause.message :
          "The new draft was rejected. Refresh the revision before trying again.";
      } else if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!signal.aborted) busy = false;
    }
  }
</script>

{#if pending || createdRevision || draft.state === "finalized" && draft.revision === set.head_revision}
  <section class="draft-fork" aria-labelledby="production-fork-heading">
    <div class="section-heading"><span>NEXT REVISION</span><strong id="production-fork-heading">Create a new draft</strong></div>
    {#if createdRevision}
      <p role="status">Created draft revision {createdRevision}</p>
    {:else}
      <p>Start another editable revision from this finalized production. Members and decisions carry forward for review; the new member list starts unsealed.</p>
      <Checkbox checked={confirmed} disabled={busy || !!pending}
        onchange={checked => { confirmed = checked; }}
        label="I want a new editable draft from this finalized revision." />
      <Button size="sm" surface="soft" disabled={busy || (!pending && !confirmed)} onclick={() => void fork()}>
        {busy ? "Creating…" : pending && error ? "Retry new draft" : "Create new draft"}
      </Button>
      {#if busy}<p role="status"><Spinner size={14} /> Checking finalized revision…</p>{/if}
      {#if error}
        <p class="error" role="alert">{error}</p>
        <Button size="sm" surface="soft" disabled={busy} onclick={onrefresh}>Refresh revision</Button>
      {/if}
    {/if}
  </section>
{/if}

<style>
  .draft-fork{display:grid;gap:var(--space-3);padding:var(--space-4);border:1px solid var(--border-muted);border-radius:var(--radius-lg);background:var(--bg-inset)}
  .section-heading{display:flex;align-items:center;justify-content:space-between;gap:var(--space-3)}
  .section-heading span{font-size:var(--font-size-xs);font-weight:var(--font-weight-bold);color:var(--text-muted)}
  p{margin:0;font-size:var(--font-size-sm);color:var(--text-secondary);line-height:1.5}
  .error{color:var(--accent-red)}
</style>
