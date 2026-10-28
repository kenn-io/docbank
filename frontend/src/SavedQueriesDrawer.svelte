<script lang="ts">
  import DrawerShell from "./DrawerShell.svelte";
  import { onMount, untrack } from "svelte";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import PencilIcon from "@lucide/svelte/icons/pencil";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import Trash2Icon from "@lucide/svelte/icons/trash-2";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, IconButton, Spinner, TextInput } from "@kenn-io/kit-ui";
  import "./toolForm.css";
  import { APIError } from "./api-transport.js";
  import { showToast } from "./toast.svelte.js";
  import {
    canonicalQuery,
    parseHighlightSet,
    parseQuery,
    type HighlightTerm,
    type Query,
  } from "./query.js";
  import {
    createSavedQuery,
    deleteSavedQuery,
    listSavedQueries,
    SavedQueryReceiptError,
    updateSavedQuery,
    type Definition,
    type SavedQuery,
    type SavedQueryKind,
  } from "./savedQueries.js";

  let {
    session,
    initialQuery,
    createSearch = false,
    onload,
    onclose,
    onauthfailure,
    onopenquery,
  }: {
    session: string;
    initialQuery: Query;
    createSearch?: boolean;
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
  let editorOpen = $state(untrack(() => createSearch));
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
    return () => {
      alive = false;
      generation++;
    };
  });

  function fail(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      if (alive) onauthfailure(cause);
      return;
    }
    const message = cause instanceof Error ? cause.message : String(cause);
    // The Searches page can be left while a change is pending; its outcome still needs reporting.
    if (alive) failure = message;
    else showToast(message, { tone: "danger" });
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
    editorOpen = true;
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
    editorOpen = true;
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
    return kind === "query"
      ? { kind, payload: parseQuery(rawQuery) }
      : { kind, payload: parseHighlightSet(JSON.stringify({ v: 1, terms })) };
  }

  function keepDraft(): void {
    failure = "";
    notice = "";
    try {
      const query = parseQuery(rawQuery);
      onload(query);
      rawQuery = JSON.stringify(JSON.parse(canonicalQuery(query)), null, 2);
      notice = "Search draft saved.";
    } catch (cause) {
      fail(cause);
    }
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
      if (!alive) {
        showToast(`Saved ${saved.name}.`);
        return;
      }
      edit(saved);
      notice = `Saved ${saved.name}.`;
      await refresh(0);
    } catch (cause) {
      await mutationFailed(cause);
    } finally {
      if (alive) pending = false;
    }
  }

  async function remove(): Promise<void> {
    if (!deleting || pending) return;
    pending = true;
    failure = "";
    notice = "";
    try {
      const removed = await deleteSavedQuery(session, deleting);
      if (!alive) {
        showToast(`Deleted ${removed.name}.`);
        return;
      }
      if (selected?.id === removed.id) {
        selected = null;
        editorOpen = false;
      }
      deleting = null;
      notice = `Deleted ${removed.name}.`;
      await refresh(0);
    } catch (cause) {
      await mutationFailed(cause);
    } finally {
      if (alive) pending = false;
    }
  }

  async function mutationFailed(cause: unknown): Promise<void> {
    if (alive && cause instanceof SavedQueryReceiptError) {
      receiptFailed = true;
      deleting = null;
      items = [];
      await refresh();
    }
    fail(cause);
  }
</script>

<DrawerShell
  title="Searches"
  onclose={() => {
    if (!pending) onclose();
  }}
  closeOnOverlayClick={!pending}
>
  {#snippet actions()}
    <Button size="sm" disabled={pending} onclick={() => fresh("highlight_set")}
      ><PlusIcon size="14" aria-hidden="true" /> New highlight list</Button
    >
    <IconButton
      size="sm"
      ariaLabel="Refresh searches"
      title="Refresh"
      disabled={pending || loading}
      onclick={() => void refresh()}><RefreshCwIcon size="14" aria-hidden="true" /></IconButton
    >
  {/snippet}
  <div class="tool-body">
    {#if failure}<p class="tool-error" role="alert">{failure}</p>{/if}
    {#if notice}<p class="tool-muted" role="status">{notice}</p>{/if}

    {#if deleting}
      <section aria-label="Delete saved definition" class="tool-section">
        <h3>Delete {deleting.name}?</h3>
        <div class="tool-actions">
          <Button
            disabled={pending}
            onclick={() => {
              deleting = null;
              failure = "";
            }}>Cancel</Button
          >
          <Button tone="danger" surface="solid" disabled={pending} onclick={() => void remove()}
            >Confirm deletion</Button
          >
        </div>
      </section>
    {:else}
      {#if editorOpen}<section class="tool-section" aria-label="Definition editor">
          <h3>
            {selected
              ? `Edit ${selected.name}`
              : kind === "query"
                ? "New search"
                : "New highlight list"}
          </h3>
          <div class="tool-grid">
            <div class="tool-field tool-span-all">
              <label for="definition-name">Name</label>
              <TextInput
                id="definition-name"
                ariaLabel="Definition name"
                bind:value={name}
                disabled={pending}
                block
              />
            </div>
            <div class="tool-field tool-span-all">
              <label for="definition-description">Description</label>
              <textarea
                id="definition-description"
                aria-label="Definition description"
                bind:value={description}
                disabled={pending}
                rows="2"
                maxlength="4096"></textarea>
            </div>
            {#if kind === "query"}
              <details class="tool-disclosure tool-span-all">
                <summary
                  ><ChevronRightIcon size="14" aria-hidden="true" /><span class="tool-label"
                    >Edit as JSON</span
                  ></summary
                >
                <div class="tool-section">
                  <textarea
                    id="query-json"
                    aria-label="Complete query JSON"
                    bind:value={rawQuery}
                    disabled={pending}
                    rows="10"
                    maxlength="131072"
                    spellcheck="false"></textarea>
                  <div class="tool-actions tool-actions--start">
                    <Button size="sm" disabled={pending} onclick={keepDraft}
                      >Keep query draft</Button
                    >
                  </div>
                </div>
              </details>
            {:else}
              <div class="tool-field tool-span-all">
                <span class="tool-label">Terms</span>
                {#each terms as term, index}
                  <div class="term-row">
                    <TextInput
                      ariaLabel={`Term ${index + 1}`}
                      bind:value={term.text}
                      disabled={pending}
                      block
                    />
                    <TextInput
                      ariaLabel={`Color ${index + 1}`}
                      bind:value={term.color}
                      disabled={pending}
                      block
                    />
                    <span
                      >{#if terms.length > 1}<IconButton
                          size="sm"
                          ariaLabel={`Remove term ${index + 1}`}
                          title="Remove term"
                          disabled={pending}
                          onclick={() => {
                            terms = terms.filter((_, i) => i !== index);
                          }}><XIcon size="14" aria-hidden="true" /></IconButton
                        >{/if}</span
                    >
                  </div>
                {/each}
                <div class="tool-actions tool-actions--start">
                  <Button
                    size="sm"
                    disabled={pending || terms.length >= 64}
                    onclick={() => {
                      terms = [...terms, { text: "", color: "#ffcc00" }];
                    }}><PlusIcon size="14" aria-hidden="true" /> Add term</Button
                  >
                </div>
              </div>
            {/if}
          </div>
          <div class="tool-actions">
            <Button disabled={pending} onclick={() => (editorOpen = false)}>Cancel</Button>
            <Button
              tone={selected ? "neutral" : "info"}
              surface={selected ? "outline" : "solid"}
              disabled={pending || receiptFailed || !name}
              onclick={() => void save(true)}>Save as new</Button
            >
            {#if selected}<Button
                tone="info"
                surface="solid"
                disabled={pending || receiptFailed || !name}
                onclick={() => void save(false)}>Save changes</Button
              >{/if}
          </div>
        </section>{/if}

      <section aria-label="Saved definitions" class="tool-section">
        {#if loading}<p class="tool-muted"><Spinner size={14} /> Loading…</p>{/if}
        {#if items.length}
          <ul class="tool-list">
            {#each items as item (item.id)}
              <li>
                <div class="tool-row-main">
                  <strong>{item.name}</strong>
                  {#if item.description || item.kind === "highlight_set"}<span
                      >{[item.kind === "highlight_set" ? "Highlight list" : "", item.description]
                        .filter(Boolean)
                        .join(" · ")}</span
                    >{/if}
                </div>
                <div class="tool-row-actions">
                  {#if item.kind === "query" && onopenquery}<Button
                      size="sm"
                      disabled={pending}
                      ariaLabel={`Run ${item.name}`}
                      onclick={() => item.kind === "query" && onopenquery?.(item.payload)}
                      >Run</Button
                    >{/if}
                  <IconButton
                    size="sm"
                    disabled={pending}
                    onclick={() => edit(item)}
                    title="Rename"
                    ariaLabel={`Rename ${item.name}`}><PencilIcon size="14" /></IconButton
                  >
                  <IconButton
                    size="sm"
                    disabled={pending}
                    onclick={() => {
                      deleting = item;
                      failure = "";
                      notice = "";
                    }}
                    title="Delete"
                    ariaLabel={`Delete ${item.name}`}><Trash2Icon size="14" /></IconButton
                  >
                </div>
              </li>
            {/each}
          </ul>
        {:else if !loading}<p class="tool-muted">
            No searches yet. Save one from Advanced → Save….
          </p>{/if}
        {#if total > 100}<div class="tool-actions">
            <span class="tool-muted">{offset + 1}–{offset + items.length} of {total}</span>
            <Button
              size="sm"
              disabled={pending || loading || offset === 0}
              onclick={() => void refresh(Math.max(0, offset - 100))}>Previous</Button
            >
            <Button
              size="sm"
              disabled={pending || loading || offset + 100 >= total}
              onclick={() => void refresh(offset + 100)}>Next</Button
            >
          </div>{/if}
      </section>
    {/if}
  </div>
</DrawerShell>

<style>
  .term-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 112px 28px;
    align-items: center;
    gap: var(--space-4);
  }
  textarea {
    width: 100%;
    box-sizing: border-box;
    resize: vertical;
    padding: var(--space-3) var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
  }
  #query-json {
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
  }
</style>
