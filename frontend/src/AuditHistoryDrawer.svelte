<script lang="ts">
  import { onMount } from "svelte";
  import ShieldCheckIcon from "@lucide/svelte/icons/shield-check";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Card,
    Chip,
    CopyButton,
    DetailDrawer,
    EmptyState,
    IconButton,
    Spinner,
    Timeline,
    TimelineItem,
    type TimelineTone,
  } from "@kenn-io/kit-ui";
  import {
    APIError,
    auditHistory,
    type AuditAttachmentState,
    type AuditEvent,
    type AuditEventPage,
    type Node,
  } from "./api.js";
  import {
    auditEventLabel,
    auditEventSummary,
    auditPathLabel,
  } from "./audit.js";
  import { basename, formatDate } from "./format.js";

  interface Props {
    session: string;
    node: Node;
    path: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, node, path, onclose, onauthfailure }: Props = $props();

  let page = $state<AuditEventPage | null>(null);
  let items = $state<AuditEvent[]>([]);
  let selectedEventID = $state("");
  let loading = $state(true);
  let loadingOlder = $state(false);
  let error = $state("");
  let generation = 0;

  const selectedEvent = $derived(
    items.find((event) => event.id === selectedEventID),
  );
  const historyNode = $derived(page?.node ?? node);
  const historyPath = $derived(page ? page.path : path);
  const historyLabel = $derived(
    historyPath ? basename(historyPath) : `${historyNode.name} (trashed)`,
  );
  const historyCoordinate = $derived(
    historyPath ?? `Trashed node · id:${historyNode.id}`,
  );

  onMount(() => {
    void loadPage("", false);
    return () => {
      generation += 1;
    };
  });

  async function loadPage(cursor: string, append: boolean): Promise<void> {
    const request = ++generation;
    if (append) loadingOlder = true;
    else loading = true;
    error = "";
    try {
      const next = await auditHistory(session, node.id, cursor);
      if (request !== generation) return;
      page = next;
      items = append ? [...items, ...next.items] : next.items;
      if (!selectedEventID) selectedEventID = items[0]?.id ?? "";
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        onclose();
        return;
      }
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) {
        loading = false;
        loadingOlder = false;
      }
    }
  }

  function eventTone(kind: string): TimelineTone {
    if (kind.startsWith("audit_")) return "success";
    if (kind.startsWith("content_")) return "info";
    if (kind.startsWith("tag_")) return "workspace";
    if (kind.startsWith("provenance_")) return "merged";
    if (kind === "node_path") return "warning";
    return "neutral";
  }

  function optional(value: string | number | undefined): string {
    return value === undefined || value === "" ? "—" : String(value);
  }
</script>

<DetailDrawer
  width="min(980px, 100vw)"
  ariaLabel={`Permanent audit history for ${historyCoordinate}`}
  {onclose}
>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>PERMANENT AUDIT HISTORY</span>
        <strong>{historyLabel}</strong>
        <code>{historyCoordinate}</code>
      </div>
      <div class="drawer-actions">
        <Chip tone="success" size="sm" dot>Protected</Chip>
        <IconButton size="sm" ariaLabel="Close audit history" onclick={onclose}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    </div>
  {/snippet}

  <div class="history-shell">
    <section class="event-list" aria-label="Recorded events">
      <div class="section-heading">
        <div>
          <span>NEWEST FIRST</span>
          <strong>{page?.total ?? 0} recorded event{page?.total === 1 ? "" : "s"}</strong>
        </div>
      </div>

      {#if loading}
        <div class="loading"><Spinner size={16} /> Loading permanent history…</div>
      {:else if error && items.length === 0}
        <p class="error" role="alert">{error}</p>
      {:else if items.length === 0}
        <EmptyState
          title="No events on this page"
          description="The document remains permanently protected even when a page is empty."
        >
          {#snippet icon()}<ShieldCheckIcon size="22" />{/snippet}
        </EmptyState>
      {:else}
        {#if error}
          <p class="error" role="alert">{error}</p>
        {/if}
        <Timeline ariaLabel="Permanent document history">
          {#each items as event (event.id)}
            <TimelineItem tone={eventTone(event.kind)}>
              <Card
                level="default"
                padding="sm"
                selected={event.id === selectedEventID}
                onclick={() => (selectedEventID = event.id)}
                ariaLabel={`${auditEventLabel(event.kind)} at ${formatDate(event.recorded_at)}`}
                eyebrow={auditEventLabel(event.kind)}
                meta={formatDate(event.recorded_at)}
              >
                <p class="event-summary">{auditEventSummary(event)}</p>
                <p class="event-origin">
                  Operation {event.operation_sequence}.{event.ordinal} · {event.origin}
                </p>
              </Card>
            </TimelineItem>
          {/each}
        </Timeline>
        {#if page?.next_cursor}
          <Button
            size="sm"
            disabled={loadingOlder}
            onclick={() => void loadPage(page?.next_cursor ?? "", true)}
          >
            {#if loadingOlder}<Spinner size={14} />{/if}
            Load older events
          </Button>
        {/if}
      {/if}
    </section>

    <section class="event-detail" aria-label="Complete audited event">
      {#if selectedEvent}
        <Card
          level="inset"
          eyebrow={auditEventLabel(selectedEvent.kind)}
          eyebrowTone={eventTone(selectedEvent.kind)}
          title="Complete event authority"
          meta={`${selectedEvent.operation_sequence}.${selectedEvent.ordinal}`}
        >
          <dl>
            <div class="identity">
              <dt>Event</dt>
              <dd><code>{selectedEvent.id}</code><CopyButton text={selectedEvent.id} ariaLabel="Copy event ID" /></dd>
            </div>
            <div class="identity">
              <dt>Operation</dt>
              <dd>
                <code>{selectedEvent.operation_id}</code>
                <CopyButton text={selectedEvent.operation_id} ariaLabel="Copy operation ID" />
              </dd>
            </div>
            <div class="identity">
              <dt>Scope</dt>
              <dd>
                <code>{selectedEvent.scope_id}</code>
                <CopyButton text={selectedEvent.scope_id} ariaLabel="Copy scope ID" />
              </dd>
            </div>
            <div><dt>Node</dt><dd>id:{selectedEvent.node_id}</dd></div>
            <div><dt>Recorded</dt><dd>{formatDate(selectedEvent.recorded_at)}</dd></div>
            <div><dt>Canonical time</dt><dd><code>{selectedEvent.recorded_at}</code></dd></div>
            <div><dt>Origin</dt><dd>{selectedEvent.origin}</dd></div>
            {#if selectedEvent.agent_label}
              <div><dt>Agent</dt><dd>{selectedEvent.agent_label}</dd></div>
            {/if}
            <div>
              <dt>Revision</dt>
              <dd>{selectedEvent.prior_node_revision} → {selectedEvent.resulting_node_revision}</dd>
            </div>
            {#if selectedEvent.old_path}
              <div><dt>Before path</dt><dd><code>{auditPathLabel(selectedEvent.old_path)}</code></dd></div>
            {/if}
            {#if selectedEvent.new_path}
              <div><dt>After path</dt><dd><code>{auditPathLabel(selectedEvent.new_path)}</code></dd></div>
            {/if}
            {#if selectedEvent.prior_current_version_id !== undefined}
              <div class="identity">
                <dt>Before version</dt>
                <dd>
                  <code>{optional(selectedEvent.prior_current_version_id)}</code>
                  {#if selectedEvent.prior_current_version_id}
                    <CopyButton text={selectedEvent.prior_current_version_id} ariaLabel="Copy prior version ID" />
                  {/if}
                </dd>
              </div>
            {/if}
            {#if selectedEvent.resulting_current_version_id !== undefined}
              <div class="identity">
                <dt>After version</dt>
                <dd>
                  <code>{optional(selectedEvent.resulting_current_version_id)}</code>
                  {#if selectedEvent.resulting_current_version_id}
                    <CopyButton text={selectedEvent.resulting_current_version_id} ariaLabel="Copy resulting version ID" />
                  {/if}
                </dd>
              </div>
            {/if}
            {#if selectedEvent.source_version_id}
              <div class="identity">
                <dt>Source version</dt>
                <dd>
                  <code>{selectedEvent.source_version_id}</code>
                  <CopyButton text={selectedEvent.source_version_id} ariaLabel="Copy source version ID" />
                </dd>
              </div>
            {/if}
            {#if selectedEvent.target_node_id}
              <div><dt>Target node</dt><dd>id:{selectedEvent.target_node_id}</dd></div>
            {/if}
            {#if selectedEvent.baseline_digest}
              <div class="identity">
                <dt>Baseline</dt>
                <dd>
                  <code>{selectedEvent.baseline_digest}</code>
                  <CopyButton text={selectedEvent.baseline_digest} ariaLabel="Copy baseline digest" />
                </dd>
              </div>
            {/if}
          </dl>

          {#if selectedEvent.attachment}
            <div class="attachment">
              <div class="attachment-heading">
                <span>ATTACHED METADATA</span>
                <Chip size="xs" tone="neutral">{selectedEvent.attachment.kind}</Chip>
              </div>
              <dl>
                {#if selectedEvent.attachment.identity.tag_id}
                  <div class="identity">
                    <dt>Tag ID</dt>
                    <dd>
                      <code>{selectedEvent.attachment.identity.tag_id}</code>
                      <CopyButton text={selectedEvent.attachment.identity.tag_id} ariaLabel="Copy tag ID" />
                    </dd>
                  </div>
                {/if}
                {#if selectedEvent.attachment.identity.node_id}
                  <div><dt>Attached node</dt><dd>id:{selectedEvent.attachment.identity.node_id}</dd></div>
                {/if}
                {#if selectedEvent.attachment.identity.provenance_id}
                  <div class="identity">
                    <dt>Provenance ID</dt>
                    <dd>
                      <code>{selectedEvent.attachment.identity.provenance_id}</code>
                      <CopyButton text={selectedEvent.attachment.identity.provenance_id} ariaLabel="Copy provenance ID" />
                    </dd>
                  </div>
                {/if}
              </dl>
              <div class="attachment-states">
                {@render attachmentState("Before", selectedEvent.attachment.before)}
                {@render attachmentState("After", selectedEvent.attachment.after)}
              </div>
            </div>
          {/if}
        </Card>
      {:else}
        <EmptyState
          title="Select an event"
          description="Choose a timeline entry to inspect its complete stable identities and before/after state."
        >
          {#snippet icon()}<ShieldCheckIcon size="22" />{/snippet}
        </EmptyState>
      {/if}
    </section>
  </div>
</DetailDrawer>

{#snippet attachmentState(label: string, state: AuditAttachmentState | undefined)}
  <Card level="inset" padding="sm" eyebrow={label} title={state ? "Present" : "Absent"}>
    {#if state}
      <dl>
        {#if state.tag_name}<div><dt>Tag name</dt><dd>{state.tag_name}</dd></div>{/if}
        {#if state.ingest_id}<div><dt>Ingest</dt><dd><code>{state.ingest_id}</code></dd></div>{/if}
        {#if state.original_path !== undefined}
          <div><dt>Original reference</dt><dd>{state.original_path ?? "(absent)"}</dd></div>
        {/if}
        {#if state.original_mtime !== undefined}
          <div><dt>Original modified</dt><dd>{state.original_mtime ?? "(absent)"}</dd></div>
        {/if}
        {#if state.supersedes !== undefined}
          <div><dt>Supersedes</dt><dd><code>{state.supersedes ?? "(none)"}</code></dd></div>
        {/if}
      </dl>
    {/if}
  </Card>
{/snippet}

<style>
  .drawer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }

  .drawer-heading > div:not(.drawer-actions),
  .section-heading > div {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .drawer-actions {
    display: flex;
    flex-direction: row;
    align-items: center;
    gap: var(--space-3);
    flex-shrink: 0;
  }

  .drawer-heading span,
  .section-heading span,
  .attachment-heading > span {
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

  .history-shell {
    display: grid;
    grid-template-columns: minmax(300px, 0.85fr) minmax(360px, 1.15fr);
    min-height: 100%;
  }

  .event-list,
  .event-detail {
    min-width: 0;
    padding: var(--space-5);
  }

  .event-list {
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

  .event-summary,
  .event-origin {
    margin: 0;
  }

  .event-summary {
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .event-origin {
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

  dl {
    display: grid;
    gap: var(--space-4);
    margin: 0;
  }

  dl > div {
    display: grid;
    grid-template-columns: 112px minmax(0, 1fr);
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

  .attachment {
    display: grid;
    gap: var(--space-4);
    margin-top: var(--space-5);
    padding-top: var(--space-5);
    border-top: 1px solid var(--border-default);
  }

  .attachment-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .attachment-states {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-3);
  }

  @media (max-width: 760px) {
    .history-shell {
      grid-template-columns: 1fr;
    }

    .event-list {
      border-right: 0;
      border-bottom: 1px solid var(--border-default);
    }

    .attachment-states {
      grid-template-columns: 1fr;
    }
  }
</style>
