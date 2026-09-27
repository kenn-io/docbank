<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import { appendProductionMember, getProductionDraftAt, getProductionSet, listProductionMembers,
    type ProductionDraft, type ProductionMember, type ProductionPreparedMember, type ProductionSet } from "./api.js";
  import { loadCurrentMemberAppend, StaleMemberAppendError } from "./memberAppend.js";

  let { session, draft, sets, targetMembers, targetCursor, onrefresh, onstale, onauthfailure }: {
    session: string; draft: ProductionDraft; sets: ProductionSet[];
    targetMembers: ProductionMember[]; targetCursor: string;
    onrefresh: () => void; onstale: () => void; onauthfailure: (cause: unknown) => void;
  } = $props();
  const sourceOptions = $derived([{ value: "", label: "Choose a retained set" },
    ...sets.map(set => ({ value: set.id, label: set.name }))]);
  let sourceSetID = $state("");
  let sourceRevision = $state(0);
  let sourceETag = $state(0);
  let sourceMembers = $state<ProductionPreparedMember[]>([]);
  let sourceCursor = $state("");
  let sourceLoading = $state(false);
  let sourceError = $state("");
  let sending = $state(false);
  let appendError = $state("");
  let pending = $state<{ member: ProductionPreparedMember; operationID: string } | null>(null);
  let sourceController = new AbortController();
  let sendController = new AbortController();

  onDestroy(() => { sourceController.abort(); sendController.abort(); });

  function fail(cause: unknown): string {
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      return "";
    }
    return cause instanceof Error ? cause.message : String(cause);
  }

  async function chooseSource(setID: string): Promise<void> {
    if (sending || pending) return;
    sourceController.abort();
    sourceController = new AbortController();
    const signal = sourceController.signal;
    sourceSetID = setID;
    sourceRevision = 0;
    sourceETag = 0;
    sourceMembers = [];
    sourceCursor = "";
    sourceError = "";
    if (!setID) return;
    sourceLoading = true;
    try {
      const currentSet = await getProductionSet(session, setID, signal);
      if (currentSet.id !== setID || currentSet.head_revision < 1) throw new Error("The source set identity changed.");
      const currentDraft = await getProductionDraftAt(session, setID, currentSet.head_revision, signal);
      if (currentDraft.set_id !== setID || currentDraft.revision !== currentSet.head_revision) {
        throw new Error("The source revision disagreed with the selected set.");
      }
      sourceRevision = currentDraft.revision;
      sourceETag = currentDraft.etag;
      await loadPage("", signal);
    } catch (cause) {
      if (!signal.aborted) sourceError = fail(cause);
    } finally {
      if (!signal.aborted) sourceLoading = false;
    }
  }

  async function loadPage(cursor: string, signal = sourceController.signal): Promise<void> {
    if (!sourceSetID || !sourceRevision) return;
    sourceLoading = true;
    sourceError = "";
    try {
      const page = await listProductionMembers(session, sourceSetID, sourceRevision, cursor, signal);
      const fresh = await getProductionDraftAt(session, sourceSetID, sourceRevision, signal);
      if (signal.aborted) return;
      if (fresh.set_id !== sourceSetID || fresh.revision !== sourceRevision || fresh.etag !== sourceETag) {
        sourceMembers = [];
        sourceCursor = "";
        throw new Error("The source set changed. Refresh its members before copying.");
      }
      sourceMembers = cursor ? [...sourceMembers, ...page.items.filter(item => !sourceMembers.some(existing => existing.id === item.id))] : page.items;
      sourceCursor = page.next_cursor;
    } catch (cause) {
      if (!signal.aborted) sourceError = fail(cause);
    } finally {
      if (!signal.aborted) sourceLoading = false;
    }
  }

  async function submit(source?: ProductionPreparedMember): Promise<void> {
    if (sending || targetCursor || draft.membership_sealed) return;
    sendController.abort();
    sendController = new AbortController();
    const signal = sendController.signal;
    sending = true;
    appendError = "";
    try {
      if (!pending) {
        if (!source) return;
        const member = await loadCurrentMemberAppend(session, draft, targetMembers, targetCursor,
          source, crypto.randomUUID(), signal);
        signal.throwIfAborted();
        pending = { member, operationID: crypto.randomUUID() };
      }
      const request = pending;
      const receipt = await appendProductionMember(session, draft.set_id, draft.revision, draft.etag,
        request.member, request.operationID, signal);
      signal.throwIfAborted();
      if (receipt.set_id !== draft.set_id || receipt.revision !== draft.revision ||
          receipt.operation_id !== request.operationID || receipt.etag <= draft.etag) {
        throw new Error("The append receipt disagreed with the selected draft.");
      }
      pending = null;
      onrefresh();
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof StaleMemberAppendError || cause instanceof APIError && cause.status === 409) onstale();
      else appendError = fail(cause);
    } finally {
      if (!signal.aborted) sending = false;
    }
  }
</script>

<section class="member-append" aria-label="Add prepared member">
  <strong>Add another source occurrence</strong>
  <p>Copy a prepared source from a retained set. The new occurrence starts unreviewed and can have its own decisions.</p>
  <SelectDropdown title="Prepared source set" value={sourceSetID} options={sourceOptions}
    disabled={sending || !!pending} onchange={value => void chooseSource(value)} />
  {#if targetCursor}<small>Load all target members before adding another occurrence.</small>{/if}
  {#if sourceLoading}<p role="status"><Spinner size={14} /> Loading exact source members…</p>{/if}
  {#if sourceError}<p class="error" role="alert">{sourceError}</p><Button size="sm" surface="soft" onclick={() => void chooseSource(sourceSetID)}>Refresh source</Button>{/if}
  {#if sourceSetID && !sourceLoading && !sourceError && sourceMembers.length === 0}
    <small>No prepared members in this set.</small>
  {/if}
  {#if sourceMembers.length > 0}
    <ol>
      {#each sourceMembers as member (member.id)}
        <li>
          <span>Member {member.ordinal} · source version <code>{member.source_version_id}</code></span>
          <Button size="sm" surface="soft" disabled={sending || !!pending || !!targetCursor}
            onclick={() => void submit(member)}>Add member {member.ordinal} as new occurrence</Button>
        </li>
      {/each}
    </ol>
  {/if}
  {#if sourceCursor}<Button size="sm" surface="soft" disabled={sourceLoading || sending || !!pending}
    onclick={() => void loadPage(sourceCursor)}>Load more source members</Button>{/if}
  {#if appendError}
    <p class="error" role="alert">{appendError}</p>
    <Button size="sm" surface="soft" disabled={sending} onclick={onrefresh}>Refresh draft</Button>
  {/if}
  {#if pending}<Button size="sm" disabled={sending} onclick={() => void submit()}>Retry append</Button>{/if}
</section>

<style>
  .member-append{display:grid;gap:var(--space-2);padding:var(--space-3);border:1px solid var(--border-muted);border-radius:var(--radius-md)}
  p{margin:0;display:flex;align-items:center;gap:var(--space-2);font-size:var(--font-size-sm);color:var(--text-secondary)}
  small{color:var(--text-muted)}
  ol{display:grid;gap:var(--space-2);margin:0;padding-left:var(--space-5)}
  li{display:grid;gap:var(--space-2);padding:var(--space-2);border:1px solid var(--border-muted);border-radius:var(--radius-md)}
  .error{color:var(--accent-red)}
</style>
