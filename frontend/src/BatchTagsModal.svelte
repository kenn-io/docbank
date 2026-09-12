<script lang="ts">
  import { onDestroy, untrack } from "svelte";
  import { Button, Checkbox, Modal, SelectDropdown, Spinner, type SelectDropdownOption } from "@kenn-io/kit-ui";
  import { APIError, type Tag } from "./api.js";
  import type { SelectionTarget } from "./selection.js";
  import { changeBatchTags, previewBatchTags, type BatchTagRequest, type BatchTagReceipt, type BatchTagPreview } from "./batch-tags.js";

  interface Props {
    session: string;
    targets: readonly SelectionTarget[];
    catalog: Tag[];
    catalogTotal: number;
    disabled: boolean;
    onclose: () => void;
    onchanged: (receipt: BatchTagReceipt) => void;
    onauthfailure: (cause: unknown) => void;
  }
  let { session, targets, catalog, catalogTotal, disabled, onclose, onchanged, onauthfailure }: Props = $props();
  let currentTargets = $state(untrack(() => targets.map((target) => ({ ...target }))));
  let tagID = $state("");
  let preview = $state<BatchTagPreview>();
  let uncertain = $state<BatchTagRequest>();
  let loading = $state(false);
  let pending = $state(false);
  let stale = $state(false);
  let closingUncertain = $state(false);
  let failure = $state("");
  let notice = $state("");
  let generation = 0;
  let alive = true;
  onDestroy(() => { alive = false; generation++; });

  const options = $derived<SelectDropdownOption[]>([
    { value: "", label: "Choose a tag…" },
    ...catalog.map((tag) => ({ value: tag.id, label: tag.name })),
  ]);
  const assignedCount = $derived(preview?.nodes.filter((node) => node.assigned).length ?? 0);
  const busy = $derived(disabled || pending || loading);
  const canChange = $derived(!busy && !stale && !uncertain && preview !== undefined);

  function close() {
    if (pending || loading) return;
    if (uncertain) { closingUncertain = true; return; }
    onclose();
  }

  async function loadPreview() {
    const requestGeneration = ++generation;
    preview = undefined;
    failure = "";
    if (!tagID) return;
    loading = true;
    try {
      const result = await previewBatchTags(session, tagID, currentTargets);
      if (alive && requestGeneration === generation) preview = result;
    } catch (cause) {
      if (!alive || requestGeneration !== generation) return;
      if (cause instanceof APIError && cause.status === 401) { onauthfailure(cause); return; }
      stale = cause instanceof APIError && (cause.code === "stale_revision" || cause.code === "not_found");
      failure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (alive && requestGeneration === generation) loading = false;
    }
  }

  function selectTag(value: string) {
    if (busy || stale || uncertain) return;
    tagID = value;
    notice = "";
    void loadPreview();
  }

  async function submit(request: BatchTagRequest) {
    pending = true;
    failure = "";
    closingUncertain = false;
    try {
      const receipt = await changeBatchTags(session, request);
      if (!alive) return;
      uncertain = undefined;
      currentTargets = receipt.nodes.map((node) => ({ node_id: node.node_id, revision: node.revision }));
      notice = `Operation confirmed: ${receipt.nodes.filter((node) => node.changed).length} changed, ${receipt.nodes.filter((node) => !node.changed).length} already in the requested state.`;
      onchanged(receipt);
      await loadPreview();
    } catch (cause) {
      if (!alive) return;
      if (cause instanceof APIError && cause.status >= 400 && cause.status < 500) {
        uncertain = undefined;
        stale = cause.code === "stale_revision" || cause.code === "not_found";
        if (cause.status === 401) { onauthfailure(cause); return; }
      } else {
        uncertain = request;
      }
      failure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (alive) pending = false;
    }
  }

  function change(assign: boolean) {
    if (!canChange) return;
    try {
      const request = { operation_id: crypto.randomUUID(), tag_id: tagID, assign,
        nodes: currentTargets.map((target) => ({ ...target })) };
      void submit(request);
    } catch (cause) {
      failure = cause instanceof Error ? cause.message : String(cause);
    }
  }
</script>

<Modal title="Tag selected documents" tone="info" width="620px" maxWidth="min(620px, calc(100vw - 32px))"
  ariaLabel="Tag selected documents" onclose={close} closeOnOverlayClick={!pending && !loading}>
  <div class="batch-tags">
    <p>{currentTargets.length} selected document{currentTargets.length === 1 ? "" : "s"}. Each operation changes one tag across this exact selection, or changes nothing if a document is stale.</p>
    <SelectDropdown value={tagID} {options} title="Tag for selected documents"
      disabled={busy || stale || uncertain !== undefined} onchange={selectTag} />
    {#if catalogTotal > catalog.length}
      <p class="hint">Showing {catalog.length} of {catalogTotal} tag definitions. Definitions outside this catalog page remain available through the API.</p>
    {/if}
    {#if loading}<div class="loading"><Spinner size={16} /> Checking selected documents…</div>{/if}
    {#if preview}
      <Checkbox checked={assignedCount === currentTargets.length}
        indeterminate={assignedCount > 0 && assignedCount < currentTargets.length}
        disabled ariaLabel="Selected tag membership" label={`${assignedCount} of ${currentTargets.length} selected documents have this tag.`} />
    {/if}
    <div class="actions">
      <Button tone="info" disabled={!canChange || assignedCount === currentTargets.length} onclick={() => change(true)}>Add to all</Button>
      <Button disabled={!canChange || assignedCount === 0} onclick={() => change(false)}>Remove from all</Button>
    </div>
    {#if pending}<div class="loading"><Spinner size={16} /> Confirming operation…</div>{/if}
    {#if notice}<p role="status">{notice}</p>{/if}
    {#if failure}<p role="alert">{failure}</p>{/if}
    {#if stale}<p>Close and refresh your selection before starting another operation. Expected revisions have not been replaced automatically.</p>{/if}
    {#if uncertain}
      <p>The result is uncertain. Retry uses the same operation identity and original revisions; it cannot apply the change twice.</p>
      <Button disabled={busy} onclick={() => uncertain && void submit(uncertain)}>Retry same operation</Button>
    {/if}
    {#if closingUncertain}
      <p role="alert">Closing loses this browser’s retry request without confirming the result. Refresh the documents before starting new work.</p>
      <Button disabled={busy} onclick={onclose}>Close without confirmation</Button>
    {/if}
    <p class="hint">No query-wide selection or automatic conflict retry. A confirmed receipt describes the original operation; current membership is checked again afterward.</p>
  </div>
  {#snippet footer()}<Button surface="soft" disabled={pending || loading} onclick={close}>Done</Button>{/snippet}
</Modal>

<style>
  .batch-tags { display: grid; gap: var(--space-4); }
  .batch-tags p { margin: 0; }
  .actions, .loading { display: flex; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
  .hint { color: var(--text-muted); font-size: var(--text-sm); }
</style>
