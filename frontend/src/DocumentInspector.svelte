<script lang="ts">
  import type { Snippet } from "svelte";
  import * as api from "./generated/docbank.js";
  import "./inspector.css";
  import {
    Button,
    Chip,
    ChipStack,
    Menu,
    MenuTrigger,
    MenuContent,
    MenuItem,
    Modal,
    Spinner,
  } from "@kenn-io/kit-ui";
  import XIcon from "@lucide/svelte/icons/x";
  import EllipsisIcon from "@lucide/svelte/icons/ellipsis";
  import AppTooltip from "./AppTooltip.svelte";
  import InspectorHeader from "./InspectorHeader.svelte";
  import InspectorDetails from "./InspectorDetails.svelte";
  import TechnicalDetails from "./TechnicalDetails.svelte";
  import InspectorHistory from "./InspectorHistory.svelte";
  import CopyValue from "./CopyValue.svelte";
  import DownloadButton from "./DownloadButton.svelte";
  import type { SelectedSource } from "./selectedSource.js";
  import TagLabel from "./TagLabel.svelte";
  import type { Node, Tag } from "./generated/docbank.js";
  import type { SnapshotRow } from "./snapshots.js";
  import type { SnapshotReceiptOverlay } from "./snapshotOverlays.js";
  import { basename, formatBytes, formatDate } from "./format.js";
  import IconAction from "./IconAction.svelte";
  import Trash2Icon from "@lucide/svelte/icons/trash-2";

  let {
    node,
    path,
    snapshot,
    collectionName,
    snapshotID,
    overlay,
    protected: protectedItem = false,
    tags,
    tagsLoading,
    tagsError,
    tagsDisabled = false,
    tagsDisabledReason = "",
    actionsDisabled = false,
    trashing = false,
    previewAvailable = false,
    preview,
    session,
    source,
    authorizationRevision = 0,
    onauthfailure,
    onopen,
    onversions,
    onprovenance,
    onprocess,
    profiles = [],
    ontrash,
    onhistory,
    ontags,
    onremovetag,
  }: {
    node?: Node;
    path: string;
    snapshot?: SnapshotRow;
    collectionName?: string;
    snapshotID?: string;
    overlay?: SnapshotReceiptOverlay;
    protected?: boolean;
    tags: Tag[];
    tagsLoading: boolean;
    tagsError: string;
    tagsDisabled?: boolean;
    tagsDisabledReason?: string;
    actionsDisabled?: boolean;
    trashing?: boolean;
    previewAvailable?: boolean;
    preview?: Snippet<[(source: SelectedSource, revision: number) => void]>;
    session?: string;
    source?: SelectedSource;
    authorizationRevision?: number;
    onauthfailure?: (cause: unknown) => void;
    onopen?: () => void;
    onversions?: () => void;
    onprovenance: () => void;
    onprocess?: () => void;
    profiles?: api.ProcessingProfileSummary[];
    ontrash?: () => void;
    onhistory: () => void;
    ontags?: () => void;
    onremovetag?: (tag: Tag) => void;
  } = $props();
  let protect = $state(false);
  let displayed = $state<{ origin: string; source: SelectedSource; revision: number }>();
  const download = $derived(displayed?.origin === source?.key ? displayed : undefined);
  function showSource(next: SelectedSource, revision: number): void {
    if (source) displayed = { origin: source.key, source: next, revision };
  }
  // The media type without parameters such as "; charset=utf-8".
  function mediaType(value: string | undefined): string {
    return (value ?? "").split(";")[0]!.trim() || "application/octet-stream";
  }
  const name = $derived(snapshot?.name ?? basename(path));
  const kind = $derived(snapshot ? "file" : (node?.kind ?? "file"));
  // A result shows what it saved beside what the document is now only when they differ.
  const diverged = $derived.by(() => {
    if (!snapshot) return false;
    if (overlay && Object.keys(overlay.assignments).length > 0) return true;
    if (node && (node.path !== snapshot.path || node.name !== snapshot.name)) return true;
    if (tagsLoading || tagsError) return false;
    const saved = new Set(snapshot.tags.map((tag) => tag.id));
    return tags.length !== saved.size || tags.some((tag) => !saved.has(tag.id));
  });
  // The dialog checks which profiles accept the file.
  const canExtract = $derived(profiles.some((profile) => profile.rendition));
  const mime = $derived(snapshot?.mime_type ?? node?.mime_type);
  const size = $derived(snapshot?.size ?? node?.size);
  // Browser sessions exclude audit preview/enable. Enrollment stays in the CLI.
  const protectCommand = $derived(`docbank audit enable --node-id ${node?.id}`);
  $effect(() => {
    void path;
    void snapshotID;
    protect = false;
  });
</script>

{#snippet actions()}
  {#if previewAvailable && source && session && onauthfailure}
    <DownloadButton
      {session}
      source={download?.source ?? source}
      authorizationRevision={download?.revision ?? authorizationRevision}
      {onauthfailure}
    />
  {/if}
  {#if onopen}<Button size="sm" tone="info" surface="solid" onclick={onopen}>Open</Button>{/if}
  {#if onprocess && profiles.length > 0}<AppTooltip
      text={actionsDisabled ? "Refresh this view first" : ""}
      ><Button size="sm" surface="soft" disabled={actionsDisabled} onclick={onprocess}
        >{canExtract ? "Extract text" : "Index for search"}</Button
      ></AppTooltip
    >{/if}
  {#if kind === "dir" && node && !protectedItem}<Menu align="end">
      <AppTooltip text="Folder actions"
        ><MenuTrigger ariaLabel="Document actions" title=""><EllipsisIcon size={16} /></MenuTrigger
        ></AppTooltip
      >
      <MenuContent ariaLabel="Document actions">
        <MenuItem onselect={() => (protect = true)}>Protect permanently…</MenuItem>
        {#if ontrash}<MenuItem
            tone="danger"
            disabled={actionsDisabled || trashing}
            onselect={ontrash}>Move to trash</MenuItem
          >{/if}
      </MenuContent>
    </Menu>{:else if ontrash}<IconAction
      ariaLabel="Move to trash"
      tip={actionsDisabled ? "Refresh this view first" : "Move to trash"}
      disabled={actionsDisabled || trashing}
      onclick={ontrash}><Trash2Icon size={16} aria-hidden="true" /></IconAction
    >{/if}
{/snippet}

<section
  class="document-inspector inspector-sections"
  aria-label={`${kind === "dir" ? "Folder" : "Document"} details for ${name}`}
>
  <InspectorHeader {name} {kind} {mime} {size} protected={protectedItem} {onhistory} {actions} />
  {#if previewAvailable && preview}{@render preview(showSource)}{/if}
  {#if actionsDisabled && !snapshot}<p role="status">
      Refresh the current view before using document actions.
    </p>{/if}
  <div class="detail-rows">
    <InspectorDetails
      facts={[
        { label: "Path", value: path, copy: true },
        { label: "Modified", value: formatDate(snapshot?.modified_at ?? node?.modified_at ?? "") },
        ...(kind === "file" ? [{ label: "Size", value: formatBytes(size ?? 0) }] : []),
        ...(snapshot
          ? [
              {
                label: "Import",
                value:
                  snapshot.display_collection_label ??
                  collectionName ??
                  (snapshot.display_collection_id
                    ? `Import ${snapshot.display_collection_id.slice(0, 8)}`
                    : undefined),
              },
            ]
          : []),
      ]}
    />
    {#if snapshot && diverged}
      <section class="tags-row" aria-label="Saved tags">
        <span>Saved tags</span>
        <div class="tag-values">
          {#each snapshot.tags as tag (tag.id)}<Chip size="sm" uppercase={false}>{tag.name}</Chip
            >{:else}<span class="muted">None</span>{/each}
        </div>
      </section>
      {#if overlay}<section class="tags-row" aria-label="Tag changes">
          <span>Tag changes</span>
          <div class="tag-values">
            {#each Object.entries(overlay.assignments) as [tagID, assignment] (tagID)}<Chip
                size="sm"
                tone={assignment.assign ? "success" : "muted"}
                uppercase={false}
                >{assignment.assign ? "Added" : "Removed"}: {assignment.label}</Chip
              >{/each}
          </div>
        </section>{/if}
    {:else}
      {@render currentTags()}
    {/if}
    <TechnicalDetails
      facts={[
        ...(kind === "file" ? [{ label: "Type", value: mediaType(mime), mono: false }] : []),
        ...(kind === "file"
          ? [
              {
                label: "Version",
                value: snapshot?.content_version_id ?? node?.current_version_id,
                copy: true,
              },
              { label: "SHA-256", value: snapshot?.blob_hash ?? node?.blob_hash, copy: true },
            ]
          : []),
        { label: "ID", value: snapshot?.node_id ?? node?.id, copy: true },
        ...(snapshot ? [{ label: "Result ID", value: snapshotID, copy: true }] : []),
      ]}
    />
  </div>
  {#if snapshot && diverged}
    <section class="current-details" aria-label="Current details">
      <h3>Current details</h3>
      {#if node}<InspectorDetails
          label="Current file"
          facts={[
            { label: "Path", value: node.path, copy: true },
            { label: "Name", value: node.name },
          ]}
        />{/if}
      {@render currentTags()}
    </section>
  {/if}
  {#if kind === "file" && node && session && onauthfailure}
    <InspectorHistory
      {session}
      {node}
      onversions={actionsDisabled ? undefined : onversions}
      {onprovenance}
      {onauthfailure}
    />
  {/if}
</section>

{#snippet currentTags()}
  <div class="tags-row">
    <span>Tags</span>
    <div class="tag-values">
      {#if tagsLoading}<Spinner size={13} />{:else if tagsError}<span role="status"
          >{tagsError}</span
        >{:else if tags.length}
        <ChipStack
          items={tags}
          key={(tag) => tag.id}
          maxVisible={6}
          size="sm"
          ariaLabel={snapshot ? "Current tags" : "Assigned tags"}
        >
          {#snippet chip(tag)}
            <span class="assigned-tag">
              <TagLabel {tag} size="sm" />
              {#if onremovetag}<button
                  type="button"
                  class="remove-tag"
                  aria-label={`Remove tag ${tag.name}`}
                  disabled={tagsDisabled}
                  onclick={() => onremovetag?.(tag)}><XIcon size={12} aria-hidden="true" /></button
                >{/if}
            </span>
          {/snippet}
        </ChipStack>
      {/if}
      {#if ontags}<AppTooltip text={tagsDisabled ? tagsDisabledReason : ""}
          ><Button size="sm" surface="soft" disabled={tagsDisabled} onclick={ontags}
            >{tags.length ? "Manage" : "Add tag"}</Button
          ></AppTooltip
        >{:else if !tagsLoading && !tagsError && !tags.length}<span class="muted">None</span>{/if}
    </div>
  </div>
{/snippet}

{#if protect && node}
  <Modal title="Protect permanently" onclose={() => (protect = false)}>
    <p>Permanent protection must be enabled from the command line. Preview this folder:</p>
    <CopyValue label="protection command" value={protectCommand} />
    <p>
      The preview includes retained versions and vault-wide metadata. Enabling protection can't be
      undone.
    </p>
    {#snippet footer()}<Button onclick={() => (protect = false)}>Close</Button>{/snippet}
  </Modal>
{/if}

<style>
  .assigned-tag {
    position: relative;
    display: inline-flex;
    min-width: 0;
  }
  .remove-tag {
    position: absolute;
    right: -6px;
    top: -6px;
    display: grid;
    place-items: center;
    width: 20px;
    height: 20px;
    padding: 0;
    border: 1px solid var(--border-default);
    border-radius: 50%;
    background: var(--bg-surface);
    color: var(--text-primary);
    cursor: pointer;
    opacity: 0;
  }
  .assigned-tag:hover .remove-tag,
  .remove-tag:focus-visible {
    opacity: 1;
  }
  .remove-tag:disabled {
    cursor: default;
  }
  @media (hover: none) {
    .remove-tag {
      opacity: 1;
    }
  }
  .document-inspector {
    padding: var(--space-6);
  }
  .detail-rows {
    display: grid;
    gap: var(--space-4);
    min-width: 0;
  }
  .tags-row {
    display: grid;
    grid-template-columns: 76px minmax(0, 1fr);
    gap: var(--space-3);
    align-items: baseline;
    font-size: var(--font-size-sm);
  }
  .tags-row > span,
  .muted {
    color: var(--text-muted);
  }
  .tag-values {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-4);
    min-width: 0;
  }
  .current-details {
    display: grid;
    gap: var(--space-4);
    padding-top: var(--space-4);
    border-top: 1px solid var(--border-muted);
  }
  h3 {
    margin: 0;
    font-size: var(--font-size-sm);
    font-weight: 500;
  }
  p {
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }
</style>
