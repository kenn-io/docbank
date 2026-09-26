<script lang="ts">
  import { onDestroy, tick } from "svelte";
  import { Button, Chip, EmptyState, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import {
    applyProductionChange, getProductionDraftAt, listProductionMembers, listUncertainDecisions,
    type ProductionChange, type ProductionDecision, type ProductionDraft, type ProductionMember, type ProductionSet,
  } from "./api.js";
  import { loadReviewContext, type ReviewContext } from "./reviewContext.js";
  import { loadProductionSourcePDF } from "./sourcePDF.js";
  import { embedpdfMarqueeBox, type Frame } from "./embedpdfAdapter.js";
  import { loadSelectionFrame } from "./selectionFrame.js";
  import type { Marquee } from "./SourcePageSelection.svelte";

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
  let changeError = $state("");
  let changing = $state(false);
  let contextLoading = $state(false);
  let contextError = $state("");
  let contextFlagID = $state("");
  let inspected = $state<{ flagID: string; context: ReviewContext } | null>(null);
  let redactReason = $state("");
  let redactLabel = $state("");
  let stale = $state(false);
  let sourceLoading = $state(false);
  let sourceError = $state("");
  let sourceMemberID = $state("");
  let sourceEditor = $state.raw<{ member: ProductionMember; bytes: Uint8Array<ArrayBuffer> } | null>(null);
  let SourceViewer = $state.raw<typeof import("./SourcePDFViewer.svelte").default | null>(null);
  let selectionLoading = $state(false);
  let selectionError = $state("");
  let selectedRegion = $state<{ member: ProductionMember; page: number; frame: Frame; selector: ProductionDecision["selector"] } | null>(null);
  let authFailed = false;
  let membersController = new AbortController();
  let flagsController = new AbortController();
  let changeController = new AbortController();
  let contextController = new AbortController();
  let sourceController = new AbortController();
  let selectionController = new AbortController();

  type Scope = { session: string; setID: string; revision: number; etag: number };
  type PendingChange = { scope: Scope; operationID: string; change: ProductionChange };
  let pendingChange = $state<PendingChange | null>(null);

  $effect(() => {
    const scope = { session, setID: set.id, revision: draft.revision, etag: draft.etag };
    membersController.abort();
    flagsController.abort();
    changeController.abort();
    contextController.abort();
    sourceController.abort();
    selectionController.abort();
    membersController = new AbortController();
    flagsController = new AbortController();
    changeController = new AbortController();
    contextController = new AbortController();
    sourceController = new AbortController();
    selectionController = new AbortController();
    members = [];
    flags = [];
    memberCursor = "";
    flagCursor = "";
    membersLoading = true;
    flagsLoading = true;
    membersError = "";
    flagsError = "";
    changeError = "";
    changing = false;
    pendingChange = null;
    inspected = null;
    contextFlagID = "";
    contextError = "";
    contextLoading = false;
    redactReason = "";
    redactLabel = "";
    stale = false;
    sourceLoading = false;
    sourceError = "";
    sourceMemberID = "";
    sourceEditor = null;
    selectionLoading = false;
    selectionError = "";
    selectedRegion = null;
    authFailed = false;
    void loadMembers(scope, "", membersController.signal);
    void loadFlags(scope, "", flagsController.signal);
    return () => { membersController.abort(); flagsController.abort(); changeController.abort(); contextController.abort(); sourceController.abort(); selectionController.abort(); };
  });
  onDestroy(() => { membersController.abort(); flagsController.abort(); changeController.abort(); contextController.abort(); sourceController.abort(); selectionController.abort(); });

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
      changeController.abort();
      contextController.abort();
      sourceController.abort();
      selectionController.abort();
      sourceEditor = null;
      selectedRegion = null;
      return "";
    }
    return cause instanceof Error ? cause.message : String(cause);
  }

  async function unchanged(scope: Scope, signal: AbortSignal): Promise<boolean> {
    const fresh = await getProductionDraftAt(scope.session, scope.setID, scope.revision, signal);
    if (!current(scope, signal)) return false;
    if (fresh.etag !== scope.etag || fresh.state !== draft.state) {
      markStale();
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

  function markStale(): void {
    stale = true;
    members = [];
    flags = [];
    memberCursor = "";
    flagCursor = "";
    pendingChange = null;
    inspected = null;
    contextController.abort();
    sourceController.abort();
    selectionController.abort();
    sourceEditor = null;
    selectedRegion = null;
    changeError = "";
    membersController.abort();
    flagsController.abort();
  }

  async function sendChange(pending: PendingChange): Promise<void> {
    if (changing || !current(pending.scope, changeController.signal)) return;
    changing = true;
    changeError = "";
    try {
      await applyProductionChange(pending.scope.session, pending.scope.setID, pending.scope.revision,
        pending.scope.etag, pending.operationID, pending.change, changeController.signal);
      if (!current(pending.scope, changeController.signal)) return;
      markStale();
      onrefresh();
    } catch (cause) {
      if (!current(pending.scope, changeController.signal)) return;
      if (cause instanceof APIError && cause.status === 409 &&
          (cause.code === "production_revision_conflict" || cause.code === "source_stale")) markStale();
      else {
        changeError = fail(cause);
        if (cause instanceof APIError && cause.status >= 400 && cause.status < 500) pendingChange = null;
      }
    } finally {
      changing = false;
    }
  }

  function change(next: ProductionChange): void {
    if (draft.state !== "draft" || changing || pendingChange || stale) return;
    pendingChange = { scope: scope(), operationID: crypto.randomUUID(), change: next };
    void sendChange(pendingChange);
  }

  function decide(flag: ProductionDecision, action: "keep" | "redact"): void {
    if (inspected?.flagID !== flag.id) return;
    if (action === "redact" && !validRedactInput()) return;
    change({ kind: "decision", decision: {
      id: flag.id, member_id: flag.member_id, action, uncertain: false,
      reason: action === "redact" ? redactReason.trim() : flag.reason ?? "",
      label: action === "redact" ? redactLabel.trim() : flag.label ?? "", selector: flag.selector,
    } });
  }

  function validRedactInput(): boolean {
    const encoder = new TextEncoder();
    return redactReason.trim().length > 0 && encoder.encode(redactReason.trim()).length <= 4096 &&
      encoder.encode(redactLabel.trim()).length <= 256;
  }

  async function openSource(member: ProductionMember): Promise<void> {
    if (draft.state !== "draft" || sourceLoading || stale) return;
    sourceController.abort();
    sourceController = new AbortController();
    const signal = sourceController.signal;
    const exact = scope();
    sourceEditor = null;
    selectionController.abort();
    selectedRegion = null;
    sourceMemberID = member.id;
    sourceError = "";
    sourceLoading = true;
    try {
      const bytes = await loadProductionSourcePDF(exact.session, exact.setID, exact.revision, exact.etag, member, signal);
      if (!await unchanged(exact, signal)) return;
      if (!current(exact, signal)) return;
      SourceViewer = (await import("./SourcePDFViewer.svelte")).default;
      if (!current(exact, signal)) return;
      sourceEditor = { member, bytes };
      await tick();
      document.getElementById("production-source-close")?.querySelector("button")?.focus();
    } catch (cause) {
      if (current(exact, signal)) sourceError = fail(cause);
    } finally {
      if (!signal.aborted) sourceLoading = false;
    }
  }

  async function closeSource(): Promise<void> {
    const memberID = sourceMemberID;
    sourceController.abort();
    selectionController.abort();
    sourceEditor = null;
    selectedRegion = null;
    sourceLoading = false;
    sourceError = "";
    await tick();
    document.getElementById(`production-source-open-${memberID}`)?.querySelector("button")?.focus();
  }

  async function selectRegion(member: ProductionMember, page: number, marquee?: Marquee): Promise<void> {
    if (draft.state !== "draft" || stale || sourceEditor?.member.id !== member.id || changing || pendingChange) return;
    selectionController.abort();
    selectionController = new AbortController();
    const signal = selectionController.signal;
    const exact = scope();
    selectionLoading = true;
    selectionError = "";
    selectedRegion = null;
    try {
      const frame = await loadSelectionFrame(exact.session, exact.setID, exact.revision, exact.etag,
        member.id, member.map_sha256, page, signal);
      if (!current(exact, signal) || sourceEditor?.member.id !== member.id) return;
      if (marquee && marquee.pageIndex + 1 !== page) throw new Error("The selection is on another page.");
      const selector: ProductionDecision["selector"] = marquee
        ? { kind: "rectangle", map_sha256: member.map_sha256,
          boxes: [embedpdfMarqueeBox(frame, marquee.rect, marquee.page, marquee.displayed)] }
        : { kind: "page", map_sha256: member.map_sha256, pages: [page] };
      selectedRegion = { member, page, frame, selector };
      await tick();
      document.getElementById("production-selected-region")?.focus();
    } catch (cause) {
      if (current(exact, signal)) selectionError = fail(cause);
    } finally {
      if (!signal.aborted) selectionLoading = false;
    }
  }

  async function inspect(flag: ProductionDecision): Promise<void> {
    if (contextLoading || draft.state !== "draft" || flag.selector.kind !== "text") return;
    contextController.abort();
    contextController = new AbortController();
    const signal = contextController.signal;
    const exact = scope();
    contextFlagID = flag.id;
    contextError = "";
    inspected = null;
    redactReason = "";
    redactLabel = flag.label ?? "";
    contextLoading = true;
    try {
      const context = await loadReviewContext(exact.session, exact.setID, exact.revision, flag.member_id, flag.selector, signal);
      if (!await unchanged(exact, signal)) return;
      if (current(exact, signal)) inspected = { flagID: flag.id, context };
    } catch (cause) {
      if (current(exact, signal)) contextError = fail(cause);
    } finally {
      if (!signal.aborted) contextLoading = false;
    }
  }

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
    {#if changeError}<div class="change-error" role="alert"><p>{changeError}</p>{#if pendingChange}<Button size="sm" disabled={changing} onclick={() => pendingChange && void sendChange(pendingChange)}>Retry change</Button>{/if}<Button size="sm" surface="soft" onclick={onrefresh}>Refresh draft</Button></div>{/if}
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
              {#if draft.state === "draft"}
                <span id={`production-source-open-${member.id}`}><Button size="sm" surface="soft" disabled={sourceLoading}
                  ariaLabel={`Open original PDF for member ${member.ordinal}`}
                  onclick={() => void openSource(member)}>Open original PDF</Button></span>
                <Button size="sm" surface="soft" disabled={changing || !!pendingChange}
                  ariaLabel={`Use ${member.mode === "keep_selected" ? "Redact" : "Keep"} selected for member ${member.ordinal}`}
                  onclick={() => change({ kind: "mode", member_id: member.id, mode: member.mode === "keep_selected" ? "redact_selected" : "keep_selected" })}>
                  Use {member.mode === "keep_selected" ? "Redact" : "Keep"} selected
                </Button>
              {/if}
            </li>
          {/each}
        </ol>
      {/if}
      {#if sourceLoading}<p class="loading" role="status"><Spinner size={16} /> Verifying original PDF…</p>{/if}
      {#if sourceError}<p class="error" role="alert">{sourceError}</p>{/if}
      {#if sourceEditor && SourceViewer}
        <div role="region" aria-label={`Original PDF for member ${sourceEditor.member.ordinal}`}>
          <div class="section-heading"><strong>Original PDF · member {sourceEditor.member.ordinal}</strong>
            <span id="production-source-close"><Button size="sm" surface="soft" onclick={() => void closeSource()}>Close original PDF</Button></span></div>
          <div class="source-workspace" class:has-selection={!!selectedRegion}>
            <div class="source-viewer-column">
              {#key sourceEditor.member.id}
                <SourceViewer bytes={sourceEditor.bytes} memberID={sourceEditor.member.id}
                  highlight={selectedRegion ? { page: selectedRegion.page, frame: selectedRegion.frame,
                    box: selectedRegion.selector.boxes?.[0] } : null}
                  onmarquee={selection => void selectRegion(sourceEditor!.member, selection.pageIndex + 1, selection)}
                  onpage={page => void selectRegion(sourceEditor!.member, page)} />
              {/key}
            </div>
            {#if selectedRegion}
              <div id="production-selected-region" class="selected-region" role="region" tabindex="-1" aria-label={`Selected region on page ${selectedRegion.page}`}>
                <strong>{selectedRegion.selector.kind === "page" ? "Whole page" : "Selected rectangle"} · page {selectedRegion.page}</strong>
                <p>The selection is bound to this member’s retained map and current draft.</p>
                <div class="decision-actions">
                  <Button size="sm" surface="soft" disabled={changing || !!pendingChange} onclick={() => selectedRegion = null}>Clear selection</Button>
                </div>
              </div>
            {/if}
          </div>
          {#if selectionLoading}<p class="loading" role="status"><Spinner size={16} /> Verifying selected page…</p>{/if}
          {#if selectionError}<p class="error" role="alert">{selectionError}</p>{/if}
        </div>
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
              <small>Member <code>{flag.member_id}</code></small>
              {#if draft.state === "draft" && flag.selector.kind === "text"}
                <Button size="sm" surface="soft" disabled={contextLoading || changing || !!pendingChange}
                  ariaLabel={`Inspect flagged text at ${selectorLocation(flag).toLowerCase()}`}
                  onclick={() => void inspect(flag)}>Inspect flagged text</Button>
                {#if contextFlagID === flag.id && contextLoading}<p class="loading" role="status"><Spinner size={14} /> Verifying exact source text…</p>{/if}
                {#if contextFlagID === flag.id && contextError}<p class="error" role="alert">{contextError}</p>{/if}
                {#if inspected?.flagID === flag.id}
                  <p class="source-context" aria-label="Verified source context">{inspected.context.before}<mark>{inspected.context.selected}</mark>{inspected.context.after}</p>
                  <label for={`production-redact-reason-${flag.id}`}>Private reason for redaction
                    <textarea id={`production-redact-reason-${flag.id}`} bind:value={redactReason} rows="2" maxlength="4096" placeholder="Enter the reason for this redaction"></textarea>
                  </label>
                  <label for={`production-redact-label-${flag.id}`}>Public label
                    <input id={`production-redact-label-${flag.id}`} type="text" bind:value={redactLabel} maxlength="256" placeholder="Optional label" />
                  </label>
                  {#if !redactReason.trim()}<small>Enter a new private reason to redact this passage.</small>{/if}
                  {#if redactReason.trim() && !validRedactInput()}<small class="error">Reason or label exceeds its UTF-8 byte limit.</small>{/if}
                  <div class="decision-actions">
                    <Button size="sm" surface="soft" disabled={changing || !!pendingChange} ariaLabel={`Keep passage at ${selectorLocation(flag).toLowerCase()}`} onclick={() => decide(flag, "keep")}>Keep</Button>
                    <Button size="sm" tone="danger" disabled={changing || !!pendingChange || !validRedactInput()} ariaLabel={`Redact passage at ${selectorLocation(flag).toLowerCase()}`} onclick={() => decide(flag, "redact")}>Redact</Button>
                  </div>
                {/if}
              {:else if draft.state === "draft"}
                <small>This region needs the original source editor before a decision can be made.</small>
              {/if}</li>
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
  .change-error{display:flex;align-items:center;gap:var(--space-2);flex-wrap:wrap;color:var(--accent-red)}
  .decision-actions{display:flex;justify-content:flex-start;gap:var(--space-2);flex-wrap:wrap}
  .source-context{padding:var(--space-3);border:1px solid var(--border-muted);border-radius:var(--radius-md);background:var(--bg-raised);white-space:pre-wrap;overflow-wrap:anywhere}
  .source-workspace{display:grid;gap:var(--space-3)}.source-viewer-column{min-width:0}.source-workspace.has-selection{grid-template-columns:minmax(0,1fr) minmax(260px,340px)}
  .selected-region{display:grid;gap:var(--space-2);align-self:start;padding:var(--space-3);border:1px solid var(--border-default);border-radius:var(--radius-md);background:var(--bg-raised)}
  @media(max-width:900px){.source-workspace.has-selection{grid-template-columns:minmax(0,1fr)}}
  label{display:grid;gap:var(--space-1);font-size:var(--font-size-xs);color:var(--text-secondary)}
  textarea,input{width:100%;box-sizing:border-box;padding:var(--space-2);border:1px solid var(--border-muted);border-radius:var(--radius-md);background:var(--bg-raised);color:var(--text-primary);font:inherit}
  mark{color:var(--text-primary);background:color-mix(in srgb,var(--accent-amber) 30%,var(--bg-raised))}
</style>
