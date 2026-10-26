<script lang="ts">
  import { provenanceSourceLabel } from "./provenanceLabel.js";
  import { untrack } from "svelte";
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import * as api from "./generated/docbank.js";
  import { APIError } from "./api-transport.js";
  import AppTooltip from "./AppTooltip.svelte";
  import CopyValue from "./CopyValue.svelte";

  let {
    session,
    node,
    onversions,
    onprovenance,
    onauthfailure,
  }: {
    session: string;
    node: api.Node;
    onversions?: () => void;
    onprovenance: () => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();
  let versions = $state<api.ContentVersion[]>([]);
  let imports = $state<api.ProvenanceFact[]>([]);
  let loading = $state(false);
  let error = $state("");
  let retry = $state(0);
  const entries = $derived(
    [
      ...versions
        .filter((version) => version.transition_kind !== "content_create" || !imports.length)
        .map((version) => ({
          id: `version:${version.id}`,
          date: version.recorded_at,
          label:
            version.transition_kind === "content_revert"
              ? "Reverted version"
              : version.transition_kind === "content_create"
                ? "Created"
                : "New version",
          path: "",
        })),
      ...imports.map((fact) => ({
        id: `import:${fact.identity}`,
        date: fact.ingest_started_at,
        label: provenanceSourceLabel(fact.source_kind),
        path: fact.original_path,
      })),
    ]
      .sort((a, b) => Date.parse(b.date) - Date.parse(a.date))
      .slice(0, 5),
  );

  $effect(() => {
    const id = node.id;
    void node.revision;
    void retry;
    const token = session;
    const controller = new AbortController();
    versions = [];
    imports = [];
    loading = true;
    error = "";
    const options = { session: token, signal: controller.signal };
    void Promise.allSettled([
      api.listContentVersions(id, { limit: 5, offset: 0 }, options),
      api.listNodeProvenance(id, { limit: 5, offset: 0 }, options),
    ]).then(([history, provenance]) => {
      if (controller.signal.aborted) return;
      if (history.status === "fulfilled") versions = history.value.items;
      if (provenance.status === "fulfilled") imports = provenance.value.items;
      for (const result of [history, provenance]) {
        if (result.status !== "rejected") continue;
        if (result.reason instanceof APIError && result.reason.status === 401)
          untrack(() => onauthfailure(result.reason));
        else error = "Some history couldn't be loaded.";
      }
      loading = false;
    });
    return () => controller.abort();
  });
</script>

<section class="history" aria-label="History">
  <header>
    <h3>History</h3>
    {#if onversions}<Button size="sm" surface="soft" onclick={onversions}>All versions</Button>{/if}
  </header>
  {#if loading}<span role="status"><Spinner size={13} /> Loading history…</span>
  {:else}
    {#if error}<div class="history-error" role="status">
        {error}<Button size="sm" surface="soft" onclick={() => retry++}>Retry history</Button>
      </div>{/if}
    <ol>
      {#each entries as entry (entry.id)}
        <li>
          <div class="entry-heading">
            <AppTooltip text={new Date(entry.date).toLocaleString()}
              ><time datetime={entry.date}
                >{new Date(entry.date).toLocaleDateString(undefined, {
                  month: "short",
                  day: "numeric",
                })}</time
              ></AppTooltip
            >
            <span aria-hidden="true">·</span>
            <span>{entry.label}</span>
          </div>
          {#if entry.path}<CopyValue value={entry.path} label="source path" />{/if}
        </li>
      {:else}{#if !error}<li class="muted">No history recorded</li>{/if}{/each}
    </ol>
    {#if imports.length}<div>
        <Button size="sm" surface="soft" onclick={onprovenance}>All imports</Button>
      </div>{/if}
  {/if}
</section>

<style>
  .history {
    display: grid;
    gap: var(--space-5);
    border-top: 1px solid var(--border-muted);
    padding-top: var(--space-6);
    min-width: 0;
    font-size: var(--font-size-sm);
  }
  header,
  .entry-heading,
  .history-error {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  header {
    justify-content: space-between;
  }
  h3 {
    margin: 0;
    font-size: inherit;
    font-weight: 500;
  }
  ol {
    display: grid;
    gap: var(--space-5);
    list-style: none;
    padding: 0;
    margin: 0;
  }
  li {
    display: grid;
    gap: var(--space-3);
    min-width: 0;
  }
  time,
  .muted {
    color: var(--text-muted);
  }
</style>
