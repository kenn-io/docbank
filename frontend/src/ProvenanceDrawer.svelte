<script lang="ts">
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import { Chip, DetailDrawer, IconButton, Spinner } from "@kenn-io/kit-ui";
  import CopyValue from "./CopyValue.svelte";
  import { APIError } from "./api-transport.js";
  import { type Node, type ProvenanceFact, type ProvenancePage } from "./generated/docbank.js";
  import { basename, formatDate } from "./format.js";

  let {
    session,
    node,
    path,
    onclose,
    onauthfailure,
  }: {
    session: string;
    node: Node;
    path: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();

  let page = $state<ProvenancePage | null>(null);
  let loading = $state(true);
  let error = $state("");
  let generation = 0;
  const currentPath = $derived(page ? page.node.path : path);
  const documentName = $derived(
    currentPath ? basename(currentPath) : `${page?.node.name ?? node.name} (trashed)`,
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
      const next = await generated.listNodeProvenance(
        node.id,
        { limit: 1000, offset: 0 },
        { session },
      );
      if (request === generation) page = next;
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

  function sourceReferences(fact: ProvenanceFact): string[] {
    let values: unknown;
    try {
      values = JSON.parse(fact.source_description);
    } catch {
      values = fact.source_description;
    }
    return (Array.isArray(values) ? values : [values]).filter(
      (value): value is string => typeof value === "string" && value !== fact.original_path,
    );
  }

  function sourceLabel(fact: ProvenanceFact): string {
    switch (fact.source_kind) {
      case "cli":
        return "Added from command line";
      case "upload":
      case "web":
        return "Uploaded in browser";
      case "mail":
      case "mailbox":
      case "msgvault":
        return "Imported from mailbox";
      case "watch":
        return "Added from watched folder";
      case "filesystem":
        return "Added from folder";
      case "photo-import":
        return "Imported photos";
      default:
        return "Imported document";
    }
  }
</script>

<DetailDrawer width="min(520px, 100vw)" ariaLabel={`Provenance for ${documentName}`} {onclose}>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <h2>Document provenance</h2>
        <strong>{documentName}</strong>
      </div>
      <IconButton size="sm" ariaLabel="Close provenance" onclick={onclose}
        ><XIcon size="14" aria-hidden="true" /></IconButton
      >
    </div>
  {/snippet}
  <div class="provenance-history">
    {#if currentPath}<CopyValue value={currentPath} label="document path" />{/if}
    {#if loading}
      <div class="loading" role="status"><Spinner size={16} /> Loading provenance…</div>
    {:else if error}
      <p class="error" role="alert">{error}</p>
    {:else if !page?.items.length}
      <p class="empty">No provenance recorded</p>
    {:else}
      <ol aria-label="Import history">
        {#each page.items as fact (fact.identity)}
          <li>
            <div class="import-heading">
              <strong>{sourceLabel(fact)}</strong>
              {#if !fact.active}<Chip size="xs" tone="muted">Superseded</Chip>{/if}
            </div>
            <CopyValue value={fact.original_path} label="source path" />
            <time datetime={fact.ingest_started_at} title={fact.ingest_started_at}
              >{formatDate(fact.ingest_started_at)}</time
            >
            <details>
              <summary>Import details</summary>
              <dl>
                <div>
                  <dt>Import ID</dt>
                  <dd><CopyValue value={fact.ingest_id} label="import ID" /></dd>
                </div>
                <div>
                  <dt>Record hash</dt>
                  <dd><CopyValue value={fact.identity} label="record hash" /></dd>
                </div>
                <div>
                  <dt>Document ID</dt>
                  <dd><CopyValue value={String(fact.node_id)} label="document ID" /></dd>
                </div>
                <div>
                  <dt>Source type</dt>
                  <dd>{fact.source_kind}</dd>
                </div>
                {#each sourceReferences(fact) as reference}<div>
                    <dt>Source</dt>
                    <dd><CopyValue value={reference} label="source reference" /></dd>
                  </div>{/each}
                {#if fact.original_mtime}<div>
                    <dt>Source modified</dt>
                    <dd>
                      <time datetime={fact.original_mtime} title={fact.original_mtime}
                        >{formatDate(fact.original_mtime)}</time
                      >
                    </dd>
                  </div>{/if}
                {#if fact.supersedes}<div>
                    <dt>Replaces</dt>
                    <dd><CopyValue value={fact.supersedes} label="replaced record hash" /></dd>
                  </div>{/if}
              </dl>
            </details>
          </li>
        {/each}
      </ol>
      {#if page.total > page.items.length}<p class="limit-note">
          Showing the newest {page.items.length} of {page.total} imports. Older history is available through
          the CLI or API.
        </p>{/if}
    {/if}
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
  .drawer-heading > div {
    min-width: 0;
  }
  h2 {
    margin: 0 0 var(--space-1);
    font-size: var(--font-size-lg);
  }
  .drawer-heading strong {
    font-size: var(--font-size-sm);
    font-weight: 400;
    color: var(--text-muted);
    overflow-wrap: anywhere;
  }
  .provenance-history {
    min-width: 0;
    padding: var(--space-5);
  }
  ol {
    list-style: none;
    padding: 0;
    margin: var(--space-4) 0 0;
  }
  li {
    display: grid;
    gap: var(--space-2);
    min-width: 0;
    padding: var(--space-4) 0;
    border-top: 1px solid var(--border-default);
  }
  .import-heading {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
    font-size: var(--font-size-sm);
  }
  time,
  summary,
  .empty,
  .limit-note {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  summary {
    cursor: pointer;
  }
  details {
    margin-top: var(--space-2);
    min-width: 0;
  }
  dl {
    display: grid;
    gap: var(--space-3);
    margin: var(--space-3) 0 0;
  }
  dl > div {
    display: grid;
    grid-template-columns: 100px minmax(0, 1fr);
    align-items: start;
    gap: var(--space-3);
  }
  dt {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  dd {
    min-width: 0;
    margin: 0;
    overflow-wrap: anywhere;
    font-size: var(--font-size-sm);
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
    overflow-wrap: anywhere;
  }
</style>
