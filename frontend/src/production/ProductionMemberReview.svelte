<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Checkbox, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import { reviewProductionMember, type ProductionMember } from "./api.js";
  import { loadCurrentMemberReviewBinding, StaleMemberReviewError } from "./memberReview.js";

  let { session, setID, revision, etag, member, onrefresh, onstale, onauthfailure }: {
    session: string; setID: string; revision: number; etag: number; member: ProductionMember;
    onrefresh: () => void; onstale: () => void; onauthfailure: (cause: unknown) => void;
  } = $props();
  let confirmed = $state(false);
  let loading = $state(false);
  let error = $state("");
  let pending = $state<{ binding: string; operationID: string } | null>(null);
  let controller = new AbortController();

  onDestroy(() => controller.abort());

  async function declareReview(): Promise<void> {
    if (!confirmed || loading || member.reviewed) return;
    controller.abort();
    controller = new AbortController();
    const signal = controller.signal;
    loading = true;
    error = "";
    try {
      if (!pending) {
        const binding = await loadCurrentMemberReviewBinding(session, setID, revision, etag, member, signal);
        signal.throwIfAborted();
        pending = { binding, operationID: crypto.randomUUID() };
      }
      const request = pending;
      const receipt = await reviewProductionMember(session, setID, revision, etag, member.id,
        request.binding, request.operationID, signal);
      signal.throwIfAborted();
      if (receipt.set_id !== setID || receipt.revision !== revision ||
          receipt.operation_id !== request.operationID || receipt.etag <= etag) {
        throw new Error("The review receipt disagreed with the selected draft.");
      }
      pending = null;
      onrefresh();
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else if (cause instanceof StaleMemberReviewError || cause instanceof APIError && cause.status === 409) onstale();
      else error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!signal.aborted) loading = false;
    }
  }
</script>

<div class="member-review">
  <Checkbox checked={confirmed} onchange={checked => { confirmed = checked; }} disabled={loading || !!pending}
    label={`I reviewed the original and decisions for member ${member.ordinal}.`} />
  <small>This records review of the current draft. It does not approve production.</small>
  <Button size="sm" surface="soft" disabled={!confirmed || loading} onclick={() => void declareReview()}>
    {pending ? "Retry review declaration" : "Declare review complete"}
  </Button>
  {#if loading}<p role="status"><Spinner size={14} /> Checking exact review binding…</p>{/if}
  {#if error}<p class="error" role="alert">{error}</p>{/if}
</div>

<style>
  .member-review{display:grid;gap:var(--space-2);padding:var(--space-2);border:1px solid var(--border-muted);border-radius:var(--radius-md)}
  small{color:var(--text-muted)}
  p{margin:0;display:flex;align-items:center;gap:var(--space-2);font-size:var(--font-size-sm)}
  .error{color:var(--accent-red)}
</style>
