<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { Button, DetailDrawer, Spinner, TextInput } from "@kenn-io/kit-ui";
  import { APIError } from "./api.js";
  import { canonicalQuery, parseHighlightSet, parseQuery, type HighlightTerm, type Query } from "./query.js";
  import { createSavedQuery, deleteSavedQuery, listSavedQueries, SavedQueryReceiptError, updateSavedQuery, type Definition, type SavedQuery, type SavedQueryKind } from "./savedQueries.js";

  let { session, initialQuery, onload, onclose, onauthfailure, onopenquery }: {
    session: string;
    initialQuery: Query;
    onload: (query: Query) => void;
    onopenquery?: (query: Query) => void;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();

  let items = $state<SavedQuery[]>([]);
  let total = $state(0);
  let offset = $state(0);
  let loading = $state(true);
  let pending = $state(false);
  let failure = $state("");
  let receiptFailed = $state(false);
  let notice = $state("");
  let selected = $state<SavedQuery | null>(null);
  let deleting = $state<SavedQuery | null>(null);
  let kind = $state<SavedQueryKind>("query");
  let name = $state("");
  let description = $state("");
  let rawQuery = $state(untrack(() => JSON.stringify(initialQuery, null, 2)));
  let terms = $state<HighlightTerm[]>([{ text: "", color: "#ffcc00" }]);
  let generation = 0;
  let alive = true;

  onMount(() => {
    void refresh(0);
    return () => { alive = false; generation++; };
  });

  function fail(cause: unknown): void {
    if (!alive) return;
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
    else failure = cause instanceof Error ? cause.message : String(cause);
  }

  async function refresh(nextOffset = offset): Promise<void> {
    const request = ++generation;
    loading = true;
    failure = "";
    try {
      const page = await listSavedQueries(session, undefined, nextOffset);
      if (request !== generation) return;
      items = page.items;
      total = page.total;
      offset = page.offset;
    } catch (cause) {
      if (request === generation) fail(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  function edit(item: SavedQuery): void {
    receiptFailed = false;
    selected = item;
    deleting = null;
    kind = item.kind;
    name = item.name;
    description = item.description;
    if (item.kind === "query") rawQuery = JSON.stringify(item.payload, null, 2);
    else terms = item.payload.terms.map((term) => ({ ...term }));
    failure = "";
    notice = "";
  }

  function fresh(nextKind: SavedQueryKind): void {
    receiptFailed = false;
    selected = null;
    deleting = null;
    kind = nextKind;
    name = "";
    description = "";
    rawQuery = JSON.stringify(initialQuery, null, 2);
    terms = [{ text: "", color: "#ffcc00" }];
    failure = "";
    notice = "";
  }

  function definition(): Definition {
    return kind === "query" ? { kind, payload: parseQuery(rawQuery) }
      : { kind, payload: parseHighlightSet(JSON.stringify({ v: 1, terms })) };
  }

  function keepDraft(): void {
    failure = "";
    notice = "";
    try {
      const query = parseQuery(rawQuery);
      onload(query);
      rawQuery = JSON.stringify(JSON.parse(canonicalQuery(query)), null, 2);
      notice = "Query draft kept in this tab and URL. It has not been executed.";
    } catch (cause) { fail(cause); }
  }

  async function save(asNew: boolean): Promise<void> {
    if (pending || receiptFailed || (!asNew && !selected)) return;
    failure = "";
    notice = "";
    pending = true;
    try {
      const value = definition();
      const saved = asNew
        ? await createSavedQuery(session, { ...value, name, description })
        : await updateSavedQuery(session, selected!, { name, description, payload: value.payload });
      if (!alive) return;
      edit(saved);
      notice = `Saved ${saved.name}.`;
      await refresh(0);
    } catch (cause) { await mutationFailed(cause); }
    finally { if (alive) pending = false; }
  }

  async function remove(): Promise<void> {
    if (!deleting || pending) return;
    pending = true;
    failure = "";
    notice = "";
    try {
      const removed = await deleteSavedQuery(session, deleting);
      if (!alive) return;
      if (selected?.id === removed.id) fresh("query");
      deleting = null;
      notice = `Deleted ${removed.name}. Documents were not changed.`;
      await refresh(0);
    } catch (cause) { await mutationFailed(cause); }
    finally { if (alive) pending = false; }
  }

  async function mutationFailed(cause: unknown): Promise<void> {
    if (!alive) return;
    if (cause instanceof SavedQueryReceiptError) {
      receiptFailed = true;
      deleting = null;
      items = [];
      await refresh();
    }
    fail(cause);
  }
</script>

<DetailDrawer title="Saved queries & highlights" ariaLabel="Saved queries and highlights" width="min(760px, 100vw)" onclose={() => { if (!pending) onclose(); }} closeOnOverlayClick={!pending}>
  <div class="workspace">
    <p class="boundary">Save search intent and reusable literal highlights. Definitions contain no result rows or document bodies.</p>
    {#if failure}<p class="failure" role="alert">{failure}</p>{/if}
    {#if notice}<p class="notice" role="status">{notice}</p>{/if}

    {#if deleting}
      <section aria-label="Delete saved definition" class="editor">
        <h3>Delete {deleting.name}?</h3>
        <p>Revision {deleting.revision} · <code>{deleting.id}</code></p>
        <p>This permanently removes only this saved definition. Documents, other saved definitions and the kept query draft are unchanged.</p>
        <div class="actions">
          <Button tone="danger" disabled={pending} onclick={() => void remove()}>Confirm deletion</Button>
          <Button disabled={pending} onclick={() => { deleting = null; failure = ""; }}>Cancel</Button>
        </div>
      </section>
    {:else}
      <div class="actions">
        <Button disabled={pending} onclick={() => fresh("query")}>New query</Button>
        <Button disabled={pending} onclick={() => fresh("highlight_set")}>New highlight set</Button>
        <Button disabled={pending || loading} onclick={() => void refresh()}>Reload definitions</Button>
      </div>
      <section aria-label="Saved definitions" class="catalog">
        {#if loading}<span><Spinner size={14} /> Loading definitions…</span>{/if}
        {#each items as item (item.id)}
          <div class="definition-row">
            <div><strong>{item.name}</strong><small>{item.kind === "query" ? "Query" : "Highlight set"} · revision {item.revision}</small></div>
            {#if item.kind === "query" && onopenquery}<Button size="sm" disabled={pending} ariaLabel={`Open query ${item.name}`} onclick={() => item.kind === "query" && onopenquery?.(item.payload)}>Open query</Button>{/if}
            <Button size="sm" disabled={pending} onclick={() => edit(item)} ariaLabel={`Edit ${item.name}`}>Edit</Button>
            <Button size="sm" disabled={pending} onclick={() => { deleting = item; failure = ""; notice = ""; }} ariaLabel={`Delete ${item.name}`}>Delete</Button>
          </div>
        {:else}
          {#if !loading}<p>No saved definitions on this page.</p>{/if}
        {/each}
        <div class="actions">
          <span>{items.length} shown · {total} total</span>
          <Button size="sm" disabled={pending || loading || offset === 0} onclick={() => void refresh(Math.max(0, offset - 100))}>Previous</Button>
          <Button size="sm" disabled={pending || loading || offset + 100 >= total} onclick={() => void refresh(offset + 100)}>Next</Button>
        </div>
      </section>

      <section class="editor" aria-label="Definition editor">
        <h3>{selected ? `Edit ${selected.name}` : kind === "query" ? "New query" : "New highlight set"}</h3>
        {#if selected}<small>Revision {selected.revision} · {selected.id}</small>{/if}
        <label for="definition-name">Name</label>
        <TextInput id="definition-name" ariaLabel="Definition name" bind:value={name} disabled={pending} block />
        <label for="definition-description">Description</label>
        <textarea id="definition-description" aria-label="Definition description" bind:value={description} disabled={pending} rows="2" maxlength="4096"></textarea>
        {#if kind === "query"}
          <label for="query-json">Complete query JSON</label>
          <p class="boundary">Expression, facets, mode and sort stay together. Unsupported or unknown fields are never silently removed.</p>
          <textarea id="query-json" aria-label="Complete query JSON" bind:value={rawQuery} disabled={pending} rows="13" maxlength="131072" spellcheck="false"></textarea>
          <p class="boundary">Saved-query execution is unavailable: the current live-search adapter cannot honor the complete query contract. Keeping or saving this draft does not change live results.</p>
        {:else}
          <p class="boundary">Literal terms only: 1–64 unique terms, up to 256 characters each, with lowercase #rrggbb colors. Highlight rendering is not available in the document viewer.</p>
          {#each terms as term, index}
            <div class="term-row">
              <TextInput ariaLabel={`Term ${index + 1}`} bind:value={term.text} disabled={pending} block />
              <TextInput ariaLabel={`Color ${index + 1}`} bind:value={term.color} disabled={pending} />
              <Button size="sm" ariaLabel={`Remove term ${index + 1}`} disabled={pending || terms.length === 1} onclick={() => { terms = terms.filter((_, i) => i !== index); }}>Remove</Button>
            </div>
          {/each}
          <Button size="sm" disabled={pending || terms.length >= 64} onclick={() => { terms = [...terms, { text: "", color: "#ffcc00" }]; }}>Add term</Button>
        {/if}
        <div class="actions">
          <Button tone="info" surface="solid" disabled={pending || receiptFailed || !name} onclick={() => void save(true)}>Save as new</Button>
          {#if selected}<Button disabled={pending || receiptFailed || !name} onclick={() => void save(false)}>Save changes</Button>{/if}
          {#if kind === "query"}<Button disabled={pending} onclick={keepDraft}>Keep query draft</Button>{/if}
        </div>
      </section>
    {/if}
  </div>
</DetailDrawer>

<style>
  .workspace { display: flex; flex-direction: column; gap: var(--space-4); padding: var(--space-5); }
  .boundary, small { color: var(--text-muted); font-size: var(--font-size-sm); }
  .actions { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); }
  .catalog, .editor { display: flex; flex-direction: column; gap: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); padding: var(--space-4); }
  .definition-row { display: flex; align-items: center; gap: var(--space-2); }
  .definition-row > div { flex: 1; min-width: 0; overflow-wrap: anywhere; }
  .definition-row small { display: block; }
  .term-row { display: grid; grid-template-columns: minmax(0, 1fr) minmax(80px, 120px) auto; gap: var(--space-2); }
  textarea { width: 100%; box-sizing: border-box; resize: vertical; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-primary); font: inherit; }
  #query-json { font-family: var(--font-mono); font-size: var(--font-size-sm); }
  .failure { color: var(--accent-red); }
  .notice { color: var(--accent-green); }
  h3, p { margin: 0; }
  code, small { overflow-wrap: anywhere; }
</style>
