<script lang="ts">
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "./api.js";
  import { readDuplicateContext } from "./duplicates.js";
  import type { LiveSelectedSource, SelectedSource } from "./selectedSource.js";

  let { session, source, onopen, onauthfailure }: {
    session: string; source: SelectedSource; onopen: (source: LiveSelectedSource) => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();
  let references = $state<LiveSelectedSource[]>([]);
  let total = $state(0);
  let truncated = $state(false);
  let loading = $state(false);
  let error = $state("");

  $effect(() => {
    const key = `${session}:${source.blobHash}:${source.size}`;
    const controller = new AbortController();
    let current = true;
    void key;
    references = [];
    total = 0;
    truncated = false;
    error = "";
    loading = true;
    void readDuplicateContext(session, source, controller.signal).then((group) => {
      if (!current) return;
      references = group.references;
      total = group.referenceCount;
      truncated = group.truncated;
    }).catch((cause: unknown) => {
      if (!current || (cause instanceof DOMException && cause.name === "AbortError")) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else if (cause instanceof APIError && cause.status === 404) error = "No current duplicate group exists for this exact content.";
      else error = cause instanceof Error ? cause.message : String(cause);
    }).finally(() => { if (current) loading = false; });
    return () => { current = false; controller.abort(); references = []; error = ""; };
  });
</script>

<div class="duplicates">
  <div><strong>Current exact-content duplicates</strong>
    {#if total}<span>{total} live references{truncated ? " · first 16 shown" : ""}</span>{/if}
  </div>
  {#if loading}<p role="status"><Spinner size={14} /> Loading exact duplicate group…</p>
  {:else if error}<p role="status">{error}</p>
  {:else}<ul>
    {#each references as reference (reference.nodeID)}
      <li class:active={reference.nodeID === source.nodeID && reference.versionID === source.versionID}>
        <div><strong>{reference.name}</strong><span>{reference.path}</span></div>
        {#if reference.nodeID === source.nodeID && reference.versionID === source.versionID}
          <span>Open</span>
        {:else}<Button size="sm" surface="soft" onclick={() => onopen(reference)}>Open separately</Button>{/if}
      </li>
    {/each}
  </ul>{/if}
</div>

<style>
  .duplicates { display: grid; gap: var(--space-3); color: var(--text-primary); font-size: var(--font-size-xs); }
  .duplicates > div, .duplicates p { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); margin: 0; }
  .duplicates span, .duplicates p { color: var(--text-muted); }
  ul { display: grid; gap: var(--space-2); margin: 0; padding: 0; list-style: none; }
  li { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  li > div { display: grid; min-width: 0; }
  li > div span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  li.active { background: var(--bg-inset); }
</style>
