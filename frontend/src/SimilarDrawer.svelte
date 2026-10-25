<script lang="ts">
  import { reportAction } from "./action-events.js";
  import * as generated from "./generated/docbank.js";
  import { onMount } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import { Chip, DetailDrawer, IconButton, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { documentSimilar } from "./receipts.js";
  import { processingSetupURL } from "./processing-summary.js";
  import type {
    DocumentSimilarReport,
    Node,
    ProcessingPlan,
    ProcessingProfileSummary,
  } from "./generated/docbank.js";

  interface Props {
    session: string;
    node: Node;
    path: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
    scopeVersionIDs?: string[];
  }

  let { session, node, path, onclose, onauthfailure, scopeVersionIDs = [] }: Props = $props();

  const maxScope = 4096;
  let profiles = $state<ProcessingProfileSummary[]>([]);
  let profileName = $state("");
  let report = $state<DocumentSimilarReport | null>(null);
  let loading = $state(true);
  let busy = $state(false);
  let error = $state("");
  let generation = 0;

  onMount(() => {
    void load();
    return () => {
      generation += 1;
    };
  });

  function close(): void {
    generation += 1;
    onclose();
  }

  function fail(request: number, cause: unknown): void {
    if (request !== generation) return;
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      onclose();
      return;
    }
    error = cause instanceof Error ? cause.message : String(cause);
  }

  async function load(): Promise<void> {
    const request = ++generation;
    loading = true;
    error = "";
    try {
      const all = await generated.listDocumentProcessingProfiles({ session });
      if (request !== generation) return;
      profiles = all.filter((profile) => profile.embedding_bindings.length > 0);
      profileName = profiles[0]?.name ?? "";
      if (profileName) await findSimilar(request);
    } catch (cause) {
      fail(request, cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  async function findSimilar(existing?: number): Promise<void> {
    const request = existing ?? ++generation;
    const binding = profiles.find((item) => item.name === profileName)?.embedding_bindings[0];
    report = null;
    error = "";
    if (!binding || !node.current_version_id) return;
    if (scopeVersionIDs.length === 0 || scopeVersionIDs.length > maxScope) return;
    busy = true;
    try {
      const plan: ProcessingPlan = await generated.planDocumentProcessing(
        {
          selector: {
            node_id: node.id,
            content_version_id: node.current_version_id,
            profile: profileName,
          },
        },
        { session },
      );
      if (request !== generation) return;
      const next = await documentSimilar(session, {
        selector: { ...plan.selector },
        binding_id: binding,
        limit: 20,
        fence: { vault_uid: plan.vault_uid, content_version_ids: [...scopeVersionIDs] },
      });
      if (request !== generation) return;
      report = next;
      if (next.state !== "unavailable") reportAction(session, "search_run", "semantic");
    } catch (cause) {
      fail(request, cause);
    } finally {
      if (request === generation) busy = false;
    }
  }

  function chooseProfile(name: string): void {
    if (name === profileName) return;
    profileName = name;
    void findSimilar();
  }
</script>

<DetailDrawer width="min(560px, 100vw)" ariaLabel="Similar documents" onclose={close}>
  {#snippet header()}
    <div class="heading">
      <div>
        <strong>Similar documents</strong>
        <small title={path}>{path}</small>
      </div>
      <IconButton size="sm" ariaLabel="Close similar documents" onclick={close}>
        <XIcon size="14" aria-hidden="true" />
      </IconButton>
    </div>
  {/snippet}

  <section class="similar" aria-label="Similar documents">
    {#if loading && profiles.length === 0}
      <p class="muted"><Spinner size={14} /> Loading…</p>
    {:else if error}
      <p class="error" role="alert">{error}</p>
    {:else if profiles.length === 0}
      <p>
        No profile stores search vectors. <a
          href={processingSetupURL}
          target="_blank"
          rel="noreferrer">Set one up</a
        >
      </p>
    {:else}
      {#if profiles.length > 1}
        <SelectDropdown
          title="Profile"
          value={profileName}
          disabled={busy}
          options={profiles.map((profile) => ({ value: profile.name, label: profile.name }))}
          onchange={chooseProfile}
        />
      {/if}
      {#if scopeVersionIDs.length > maxScope}
        <p class="warning">Narrow this view to at most 4,096 files.</p>
      {:else if busy}
        <p class="muted"><Spinner size={14} /> Finding similar files…</p>
      {:else if report?.state === "unavailable"}
        <p>This file has no search vectors yet. Process it first.</p>
      {:else if report}
        {#if report.coverage.complete_documents < report.coverage.scoped_documents}
          <p class="muted">
            {report.coverage.complete_documents} of {report.coverage.scoped_documents} files in this view
            are indexed.
          </p>
        {/if}
        {#if report.results.length === 0}
          <p>No similar documents in this view.</p>
        {:else}
          <ol class="results">
            {#each report.results as result (result.rank)}
              <li>
                <span class="path" title={result.path}>{result.path}</span>
                {#if result.duplicate_count > 0}<Chip size="xs"
                    >+{result.duplicate_count} identical</Chip
                  >{/if}
                <span class="score">{result.score.toFixed(2)}</span>
              </li>
            {/each}
          </ol>
          {#if report.truncated}<p class="muted">Showing the first 20 groups.</p>{/if}
        {/if}
      {/if}
    {/if}
  </section>
</DetailDrawer>

<style>
  .heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    width: 100%;
  }
  .heading > div {
    display: grid;
    gap: var(--space-1);
    min-width: 0;
  }
  .heading strong {
    font-size: var(--font-size-lg);
  }
  .heading small {
    overflow: hidden;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .similar {
    display: grid;
    gap: var(--space-4);
    padding: var(--space-5);
  }
  p {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .muted {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--text-muted);
  }
  .error {
    color: var(--accent-red);
  }
  .warning {
    color: var(--accent-amber);
  }
  .results {
    display: grid;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .results li {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) 0;
    border-bottom: 1px solid var(--border-muted);
    font-size: var(--font-size-sm);
  }
  .path {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .score {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }
</style>
