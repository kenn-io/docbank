<script lang="ts">
  import { untrack } from "svelte";
  import { Button } from "@kenn-io/kit-ui";
  import InspectorHeader from "./InspectorHeader.svelte";
  import InspectorDetails from "./InspectorDetails.svelte";
  import TechnicalDetails from "./TechnicalDetails.svelte";
  import AuditHistoryDrawer from "./AuditHistoryDrawer.svelte";
  import { getNode, auditStatus, type Node, type PhotoBrowseRow } from "./generated/docbank.js";
  import { APIError } from "./api-transport.js";
  import { formatBytes, formatDate } from "./format.js";
  import { photoFacts } from "./photoFacts.js";
  let {
    photo,
    session,
    onauthfailure,
    compact = false,
  }: {
    photo: PhotoBrowseRow;
    session: string;
    onauthfailure: (cause: unknown) => void;
    compact?: boolean;
  } = $props();
  let node = $state<Node>();
  let protectedItem = $state(false);
  let historyOpen = $state(false);
  let error = $state("");
  let retry = $state(0);
  const detailsKey = $derived(
    `${session}:${photo.node_id}:${photo.content_version_id}:${photo.revision}`,
  );

  $effect(() => {
    void detailsKey;
    const selected = untrack(() => ({
      nodeID: photo.node_id,
      versionID: photo.content_version_id,
    }));
    const activeSession = untrack(() => session);
    void retry;
    node = undefined;
    protectedItem = false;
    historyOpen = false;
    error = "";
    if (compact) return;
    const controller = new AbortController();
    const options = { session: activeSession, signal: controller.signal };
    void getNode(selected.nodeID, options)
      .then((current) => {
        if (controller.signal.aborted) return;
        if (current.id !== selected.nodeID || current.current_version_id !== selected.versionID) {
          error = "This file changed. Reopen the photo to refresh its details.";
          return;
        }
        node = current;
      })
      .catch((cause) => {
        if (controller.signal.aborted) return;
        if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
        else error = "File details unavailable.";
      });
    void auditStatus({ node_id: selected.nodeID }, options)
      .then((status) => {
        if (!controller.signal.aborted) protectedItem = status.membership?.protected ?? false;
      })
      .catch((cause) => {
        if (!controller.signal.aborted && cause instanceof APIError && cause.status === 401)
          onauthfailure(cause);
      });
    return () => controller.abort();
  });
</script>

<section class="photo-details" aria-label="Photo file details">
  {#if !compact}<InspectorHeader
      name={photo.name}
      mime={photo.media_type}
      size={node?.size}
      protected={protectedItem && !!node}
      onhistory={() => (historyOpen = true)}
    />{/if}
  {#if error}<div class="details-error" role="status">
      {error}<Button size="sm" onclick={() => retry++}>Retry details</Button>
    </div>{/if}
  <InspectorDetails
    facts={[
      { label: "Path", value: node?.path, copy: true },
      { label: "Modified", value: node ? formatDate(node.modified_at) : undefined },
      { label: "Size", value: node ? formatBytes(node.size) : undefined },
      ...photoFacts(photo)
        .slice(0, compact ? 5 : undefined)
        .filter(([label]) => label !== "Format")
        .map(([label, value]) => ({ label: String(label), value })),
    ]}
  />
  {#if !compact}
    <TechnicalDetails
      facts={[
        { label: "Type", value: photo.media_type.split(";")[0]?.trim(), mono: false },
        { label: "Version", value: photo.content_version_id, copy: true },
        { label: "SHA-256", value: node?.blob_hash, copy: true },
        { label: "ID", value: photo.node_id, copy: true },
        { label: "Photo ID", value: photo.asset_id, copy: true },
        { label: "File ID", value: photo.display_file_id, copy: true },
      ]}
    />
  {/if}
</section>
{#if historyOpen && node}<div
    role="presentation"
    onkeydown={(event) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      event.stopPropagation();
      historyOpen = false;
    }}
  >
    <AuditHistoryDrawer
      {session}
      {node}
      path={node.path ?? photo.name}
      {onauthfailure}
      onclose={() => (historyOpen = false)}
    />
  </div>{/if}

<style>
  .photo-details {
    display: grid;
    gap: var(--space-5);
    min-width: 0;
  }
  .details-error {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
</style>
