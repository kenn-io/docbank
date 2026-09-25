<script lang="ts">
  import { onDestroy } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, Card, Chip, DetailDrawer, EmptyState, IconButton, Spinner, TextInput } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import { createProductionSet, getProductionDraft, getProductionSet, listProductionSets, type ProductionDraft, type ProductionSet } from "./api.js";
  import ProductionReview from "./ProductionReview.svelte";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();
  let sets = $state<ProductionSet[]>([]);
  let cursor = $state("");
  let selected = $state<ProductionSet | null>(null);
  let draft = $state<ProductionDraft | null>(null);
  let name = $state("");
  let instructions = $state("");
  let loading = $state(true);
  let loadingMore = $state(false);
  let draftLoading = $state(false);
  let creating = $state(false);
  let error = $state("");
  let draftError = $state("");
  let createError = $state("");
  let pendingCreate = $state<{ payload: string; operationID: string } | null>(null);
  let listController = new AbortController();
  let draftController = new AbortController();

  $effect(() => {
    const current = session;
    listController.abort();
    draftController.abort();
    listController = new AbortController();
    draftController = new AbortController();
    sets = [];
    cursor = "";
    selected = null;
    draft = null;
    void load(current, "", listController.signal);
    return () => { listController.abort(); draftController.abort(); };
  });
  onDestroy(() => { listController.abort(); draftController.abort(); });

  function fail(cause: unknown): string {
    if (cause instanceof DOMException && cause.name === "AbortError") return "";
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      onclose();
      return "";
    }
    return cause instanceof Error ? cause.message : String(cause);
  }

  async function load(current: string, nextCursor: string, signal: AbortSignal): Promise<void> {
    if (nextCursor) loadingMore = true;
    else loading = true;
    error = "";
    try {
      const page = await listProductionSets(current, nextCursor, signal);
      if (signal.aborted || current !== session) return;
      sets = nextCursor ? [...sets, ...page.items.filter(item => !sets.some(existing => existing.id === item.id))] : page.items;
      cursor = page.next_cursor;
    } catch (cause) {
      if (!signal.aborted && current === session) error = fail(cause);
    } finally {
      if (!signal.aborted && current === session) { loading = false; loadingMore = false; }
    }
  }

  async function inspect(set: ProductionSet): Promise<void> {
    draftController.abort();
    draftController = new AbortController();
    selected = set;
    draft = null;
    draftError = "";
    draftLoading = true;
    try {
      const next = await getProductionDraft(session, set, draftController.signal);
      if (!draftController.signal.aborted && selected?.id === set.id) draft = next;
    } catch (cause) {
      if (!draftController.signal.aborted) draftError = fail(cause);
    } finally {
      if (!draftController.signal.aborted) draftLoading = false;
    }
  }

  async function refresh(): Promise<void> {
    const current = selected;
    if (!current) return;
    draftController.abort();
    draftController = new AbortController();
    const signal = draftController.signal;
    draftLoading = true;
    draftError = "";
    try {
      const latestSet = await getProductionSet(session, current.id, signal);
      const latestDraft = await getProductionDraft(session, latestSet, signal);
      if (signal.aborted || selected?.id !== current.id) return;
      selected = latestSet;
      draft = latestDraft;
      sets = sets.map(item => item.id === latestSet.id ? latestSet : item);
    } catch (cause) {
      if (!signal.aborted) draftError = fail(cause);
    } finally {
      if (!signal.aborted) draftLoading = false;
    }
  }

  async function create(): Promise<void> {
    if (creating || !name.trim()) return;
    const payload = JSON.stringify({ name: name.trim(), instructions });
    if (!pendingCreate || pendingCreate.payload !== payload) {
      pendingCreate = { payload, operationID: crypto.randomUUID() };
    }
    creating = true;
    createError = "";
    try {
      const created = await createProductionSet(session, name.trim(), instructions, pendingCreate.operationID, listController.signal);
      if (listController.signal.aborted) return;
      sets = [created.set, ...sets.filter(item => item.id !== created.set.id)];
      selected = created.set;
      draft = created.draft;
      name = "";
      instructions = "";
      pendingCreate = null;
    } catch (cause) {
      if (!listController.signal.aborted) createError = fail(cause);
    } finally {
      if (!listController.signal.aborted) creating = false;
    }
  }
</script>

<DetailDrawer width="min(900px, 100vw)" ariaLabel="Production sets" onclose={onclose}>
  {#snippet header()}
    <div class="drawer-heading">
      <div><span>PRODUCTION</span><strong>Production sets</strong><small>Create and inspect an exact draft revision</small></div>
      <IconButton size="sm" ariaLabel="Close production sets" onclick={onclose}><XIcon size="14" aria-hidden="true" /></IconButton>
    </div>
  {/snippet}

  <div class="content">
    <section class="new-set" aria-labelledby="production-create-heading">
      <div class="section-heading"><div><span>NEW SET</span><strong id="production-create-heading">Create a draft</strong></div></div>
      <p>A new set starts with an empty, editable draft. Add members and review decisions before finalization.</p>
      <label for="production-set-name">Set name<TextInput id="production-set-name" ariaLabel="Set name" bind:value={name} disabled={creating} block /></label>
      <label for="production-instructions">Instructions<textarea id="production-instructions" aria-label="Instructions" bind:value={instructions} disabled={creating} rows="3" maxlength="65536"></textarea></label>
      {#if createError}<p class="error" role="alert">{createError}</p>{/if}
      <Button tone="info" size="sm" disabled={creating || !name.trim()} onclick={() => void create()}>
        {creating ? "Creating…" : createError && pendingCreate ? "Retry create" : "Create draft"}
      </Button>
    </section>

    <section class="set-list" aria-labelledby="production-list-heading">
      <div class="section-heading"><div><span>RETAINED SETS</span><strong id="production-list-heading">Open a production</strong></div><Chip size="xs" tone="neutral">{sets.length} loaded</Chip></div>
      {#if loading}<p class="loading" role="status"><Spinner size={16} /> Loading production sets…</p>{/if}
      {#if error}<p class="error" role="alert">{error}</p><Button size="sm" onclick={() => void load(session, "", listController.signal)}>Retry load</Button>{/if}
      {#if !loading && sets.length === 0}<EmptyState title="No production sets yet" description="Create a draft to begin a reviewed production." />{/if}
      {#if sets.length > 0}
        <div class="set-items">
          {#each sets as set (set.id)}
            <Button size="sm" surface={selected?.id === set.id ? "solid" : "soft"} onclick={() => void inspect(set)}>{set.name}</Button>
          {/each}
        </div>
      {/if}
      {#if cursor}<Button size="sm" disabled={loadingMore} onclick={() => void load(session, cursor, listController.signal)}>{loadingMore ? "Loading…" : "Load more sets"}</Button>{/if}
    </section>

    {#if selected}
      <Card level="default" padding="sm" title={selected.name} eyebrow="SELECTED SET">
        {#if draftLoading}<p class="loading" role="status"><Spinner size={16} /> Loading exact draft…</p>{/if}
        {#if draftError}<p class="error" role="alert">{draftError}</p>{/if}
        {#if draft}
          <div class="draft-heading"><strong>Draft revision {draft.revision}</strong><Chip size="xs" tone={draft.state === "finalized" ? "success" : "neutral"}>{draft.state}</Chip></div>
          <dl><div><dt>Change version</dt><dd>{draft.etag}</dd></div><div><dt>Membership</dt><dd>{draft.membership_sealed ? "Membership sealed" : "Membership open"}</dd></div></dl>
          <p>Draft changes and review declarations are bound to this exact revision.</p>
        {/if}
      </Card>
      {#if draft}
        {#key `${selected.id}:${draft.revision}:${draft.etag}`}
          <ProductionReview {session} set={selected} {draft} onrefresh={() => void refresh()} {onauthfailure} {onclose} />
        {/key}
      {/if}
    {/if}
  </div>
</DetailDrawer>

<style>
  .drawer-heading,.section-heading,.draft-heading{display:flex;align-items:center;justify-content:space-between;gap:var(--space-3)}
  .drawer-heading{width:100%}.drawer-heading>div,.section-heading>div,.content,.new-set,.set-list{display:grid;gap:var(--space-3)}
  .drawer-heading span,.section-heading span{font-size:var(--font-size-xs);font-weight:var(--font-weight-bold);color:var(--text-muted)}
  .drawer-heading strong{font-size:var(--font-size-lg)}.drawer-heading small{color:var(--text-muted)}
  .content{padding:var(--space-5);gap:var(--space-5);overflow:auto}.new-set,.set-list{padding:var(--space-4);border:1px solid var(--border-muted);border-radius:var(--radius-lg);background:var(--bg-inset)}
  label{display:grid;gap:var(--space-2);font-size:var(--font-size-sm)}
  textarea{width:100%;resize:vertical;padding:var(--space-3);border:1px solid var(--border-muted);border-radius:var(--radius-md);color:var(--text-primary);background:var(--bg-raised);font:inherit}
  p{margin:0;font-size:var(--font-size-sm);color:var(--text-secondary);line-height:1.5}.error{color:var(--accent-red)}
  .set-items{display:flex;flex-wrap:wrap;gap:var(--space-2)}.loading{display:flex;align-items:center;gap:var(--space-2)}
  dl{display:flex;gap:var(--space-5);margin:var(--space-3) 0}dt{font-size:var(--font-size-xs);color:var(--text-muted)}dd{margin:var(--space-1) 0 0;font-size:var(--font-size-sm)}
  @media(max-width:640px){.content{padding:var(--space-3)}dl{display:grid;gap:var(--space-3)}}
</style>
