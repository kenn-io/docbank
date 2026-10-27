<script lang="ts">
  import * as generated from "./generated/docbank.js";
  import { untrack } from "svelte";
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import HistoryIcon from "@lucide/svelte/icons/history";
  import { Chip, EmptyState, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { type ContentVersion, type ContentVersionPage, type Node } from "./generated/docbank.js";
  import DownloadButton from "./DownloadButton.svelte";
  import DrawerShell from "./DrawerShell.svelte";
  import EmailPDFButton from "./EmailPDFButton.svelte";
  import InspectorDetails from "./InspectorDetails.svelte";
  import { basename, formatBytes, formatDate } from "./format.js";

  interface Props {
    session: string;
    node: Node;
    path: string;
    pinnedVersion?: ContentVersion;
    resolving?: boolean;
    unavailable?: string;
    onselect?: (version: ContentVersion) => void;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let {
    session,
    node,
    path,
    pinnedVersion,
    resolving = false,
    unavailable = "",
    onselect,
    onclose,
    onauthfailure,
  }: Props = $props();

  let page = $state<ContentVersionPage | null>(null);
  let opened = $state<Set<string>>(new Set());
  let loading = $state(true);
  let error = $state("");
  let generation = 0;

  // A link that is still resolving or failed offers no download, so the list can't stand in for it.
  const locked = $derived(Boolean(onselect) && (resolving || unavailable !== ""));
  // A linked version stays open; otherwise each version opens on its own.
  const pinnedID = $derived(
    onselect && !resolving && !unavailable ? (pinnedVersion?.id ?? "") : "",
  );

  // A linked version older than the loaded history still shows, first.
  const listed = $derived(
    page
      ? [
          ...(pinnedVersion && pinnedID && !page.items.some((item) => item.id === pinnedID)
            ? [pinnedVersion]
            : []),
          ...page.items,
        ]
      : [],
  );

  const timelineKey = $derived(`${session}:${node.id}:${onselect ? node.current_version_id : ""}`);

  $effect(() => {
    void timelineKey;
    untrack(() => void load());
    return () => {
      generation += 1;
    };
  });

  async function load(): Promise<void> {
    const request = ++generation;
    loading = true;
    error = "";
    try {
      const next = await generated.listContentVersions(
        node.id,
        { limit: 1000, offset: 0 },
        { session },
      );
      if (request !== generation) return;
      page = next;
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        if (!onselect) onclose();
        return;
      }
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  function isOpen(version: ContentVersion): boolean {
    return onselect ? version.id === pinnedID : opened.has(version.id);
  }

  function toggle(version: ContentVersion): void {
    if (onselect) {
      onselect(version);
      return;
    }
    const next = new Set(opened);
    if (!next.delete(version.id)) next.add(version.id);
    opened = next;
  }

  function label(kind: ContentVersion["transition_kind"]): string {
    switch (kind) {
      case "content_create":
        return "Created";
      case "content_replace":
        return "Replaced";
      case "content_revert":
        return "Restored";
    }
  }

  function facts(version: ContentVersion) {
    return [
      { label: "Version ID", value: version.id, copy: true },
      { label: "SHA-256", value: version.blob_hash, copy: true },
      { label: "Type", value: version.mime_type || "application/octet-stream" },
      { label: "Restored from", value: version.source_version_id, copy: true },
      { label: "Operation ID", value: version.introduced_operation_id, copy: true },
    ];
  }
</script>

<DrawerShell
  title={basename(path)}
  ariaLabel={`Version history for ${path}`}
  closeLabel="Close version history"
  {onclose}
>
  <div class="versions">
    <p class="summary">
      <span class="path">{path}</span>
      {#if page}<span>{page.total} version{page.total === 1 ? "" : "s"}</span>{/if}
      {#if node.trashed_at}<Chip size="xs" tone="warning">Trash</Chip>{/if}
    </p>
    {#if node.trashed_at}
      <p class="note" role="note">Restore this file before downloading versions or email PDFs.</p>
    {/if}
    {#if resolving}
      <p class="status"><Spinner size={16} /> Loading linked version…</p>
    {:else if unavailable}
      <p class="error" role="alert">{unavailable}</p>
    {/if}

    {#if loading}
      <div class="status"><Spinner size={16} /> Loading versions…</div>
    {:else if error}
      <p class="error" role="alert">{error}</p>
    {:else if !page || page.items.length === 0}
      <EmptyState title="No versions">
        {#snippet icon()}<HistoryIcon size="22" />{/snippet}
      </EmptyState>
    {:else}
      <ol aria-label="Versions">
        {#each listed as version (version.id)}
          {@const expanded = isOpen(version)}
          <li class:selected={version.id === pinnedID}>
            <div class="row">
              <div class="what">
                <strong>{label(version.transition_kind)}</strong>
                <span class="meta"
                  >{formatDate(version.recorded_at)} · {formatBytes(version.size)}</span
                >
              </div>
              {#if version.id === page.items[0]?.id}
                <Chip size="xs" tone="success" uppercase={false}>Current</Chip>
              {/if}
              {#if !node.trashed_at && !locked}
                <DownloadButton {session} {node} {version} surface="soft" {onauthfailure} />
              {/if}
            </div>
            <button
              type="button"
              class="disclosure"
              aria-expanded={expanded}
              aria-label={`Details for ${label(version.transition_kind)} ${formatDate(version.recorded_at)}`}
              onclick={() => toggle(version)}
            >
              {#if expanded}<ChevronDownIcon size="14" aria-hidden="true" />{:else}<ChevronRightIcon
                  size="14"
                  aria-hidden="true"
                />{/if}Details
            </button>
            {#if expanded}
              <div class="details">
                <InspectorDetails label="Version details" facts={facts(version)} />
                {#if !node.trashed_at && version.mime_type === "message/rfc822"}
                  <EmailPDFButton {session} {node} {version} {onauthfailure} />
                {/if}
              </div>
            {/if}
          </li>
        {/each}
      </ol>
      {#if page.total > page.items.length}
        <p class="note">
          Showing the newest {page.items.length} of {page.total} versions. Use the CLI or HTTP API for
          older history.
        </p>
      {/if}
    {/if}
  </div>
</DrawerShell>

<style>
  .versions {
    display: grid;
    align-content: start;
    gap: var(--space-5);
    padding: var(--space-5);
    font-size: var(--font-size-sm);
  }
  p {
    margin: 0;
  }
  .summary {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-3);
    color: var(--text-muted);
  }
  .path {
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .note {
    color: var(--text-muted);
  }
  .status {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
  }
  .error {
    color: var(--accent-red);
  }
  ol {
    display: grid;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  li {
    display: grid;
    gap: var(--space-3);
    justify-items: start;
    padding: var(--space-4) var(--space-3);
    border-bottom: 1px solid var(--border-muted);
  }
  li.selected {
    background: var(--selection-bg);
  }
  .row {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }
  .what {
    display: grid;
    flex: 1;
    gap: var(--space-1);
    min-width: 0;
  }
  .what strong {
    color: var(--text-primary);
    font-weight: var(--font-weight-medium);
  }
  .meta {
    color: var(--text-muted);
  }
  .disclosure {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: 0;
    border: 0;
    background: none;
    color: var(--text-muted);
    cursor: pointer;
    font: inherit;
  }
  .disclosure:hover {
    color: var(--text-primary);
  }
  .disclosure:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 2px;
  }
  .details {
    width: 100%;
    min-width: 0;
  }
  .details :global(.inspector-details) {
    grid-template-columns: 8rem minmax(0, 1fr);
  }
</style>
