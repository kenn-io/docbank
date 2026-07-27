<script lang="ts">
  import { onMount } from "svelte";
  import HistoryIcon from "@lucide/svelte/icons/history";
  import XIcon from "@lucide/svelte/icons/x";
  import {
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
    contentVersions,
    type ContentVersion,
    type ContentVersionPage,
    type Node,
  } from "./api.js";
  import DownloadButton from "./DownloadButton.svelte";
  import { basename, formatBytes, formatDate } from "./format.js";

  interface Props {
    session: string;
    node: Node;
    path: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, node, path, onclose, onauthfailure }: Props = $props();

  let page = $state<ContentVersionPage | null>(null);
  let selectedVersionID = $state("");
  let loading = $state(true);
  let error = $state("");
  let generation = 0;

  const selectedVersion = $derived(
    page?.items.find((version) => version.id === selectedVersionID),
  );

  onMount(() => {
    void load();
    return () => {
      generation += 1;
    };
  });

  async function load(): Promise<void> {
    const request = ++generation;
    loading = true;
    error = "";
    try {
      const next = await contentVersions(session, node.id);
      if (request !== generation) return;
      page = next;
      selectedVersionID = next.items[0]?.id ?? "";
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        onclose();
        return;
      }
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  function transitionLabel(kind: ContentVersion["transition_kind"]): string {
    switch (kind) {
      case "content_create":
        return "Created";
      case "content_replace":
        return "Replaced";
      case "content_revert":
        return "Reverted";
    }
  }

  function transitionTone(kind: ContentVersion["transition_kind"]): TimelineTone {
    switch (kind) {
      case "content_create":
        return "success";
      case "content_replace":
        return "info";
      case "content_revert":
        return "warning";
    }
  }
</script>

<DetailDrawer
  width="min(920px, 100vw)"
  ariaLabel={`Immutable version history for ${path}`}
  {onclose}
>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>IMMUTABLE VERSION HISTORY</span>
        <strong>{basename(path)}</strong>
        <code>{path} · id:{node.id}</code>
      </div>
      <IconButton size="sm" ariaLabel="Close version history" onclick={onclose}>
        <XIcon size="14" aria-hidden="true" />
      </IconButton>
    </div>
  {/snippet}

  <div class="history-shell">
    <section class="version-list" aria-label="Retained versions">
      <div class="section-heading">
        <span>NEWEST FIRST</span>
        <strong>{page?.total ?? 0} retained version{page?.total === 1 ? "" : "s"}</strong>
      </div>

      {#if loading}
        <div class="loading"><Spinner size={16} /> Loading immutable versions…</div>
      {:else if error}
        <p class="error" role="alert">{error}</p>
      {:else if !page || page.items.length === 0}
        <EmptyState
          title="No retained versions"
          description="This file does not have an immutable content version."
        >
          {#snippet icon()}<HistoryIcon size="22" />{/snippet}
        </EmptyState>
      {:else}
        <Timeline ariaLabel="Immutable content versions">
          {#each page.items as version (version.id)}
            <TimelineItem tone={transitionTone(version.transition_kind)}>
              <Card
                level="default"
                padding="sm"
                selected={version.id === selectedVersionID}
                onclick={() => (selectedVersionID = version.id)}
                ariaLabel={`${transitionLabel(version.transition_kind)} at ${formatDate(version.recorded_at)}`}
                eyebrow={transitionLabel(version.transition_kind)}
                meta={formatDate(version.recorded_at)}
              >
                <div class="version-summary">
                  <strong>Revision {version.node_revision}</strong>
                  {#if version.id === page.items[0]?.id}
                    <Chip size="xs" tone="success" dot>Current</Chip>
                  {/if}
                </div>
                <p>{formatBytes(version.size)} · <code>{version.id.slice(0, 8)}</code></p>
              </Card>
            </TimelineItem>
          {/each}
        </Timeline>
        {#if page.total > page.items.length}
          <p class="limit-note">
            Showing the newest {page.items.length} of {page.total} versions.
            Use the CLI or HTTP API for older history.
          </p>
        {/if}
      {/if}
    </section>

    <section class="version-detail" aria-label="Complete version authority">
      {#if selectedVersion}
        <Card
          level="inset"
          eyebrow={transitionLabel(selectedVersion.transition_kind)}
          eyebrowTone={transitionTone(selectedVersion.transition_kind)}
          title="Complete version authority"
          meta={`Revision ${selectedVersion.node_revision}`}
        >
          <dl>
            <div class="identity">
              <dt>Version</dt>
              <dd>
                <code>{selectedVersion.id}</code>
                <CopyButton text={selectedVersion.id} ariaLabel="Copy version ID" />
              </dd>
            </div>
            <div><dt>Transition</dt><dd>{transitionLabel(selectedVersion.transition_kind)}</dd></div>
            <div><dt>Recorded</dt><dd>{formatDate(selectedVersion.recorded_at)}</dd></div>
            <div><dt>Canonical time</dt><dd><code>{selectedVersion.recorded_at}</code></dd></div>
            <div><dt>Node</dt><dd>id:{selectedVersion.node_id}</dd></div>
            <div><dt>Node revision</dt><dd>{selectedVersion.node_revision}</dd></div>
            <div class="identity">
              <dt>SHA-256</dt>
              <dd>
                <code>{selectedVersion.blob_hash}</code>
                <CopyButton text={selectedVersion.blob_hash} ariaLabel="Copy SHA-256" />
              </dd>
            </div>
            <div>
              <dt>Size</dt>
              <dd>{formatBytes(selectedVersion.size)} ({selectedVersion.size} bytes)</dd>
            </div>
            <div>
              <dt>Media type</dt>
              <dd>{selectedVersion.mime_type || "application/octet-stream"}</dd>
            </div>
            <div class="identity">
              <dt>Operation</dt>
              <dd>
                <code>{selectedVersion.introduced_operation_id}</code>
                <CopyButton
                  text={selectedVersion.introduced_operation_id}
                  ariaLabel="Copy introducing operation ID"
                />
              </dd>
            </div>
            {#if selectedVersion.source_version_id}
              <div class="identity">
                <dt>Revert source</dt>
                <dd>
                  <code>{selectedVersion.source_version_id}</code>
                  <CopyButton text={selectedVersion.source_version_id} ariaLabel="Copy revert source version ID" />
                </dd>
              </div>
            {/if}
          </dl>
          <div class="version-actions">
            <DownloadButton
              {session}
              {node}
              version={selectedVersion}
              label="Download this version"
              {onauthfailure}
            />
          </div>
        </Card>
      {:else}
        <EmptyState
          title="Select a version"
          description="Choose a timeline entry to inspect its stable identity and complete content authority."
        >
          {#snippet icon()}<HistoryIcon size="22" />{/snippet}
        </EmptyState>
      {/if}
    </section>
  </div>
</DetailDrawer>

<style>
  .drawer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }

  .drawer-heading > div,
  .section-heading {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .drawer-heading span,
  .section-heading span {
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
    grid-template-columns: minmax(290px, 0.8fr) minmax(360px, 1.2fr);
    min-height: 100%;
  }

  .version-list,
  .version-detail {
    min-width: 0;
    padding: var(--space-5);
  }

  .version-list {
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

  .version-summary {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .version-summary strong {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .version-summary + p {
    margin: 0;
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

  .limit-note {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
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

  .version-actions {
    margin-top: var(--space-5);
    padding-top: var(--space-4);
    border-top: 1px solid var(--border-default);
  }

  code {
    min-width: 0;
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-size: var(--font-size-xs);
  }

  @media (max-width: 760px) {
    .history-shell {
      grid-template-columns: 1fr;
    }

    .version-list {
      border-right: 0;
      border-bottom: 1px solid var(--border-default);
    }
  }
</style>
