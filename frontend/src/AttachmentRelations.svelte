<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import {
    appendRelationPage,
    attachmentIdentity,
    readRelationPage,
    relationDisplayLimit,
    relationKey,
    type AttachmentIdentity,
    type RelationDirection,
    type RelationPage,
  } from "./attachments.js";
  import * as api from "./generated/docbank.js";
  import type { SelectedSource } from "./selectedSource.js";

  let {
    session,
    source,
    direction,
    total = $bindable(),
    busy = $bindable(false),
    onopen,
    onauthfailure,
  }: {
    session: string;
    source: SelectedSource;
    direction: RelationDirection;
    total?: number;
    busy?: boolean;
    onopen: (target: AttachmentIdentity) => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();
  let page = $state<RelationPage>();
  let loading = $state(false);
  let error = $state("");
  let names = $state<Record<number, string>>({});
  let controller: AbortController | undefined;
  let epoch = 0;
  const outgoing = $derived(direction === "outgoing");
  $effect(() => {
    total = page?.total;
    busy = loading;
  });

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
    page = undefined;
    error = "";
    names = {};
    loading = true;
    void readRelationPage(session, selected, direction, pending.signal)
      .then((next) => {
        if (request === epoch) {
          page = next;
          void loadNames(next, pending.signal);
        }
      })
      .catch((cause: unknown) => {
        if (request === epoch && !pending.signal.aborted) error = message(cause);
      })
      .finally(() => {
        if (request === epoch) loading = false;
      });
    return () => {
      ++epoch;
      pending.abort();
    };
  });
  async function loadMore(): Promise<void> {
    if (!page?.next || loading || !controller) return;
    const before = page,
      request = epoch,
      signal = controller.signal;
    loading = true;
    error = "";
    try {
      const next = await readRelationPage(
        session,
        attachmentIdentity(source),
        direction,
        signal,
        before.next,
      );
      if (request !== epoch) return;
      page = appendRelationPage(before, next);
      void loadNames(next, signal);
    } catch (cause) {
      if (request === epoch && !signal.aborted) error = message(cause);
    } finally {
      if (request === epoch) loading = false;
    }
  }
  async function loadNames(next: RelationPage, signal: AbortSignal): Promise<void> {
    if (outgoing) return;
    const ids = [...new Set(next.items.map((item) => item.relation.parent.node_id))];
    await Promise.all(
      ids.map(async (id) => {
        try {
          const node = await api.getNode(id, { session, signal });
          if (!signal.aborted) names[id] = node.name;
        } catch {
          /* The row keeps its fallback name. */
        }
      }),
    );
  }
</script>

{#if error || page?.items.length}
  <section aria-label={outgoing ? "Attachments" : "Attached to"}>
    <strong>{outgoing ? "Attachments" : "Attached to"}</strong>
    {#if error}<p class="problem" role="status">
        {outgoing ? "Attachment" : "Parent"} relations unavailable: {error}
      </p>{/if}
    {#if page}
      <ul>
        {#each page.items as item (relationKey(item.relation))}
          {@const relation = item.relation}
          {@const target = outgoing ? relation.child : relation.parent}
          {@const name = outgoing
            ? relation.filename || "Unnamed attachment"
            : (names[relation.parent.node_id] ?? "Document")}
          <li>
            {#if target}<button type="button" onclick={() => onopen(target)}>{name}</button
              >{:else}<span class="unavailable">{name}</span><span>{relation.outcome}</span>{/if}
          </li>
        {/each}
      </ul>
      {#if page.next}
        {#if page.items.length >= relationDisplayLimit}<p role="status">
            Showing the first {relationDisplayLimit}.
          </p>
        {:else}<Button size="sm" surface="soft" disabled={loading} onclick={() => void loadMore()}
            >Show more</Button
          >{/if}
      {/if}
    {/if}
  </section>
{/if}

<style>
  section {
    display: grid;
    gap: var(--space-2);
    min-width: 0;
    padding-block: var(--space-2);
  }
  p,
  span {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    overflow-wrap: anywhere;
  }
  ul {
    display: grid;
    padding: 0;
    margin: 0;
    list-style: none;
  }
  li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    min-width: 0;
    padding: var(--space-2);
    border-top: 1px solid var(--border-default);
    font-size: var(--font-size-sm);
  }
  button {
    min-width: 0;
    padding: 0;
    border: 0;
    background: none;
    color: var(--text-primary);
    font: inherit;
    text-align: left;
    overflow-wrap: anywhere;
    cursor: pointer;
  }
  button:hover {
    text-decoration: underline;
  }
  .unavailable {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .problem {
    color: var(--text-primary);
  }
</style>
