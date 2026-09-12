<script lang="ts">
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "./api.js";
  import { appendRelationPage, assertPublicationAgreement, attachmentIdentity, readPublication, readRelationPage,
    relationDisplayLimit, relationKey, type AttachmentIdentity, type AttachmentRelation, type PublicationReceipt,
    type RelationDirection, type RelationPage } from "./attachments.js";
  import type { SelectedSource } from "./selectedSource.js";

  let { session, source, direction, onopen, onauthfailure }: {
    session: string; source: SelectedSource; direction: RelationDirection;
    onopen: (target: AttachmentIdentity) => void; onauthfailure: (cause: unknown) => void;
  } = $props();
  let page = $state<RelationPage>();
  let loading = $state(false);
  let error = $state("");
  let receipt = $state<PublicationReceipt>();
  let receiptLoading = $state("");
  let receiptError = $state("");
  let controller: AbortController | undefined;
  let receiptController: AbortController | undefined;
  let epoch = 0;
  const outgoing = $derived(direction === "outgoing");
  const operations = $derived([...new Map((page?.items ?? []).map((item) => [item.relation.operation_id, item.relation])).values()]);

  function message(cause: unknown): string {
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
    return cause instanceof Error ? cause.message : String(cause);
  }
  $effect(() => {
    const selected = attachmentIdentity(source);
    const identity = `${session}:${source.key}:${direction}`;
    void identity;
    const request = ++epoch;
    const pending = new AbortController();
    controller = pending;
    page = undefined; error = ""; receipt = undefined; receiptError = ""; receiptLoading = ""; loading = true;
    void readRelationPage(session, selected, direction, pending.signal).then((next) => {
      if (request === epoch) page = next;
    }).catch((cause: unknown) => {
      if (request === epoch && !pending.signal.aborted) error = message(cause);
    }).finally(() => { if (request === epoch) loading = false; });
    return () => { ++epoch; pending.abort(); receiptController?.abort(); };
  });
  async function loadMore(): Promise<void> {
    if (!page?.next || loading || !controller) return;
    const before = page, request = epoch, signal = controller.signal;
    loading = true; error = "";
    try {
      const next = await readRelationPage(session, attachmentIdentity(source), direction, signal, before.next);
      if (request !== epoch) return;
      if (receipt) assertPublicationAgreement(receipt, next.items.map((item) => item.relation));
      page = appendRelationPage(before, next);
    } catch (cause) { if (request === epoch && !signal.aborted) error = message(cause); }
    finally { if (request === epoch) loading = false; }
  }
  async function checkInventory(sample: AttachmentRelation): Promise<void> {
    receiptController?.abort();
    const pending = new AbortController(); receiptController = pending;
    const request = epoch;
    receipt = undefined; receiptError = ""; receiptLoading = sample.operation_id;
    try {
      const next = await readPublication(session, sample, pending.signal);
      if (request !== epoch || pending.signal.aborted) return;
      assertPublicationAgreement(next, (page?.items ?? []).map((item) => item.relation));
      receipt = next;
    } catch (cause) { if (request === epoch && !pending.signal.aborted) receiptError = message(cause); }
    finally { if (request === epoch && !pending.signal.aborted) receiptLoading = ""; }
  }
</script>

<section aria-label={outgoing ? "Outgoing attachments" : "Incoming parents"}>
  <strong>{outgoing ? "Attachments" : "Parents"}</strong>
  {#if loading}<p role="status"><Spinner size={14} /> Loading {outgoing ? "attachments" : "parents"}…</p>{/if}
  {#if error}<p class="problem" role="status">{outgoing ? "Attachment" : "Parent"} relations unavailable: {error}</p>{/if}
  {#if page}
    <p>{page.items.length} of {page.total} observed occurrences. These are live pages, not a frozen family inventory.</p>
    {#if page.items.length === 0}<p>No published {outgoing ? "attachment" : "parent"} occurrences found. This does not establish a complete attachment inventory.</p>{/if}
    {#each operations as sample (sample.operation_id)}
      <div class="publication">
        <div class="publication-heading"><span>Publication {sample.operation_id}</span>
          <Button size="sm" surface="soft" disabled={Boolean(receiptLoading)} onclick={() => void checkInventory(sample)}
            ariaLabel={`Check inventory for ${sample.operation_id}`}>Check inventory</Button></div>
        {#if receipt?.operation_id === sample.operation_id}<p>Publication inventory: {receipt.inventory_state}</p>
        {:else}<p>Publication inventory: {receiptLoading === sample.operation_id ? "checking" : "not checked"}</p>{/if}
        <ul>
          {#each page.items.filter((item) => item.relation.operation_id === sample.operation_id) as item (relationKey(item.relation))}
            {@const relation = item.relation}
            {@const target = outgoing ? relation.child : relation.parent}
            <li>
              <strong>{relation.filename || "Unnamed attachment"}</strong>
              <span>Occurrence {relation.order} · MIME part {relation.part_path}</span>
              <span>MIME: {relation.outcome} · Processing: {item.state}{item.reason ? ` (${item.reason})` : ""}</span>
              <span class="identity">Parent version {relation.parent.version_id}</span>
              {#if relation.child}<span class="identity">Child version {relation.child.version_id}</span>{/if}
              {#if target}<Button size="sm" surface="soft" onclick={() => onopen(target)}
                ariaLabel={`Open ${outgoing ? "attachment" : "parent of"} ${relation.filename || "unnamed attachment"}, part ${relation.part_path}`}>
                Open {outgoing ? "attachment" : "parent"}
              </Button>{:else}<span>No child document was published for this occurrence.</span>{/if}
            </li>
          {/each}
        </ul>
      </div>
    {/each}
    {#if receiptError}<p class="problem" role="status">Publication inventory unavailable: {receiptError}</p>{/if}
    {#if page.next}
      {#if page.items.length >= relationDisplayLimit}<p role="status">Display limit reached. Additional occurrences are not shown; this view is incomplete.</p>
      {:else}<Button size="sm" surface="soft" disabled={loading} onclick={() => void loadMore()}>Load more {outgoing ? "attachments" : "parents"}</Button>{/if}
    {/if}
  {/if}
</section>

<style>
  section, .publication { display: grid; gap: var(--space-2); min-width: 0; }
  section { padding-block: var(--space-2); }
  p, span { margin: 0; color: var(--text-muted); font-size: var(--font-size-xs); overflow-wrap: anywhere; }
  .publication-heading { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); }
  ul { display: grid; gap: var(--space-2); padding: 0; margin: 0; list-style: none; }
  li { display: grid; justify-items: start; gap: var(--space-1); padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-md); font-size: var(--font-size-xs); }
  .identity { font-family: var(--font-mono); }
  .problem { color: var(--text-primary); }
</style>
