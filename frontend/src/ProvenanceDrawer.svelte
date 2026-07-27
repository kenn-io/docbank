<script lang="ts">
  import { onMount } from "svelte";
  import MapPinIcon from "@lucide/svelte/icons/map-pin";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Card,
    Chip,
    CopyButton,
    DetailDrawer,
    EmptyState,
    IconButton,
    Spinner,
    Timeline,
    TimelineItem,
  } from "@kenn-io/kit-ui";
  import {
    APIError,
    provenance,
    type Node,
    type ProvenanceFact,
    type ProvenancePage,
  } from "./api.js";
  import { basename, formatDate } from "./format.js";

  interface Props {
    session: string;
    node: Node;
    path: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, node, path, onclose, onauthfailure }: Props = $props();

  let page = $state<ProvenancePage | null>(null);
  let selectedIdentity = $state("");
  let loading = $state(true);
  let error = $state("");
  let generation = 0;

  const selectedFact = $derived(
    page?.items.find((fact) => fact.identity === selectedIdentity),
  );
  const authorityNode = $derived(page?.node ?? node);
  const authorityPath = $derived(page ? page.node.path : path);
  const authorityLabel = $derived(
    authorityPath
      ? basename(authorityPath)
      : `${authorityNode.name} (trashed)`,
  );
  const authorityCoordinate = $derived(
    authorityPath ?? `Trashed node · id:${authorityNode.id}`,
  );

  onMount(() => {
    void load();
    return () => {
      generation += 1;
    };
  });

  async function load(): Promise<void> {
    const request = ++generation;
    loading = true;
    error = "";
    try {
      const next = await provenance(session, node.id);
      if (request !== generation) return;
      page = next;
      selectedIdentity = next.items[0]?.identity ?? "";
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        onclose();
        return;
      }
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  function factStatus(fact: ProvenanceFact): string {
    return fact.active ? "Active origin" : "Superseded origin";
  }
</script>

<DetailDrawer
  width="min(940px, 100vw)"
  ariaLabel={`Immutable provenance for ${authorityCoordinate}`}
  {onclose}
>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>DOCUMENT PROVENANCE</span>
        <strong>{authorityLabel}</strong>
        <code>{authorityCoordinate}</code>
      </div>
      <IconButton size="sm" ariaLabel="Close provenance" onclick={onclose}>
        <XIcon size="14" aria-hidden="true" />
      </IconButton>
    </div>
  {/snippet}

  <div class="provenance-shell">
    <section class="fact-list" aria-label="Immutable origin facts">
      <div class="section-heading">
        <span>NEWEST INGEST FIRST</span>
        <strong>{page?.total ?? 0} origin fact{page?.total === 1 ? "" : "s"}</strong>
      </div>

      {#if loading}
        <div class="loading"><Spinner size={16} /> Loading provenance…</div>
      {:else if error}
        <p class="error" role="alert">{error}</p>
      {:else if !page || page.items.length === 0}
        <EmptyState
          title="No provenance recorded"
          description="This document has no retained ingest-origin facts."
        >
          {#snippet icon()}<MapPinIcon size="22" />{/snippet}
        </EmptyState>
      {:else}
        <Timeline ariaLabel="Document provenance history">
          {#each page.items as fact (fact.identity)}
            <TimelineItem tone={fact.active ? "success" : "neutral"}>
              <Card
                level="default"
                padding="sm"
                selected={fact.identity === selectedIdentity}
                onclick={() => (selectedIdentity = fact.identity)}
                ariaLabel={`${factStatus(fact)} from ${fact.source_description}`}
                eyebrow={fact.source_kind}
                meta={formatDate(fact.ingest_started_at)}
              >
                <div class="fact-summary">
                  <strong>{fact.source_description}</strong>
                  <Chip size="xs" tone={fact.active ? "success" : "muted"} dot={fact.active}>
                    {fact.active ? "Active" : "Superseded"}
                  </Chip>
                </div>
                <p>{fact.original_path}</p>
              </Card>
            </TimelineItem>
          {/each}
        </Timeline>
        {#if page.total > page.items.length}
          <p class="limit-note">
            Showing the newest {page.items.length} of {page.total} origin facts.
            Use the CLI or HTTP API for older history.
          </p>
        {/if}
      {/if}
    </section>

    <section class="fact-detail" aria-label="Complete provenance authority">
      {#if selectedFact}
        <Card
          level="inset"
          eyebrow={selectedFact.source_kind}
          eyebrowTone={selectedFact.active ? "success" : "neutral"}
          title="Complete origin authority"
          meta={selectedFact.active ? "Active fact" : "Superseded fact"}
        >
          <dl>
            <div class="identity">
              <dt>Fact</dt>
              <dd>
                <code>{selectedFact.identity}</code>
                <CopyButton text={selectedFact.identity} ariaLabel="Copy provenance identity" />
              </dd>
            </div>
            <div><dt>Status</dt><dd>{factStatus(selectedFact)}</dd></div>
            <div><dt>Node</dt><dd>id:{selectedFact.node_id}</dd></div>
            <div class="identity">
              <dt>Ingest</dt>
              <dd>
                <code>{selectedFact.ingest_id}</code>
                <CopyButton text={selectedFact.ingest_id} ariaLabel="Copy ingest ID" />
              </dd>
            </div>
            <div><dt>Ingest began</dt><dd>{formatDate(selectedFact.ingest_started_at)}</dd></div>
            <div><dt>Canonical time</dt><dd><code>{selectedFact.ingest_started_at}</code></dd></div>
            <div><dt>Source kind</dt><dd>{selectedFact.source_kind}</dd></div>
            <div><dt>Source</dt><dd>{selectedFact.source_description}</dd></div>
            <div><dt>Original reference</dt><dd>{selectedFact.original_path}</dd></div>
            <div>
              <dt>Source modified</dt>
              <dd>{selectedFact.original_mtime
                ? formatDate(selectedFact.original_mtime)
                : "Not recorded"}</dd>
            </div>
            {#if selectedFact.original_mtime}
              <div><dt>Canonical mtime</dt><dd><code>{selectedFact.original_mtime}</code></dd></div>
            {/if}
            {#if selectedFact.supersedes}
              <div class="identity">
                <dt>Supersedes</dt>
                <dd>
                  <code>{selectedFact.supersedes}</code>
                  <CopyButton text={selectedFact.supersedes} ariaLabel="Copy superseded provenance identity" />
                </dd>
              </div>
            {/if}
          </dl>
          <p class="explanation">
            Provenance records where Docbank was told this document came from.
            It does not open, manage, or retain the original source.
          </p>
        </Card>
      {:else}
        <EmptyState
          title="Select an origin fact"
          description="Choose an entry to inspect its stable identity, source reference, and ingest authority."
        >
          {#snippet icon()}<MapPinIcon size="22" />{/snippet}
        </EmptyState>
      {/if}
    </section>
  </div>
</DetailDrawer>

<style>
  .drawer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }

  .drawer-heading > div,
  .section-heading {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .drawer-heading span,
  .section-heading span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-bold);
    letter-spacing: var(--letter-spacing-label, 0.04em);
  }

  .drawer-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-lg);
  }

  .drawer-heading code {
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .provenance-shell {
    display: grid;
    grid-template-columns: minmax(300px, 0.85fr) minmax(360px, 1.15fr);
    min-height: 100%;
  }

  .fact-list,
  .fact-detail {
    min-width: 0;
    padding: var(--space-5);
  }

  .fact-list {
    border-right: 1px solid var(--border-default);
    background: var(--bg-primary);
  }

  .section-heading {
    margin-bottom: var(--space-5);
  }

  .section-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }

  .fact-summary {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .fact-summary strong {
    min-width: 0;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .fact-summary + p {
    margin: 0;
    overflow-wrap: anywhere;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .loading {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .error {
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }

  .limit-note,
  .explanation {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.45;
  }

  .explanation {
    margin: var(--space-5) 0 0;
    padding-top: var(--space-4);
    border-top: 1px solid var(--border-default);
  }

  dl {
    display: grid;
    gap: var(--space-4);
    margin: 0;
  }

  dl > div {
    display: grid;
    grid-template-columns: 118px minmax(0, 1fr);
    gap: var(--space-4);
  }

  dt {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  dd {
    min-width: 0;
    margin: 0;
    overflow-wrap: anywhere;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .identity dd {
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
  }

  code {
    min-width: 0;
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-size: var(--font-size-xs);
  }

  @media (max-width: 760px) {
    .provenance-shell {
      grid-template-columns: 1fr;
    }

    .fact-list {
      border-right: 0;
      border-bottom: 1px solid var(--border-default);
    }
  }
</style>
