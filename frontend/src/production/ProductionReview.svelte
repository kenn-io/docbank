<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Chip, EmptyState, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import {
    getProductionDraftAt, listProductionMembers, listUncertainDecisions,
    type ProductionDecision, type ProductionDraft, type ProductionMember, type ProductionSet,
  } from "./api.js";

  interface Props {
    session: string;
    set: ProductionSet;
    draft: ProductionDraft;
    onrefresh: () => void;
    onauthfailure: (cause: unknown) => void;
    onclose: () => void;
  }

  let { session, set, draft, onrefresh, onauthfailure, onclose }: Props = $props();
  let members = $state<ProductionMember[]>([]);
  let flags = $state<ProductionDecision[]>([]);
  let memberCursor = $state("");
  let flagCursor = $state("");
  let membersLoading = $state(true);
  let flagsLoading = $state(true);
  let membersError = $state("");
  let flagsError = $state("");
  let stale = $state(false);
  let authFailed = false;
  let membersController = new AbortController();
  let flagsController = new AbortController();

  type Scope = { session: string; setID: string; revision: number; etag: number };

  $effect(() => {
    const scope = { session, setID: set.id, revision: draft.revision, etag: draft.etag };
    membersController.abort();
    flagsController.abort();
    membersController = new AbortController();
    flagsController = new AbortController();
    members = [];
    flags = [];
    memberCursor = "";
    flagCursor = "";
    membersLoading = true;
    flagsLoading = true;
    membersError = "";
    flagsError = "";
    stale = false;
    authFailed = false;
    void loadMembers(scope, "", membersController.signal);
    void loadFlags(scope, "", flagsController.signal);
    return () => { membersController.abort(); flagsController.abort(); };
  });
  onDestroy(() => { membersController.abort(); flagsController.abort(); });

  function current(scope: Scope, signal: AbortSignal): boolean {
    return !signal.aborted && !stale && scope.session === session && scope.setID === set.id &&
      scope.revision === draft.revision && scope.etag === draft.etag;
  }

  function fail(cause: unknown): string {
    if (cause instanceof DOMException && cause.name === "AbortError") return "";
    if (cause instanceof APIError && cause.status === 401) {
      if (!authFailed) { authFailed = true; onauthfailure(cause); onclose(); }
      membersController.abort();
      flagsController.abort();
      return "";
    }
    return cause instanceof Error ? cause.message : String(cause);
  }

  async function unchanged(scope: Scope, signal: AbortSignal): Promise<boolean> {
    const fresh = await getProductionDraftAt(scope.session, scope.setID, scope.revision, signal);
    if (!current(scope, signal)) return false;
    if (fresh.etag !== scope.etag || fresh.state !== draft.state) {
      stale = true;
      members = [];
      flags = [];
      memberCursor = "";
      flagCursor = "";
      membersController.abort();
      flagsController.abort();
      return false;
    }
    return true;
  }

  async function loadMembers(scope: Scope, cursor: string, signal: AbortSignal): Promise<void> {
    membersLoading = true;
    membersError = "";
    try {
      const page = await listProductionMembers(scope.session, scope.setID, scope.revision, cursor, signal);
      if (!await unchanged(scope, signal)) return;
      members = cursor ? [...members, ...page.items.filter(item => !members.some(existing => existing.id === item.id))] : page.items;
      memberCursor = page.next_cursor;
    } catch (cause) {
      if (current(scope, signal)) membersError = fail(cause);
    } finally {
      if (current(scope, signal)) membersLoading = false;
    }
  }

  async function loadFlags(scope: Scope, cursor: string, signal: AbortSignal): Promise<void> {
    flagsLoading = true;
    flagsError = "";
    try {
      const page = await listUncertainDecisions(scope.session, scope.setID, scope.revision, cursor, signal);
      if (!await unchanged(scope, signal)) return;
      flags = cursor ? [...flags, ...page.items.filter(item => !flags.some(existing => existing.id === item.id))] : page.items;
      flagCursor = page.next_cursor;
    } catch (cause) {
      if (current(scope, signal)) flagsError = fail(cause);
    } finally {
      if (current(scope, signal)) flagsLoading = false;
    }
  }

  function scope(): Scope { return { session, setID: set.id, revision: draft.revision, etag: draft.etag }; }

  function selectorLocation(decision: ProductionDecision): string {
    const pages = decision.selector.pages ?? [...new Set((decision.selector.boxes ?? []).map(box => box.page))];
    if (pages.length === 1) return `Page ${pages[0]}`;
    if (pages.length > 1) return `Pages ${pages.slice(0, 5).join(", ")}${pages.length > 5 ? `, and ${pages.length - 5} more` : ""}`;
    if (decision.selector.span) return `Text bytes ${decision.selector.span.start}–${decision.selector.span.end}`;
    return decision.selector.kind;
  }
</script>

<div class="review">
  {#if stale}
    <div class="stale" role="alert">
      <p>Draft changed during review. Refresh to load the current version.</p>
      <Button size="sm" onclick={onrefresh}>Refresh draft</Button>
    </div>
  {:else}
    <section aria-labelledby="production-members-heading">
      <div class="section-heading"><div><span>EXACT MEMBERS</span><strong id="production-members-heading">Selected sources</strong></div><Chip size="xs" tone="neutral">{members.length} loaded</Chip></div>
      {#if membersLoading}<p class="loading" role="status"><Spinner size={16} /> Loading members…</p>{/if}
      {#if membersError}<p class="error" role="alert">{membersError}</p><Button size="sm" onclick={() => void loadMembers(scope(), memberCursor, membersController.signal)}>Retry members</Button>{/if}
      {#if !membersLoading && !membersError && members.length === 0}<EmptyState title="No members yet" description="This draft has no source versions to review." />{/if}
      {#if members.length > 0}
        <ol class="rows">
          {#each members as member (member.id)}
            <li><div><strong>Member {member.ordinal}</strong><Chip size="xs" tone={member.reviewed ? "success" : "neutral"}>{member.reviewed ? "Reviewed" : "Needs review"}</Chip></div>
              <small>Source version <code>{member.source_version_id}</code></small>
              <small>Mode {member.mode === "keep_selected" ? "Keep selected" : "Redact selected"}</small>
            </li>
          {/each}
        </ol>
      {/if}
      {#if memberCursor}<Button size="sm" disabled={membersLoading} onclick={() => void loadMembers(scope(), memberCursor, membersController.signal)}>{membersLoading ? "Loading…" : "Load more members"}</Button>{/if}
    </section>

    <section aria-labelledby="production-flags-heading">
      <div class="section-heading"><div><span>UNCERTAINTY</span><strong id="production-flags-heading">Flagged passages</strong></div><Chip size="xs" tone="neutral">{flags.length} loaded</Chip></div>
      {#if flagsLoading}<p class="loading" role="status"><Spinner size={16} /> Loading flagged passages…</p>{/if}
      {#if flagsError}<p class="error" role="alert">{flagsError}</p><Button size="sm" onclick={() => void loadFlags(scope(), flagCursor, flagsController.signal)}>Retry flagged passages</Button>{/if}
      {#if !flagsLoading && !flagsError && flags.length === 0}<EmptyState title="No flagged passages" description="Uncertain keep decisions will appear here for review." />{/if}
      {#if flags.length > 0}
        <ol class="rows">
          {#each flags as flag (flag.id)}
            <li><div><strong>{selectorLocation(flag)}</strong><Chip size="xs" tone="info">Keep · uncertain</Chip></div>
              <small>Member <code>{flag.member_id}</code></small></li>
          {/each}
        </ol>
      {/if}
      {#if flagCursor}<Button size="sm" disabled={flagsLoading} onclick={() => void loadFlags(scope(), flagCursor, flagsController.signal)}>{flagsLoading ? "Loading…" : "Load more flagged passages"}</Button>{/if}
    </section>
  {/if}
</div>

<style>
  .review{display:grid;gap:var(--space-4)}
  section,.stale{display:grid;gap:var(--space-3);padding:var(--space-4);border:1px solid var(--border-muted);border-radius:var(--radius-lg);background:var(--bg-inset)}
  .section-heading,.rows li>div{display:flex;align-items:center;justify-content:space-between;gap:var(--space-3)}
  .section-heading>div{display:grid;gap:var(--space-1)}.section-heading span{font-size:var(--font-size-xs);font-weight:var(--font-weight-bold);color:var(--text-muted)}
  .rows{display:grid;gap:var(--space-2);list-style:none;margin:0;padding:0}.rows li{display:grid;gap:var(--space-1);padding:var(--space-3);background:var(--bg-raised);border-radius:var(--radius-md)}
  small{color:var(--text-muted);overflow-wrap:anywhere}code{font-size:inherit}.loading{display:flex;align-items:center;gap:var(--space-2)}p{margin:0;font-size:var(--font-size-sm)}.error{color:var(--accent-red)}
</style>
