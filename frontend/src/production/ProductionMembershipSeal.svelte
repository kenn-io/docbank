<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Checkbox, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import { sealProductionMembership, type ProductionDraft, type ProductionMember } from "./api.js";
  import { prepareProductionMemberSeal, StaleMembershipSealError } from "./memberSeal.js";

  let { session, draft, members, nextCursor, onrefresh, onstale, onauthfailure }: {
    session: string; draft: ProductionDraft; members: ProductionMember[]; nextCursor: string;
    onrefresh: () => void; onstale: () => void; onauthfailure: (cause: unknown) => void;
  } = $props();
  let confirmed = $state(false);
  let loading = $state(false);
  let error = $state("");
  let pending = $state<{ total: number; memberHash: string; operationID: string } | null>(null);
  let controller = new AbortController();

  onDestroy(() => controller.abort());

  async function seal(): Promise<void> {
    if (!confirmed || loading || draft.membership_sealed || nextCursor || !members.length) return;
    controller.abort();
    controller = new AbortController();
    const signal = controller.signal;
    loading = true;
    error = "";
    try {
      if (!pending) {
        const evidence = await prepareProductionMemberSeal(session, draft, members, nextCursor, signal);
        signal.throwIfAborted();
        pending = { ...evidence, operationID: crypto.randomUUID() };
      }
      const request = pending;
      const receipt = await sealProductionMembership(session, draft.set_id, draft.revision, draft.etag,
        request.total, request.memberHash, request.operationID, signal);
      signal.throwIfAborted();
      if (receipt.set_id !== draft.set_id || receipt.revision !== draft.revision ||
          receipt.operation_id !== request.operationID || receipt.etag <= draft.etag) {
        throw new Error("The membership seal receipt disagreed with the selected draft.");
      }
      pending = null;
      onrefresh();
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else if (cause instanceof StaleMembershipSealError || cause instanceof APIError && cause.status === 409) onstale();
      else error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!signal.aborted) loading = false;
    }
  }
</script>

<div class="membership-seal">
  <strong>Seal member list</strong>
  <small>{nextCursor ? "Load all members before sealing." : `${members.length} exact members loaded for this draft.`}</small>
  <Checkbox checked={confirmed} onchange={checked => { confirmed = checked; }}
    disabled={loading || !!pending || !!nextCursor} label="I checked the full member list for this draft." />
  <Button size="sm" surface="soft" disabled={!confirmed || loading || !!nextCursor} onclick={() => void seal()}>
    {pending ? "Retry membership seal" : "Seal membership"}
  </Button>
  {#if loading}<p role="status"><Spinner size={14} /> Checking exact membership…</p>{/if}
  {#if error}<p class="error" role="alert">{error}</p>{/if}
</div>

<style>
  .membership-seal{display:grid;gap:var(--space-2);padding:var(--space-3);border:1px solid var(--border-muted);border-radius:var(--radius-md)}
  small{color:var(--text-muted)}
  p{margin:0;display:flex;align-items:center;gap:var(--space-2);font-size:var(--font-size-sm)}
  .error{color:var(--accent-red)}
</style>
