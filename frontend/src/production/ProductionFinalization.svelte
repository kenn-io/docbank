<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { Button, Checkbox, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import type { BatesNamespace } from "../bates.js";
  import { listProductionNumberingNamespaces, type ProductionDraft, type ProductionSet } from "./api.js";
  import { finalizeCurrentProductionDraft, StaleProductionDraftError, type FinalizeAttempt } from "./finalizeDraft.js";

  let { session, set, draft, onrefresh, onauthfailure }: {
    session: string; set: ProductionSet; draft: ProductionDraft;
    onrefresh: () => void; onauthfailure: (cause: unknown) => void;
  } = $props();
  let namespaces = $state<BatesNamespace[]>([]);
  let cursor = $state("");
  let namespaceID = $state("");
  let confirmed = $state(false);
  let loading = $state(false);
  let busy = $state(false);
  let error = $state("");
  let completed = $state(false);
  let pending = $state<{ set: ProductionSet; draft: ProductionDraft; attempt: FinalizeAttempt } | null>(null);
  let listController = new AbortController();
  let finalizeController = new AbortController();

  onMount(() => { void load(""); });
  onDestroy(() => { listController.abort(); finalizeController.abort(); });

  async function load(nextCursor: string): Promise<void> {
    if (loading) return;
    listController.abort();
    listController = new AbortController();
    const signal = listController.signal;
    loading = true;
    error = "";
    try {
      const page = await listProductionNumberingNamespaces(session, nextCursor, signal);
      signal.throwIfAborted();
      if (!Array.isArray(page.items) || page.next_cursor !== undefined && typeof page.next_cursor !== "string")
        throw new Error("The Bates namespace list was incomplete.");
      namespaces = nextCursor ? [...namespaces, ...page.items.filter(item => !namespaces.some(n => n.namespace_id === item.namespace_id))] : page.items;
      cursor = page.next_cursor ?? "";
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!signal.aborted) loading = false;
    }
  }

  async function finalize(): Promise<void> {
    if (busy || completed || !pending && (!confirmed || !namespaceID || !namespaces.some(n => n.namespace_id === namespaceID))) return;
    finalizeController.abort();
    finalizeController = new AbortController();
    const signal = finalizeController.signal;
    busy = true;
    error = "";
    const replay = !!pending;
    pending ??= { set: { ...set }, draft: { ...draft }, attempt: {
      set_id: set.id, revision: draft.revision, etag: draft.etag,
      namespace_id: namespaceID, snapshot_id: crypto.randomUUID(), operation_id: crypto.randomUUID(),
    } };
    try {
      await finalizeCurrentProductionDraft(session, pending.set, pending.draft, pending.attempt, signal, replay);
      signal.throwIfAborted();
      completed = true;
      onrefresh();
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof StaleProductionDraftError || cause instanceof APIError && cause.status === 409) {
        pending = null;
        confirmed = false;
        error = cause instanceof StaleProductionDraftError ? cause.message :
          "Finalization was rejected. Refresh the draft before trying again.";
      } else if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!signal.aborted) busy = false;
    }
  }
</script>

{#if pending || completed || draft.state === "draft" && draft.membership_sealed && draft.numbering_recipe_id === "bates-sequential-v1"}
  <section class="finalization" aria-labelledby="production-finalize-heading">
    <div class="section-heading"><span>FINAL REVIEW</span><strong id="production-finalize-heading">Finalize production</strong></div>
    {#if completed}
      <p role="status">Finalized revision {pending?.attempt.revision ?? draft.revision}</p>
    {:else}
      <p>Finalization locks the reviewed draft and assigns Bates numbers from the selected namespace.</p>
      {#if loading}<p role="status"><Spinner size={14} /> Loading Bates namespaces…</p>{/if}
      {#if !loading && namespaces.length === 0 && !error}
        <p>Create a Bates namespace in Bates export, then refresh this list.</p>
      {/if}
      <SelectDropdown title="Bates namespace" value={namespaceID}
        options={namespaces.map(n => ({ value: n.namespace_id, label: `${n.prefix || "(no prefix)"} · ${n.padding} digits${n.suffix ? ` · ${n.suffix}` : ""}` }))}
        disabled={busy || !!pending || namespaces.length === 0}
        onchange={value => { namespaceID = value; confirmed = false; }} />
      {#if cursor}<Button size="sm" surface="soft" disabled={loading || busy || !!pending} onclick={() => void load(cursor)}>Load more namespaces</Button>{/if}
      <Checkbox checked={confirmed} disabled={busy || !!pending || !namespaceID}
        onchange={checked => { confirmed = checked; }}
        label="I reviewed the member list and am ready to finalize this production." />
      <Button size="sm" tone="info" disabled={busy || (!pending && (!confirmed || !namespaceID))}
        onclick={() => void finalize()}>{busy ? "Finalizing…" : pending && error ? "Retry finalization" : "Finalize production"}</Button>
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      {#if error}
        <Button size="sm" surface="soft" disabled={busy} onclick={onrefresh}>Refresh draft</Button>
      {/if}
    {/if}
  </section>
{/if}

<style>
  .finalization{display:grid;gap:var(--space-3);padding:var(--space-4);border:1px solid var(--border-muted);border-radius:var(--radius-lg);background:var(--bg-inset)}
  .section-heading{display:flex;align-items:center;justify-content:space-between;gap:var(--space-3)}
  .section-heading span{font-size:var(--font-size-xs);font-weight:var(--font-weight-bold);color:var(--text-muted)}
  p{margin:0;font-size:var(--font-size-sm);color:var(--text-secondary);line-height:1.5}
  .error{color:var(--accent-red)}
</style>
