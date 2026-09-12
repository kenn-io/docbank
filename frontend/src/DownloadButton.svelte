<script lang="ts">
  import { onDestroy } from "svelte";
  import DownloadIcon from "@lucide/svelte/icons/download";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, Chip, Spinner } from "@kenn-io/kit-ui";
  import { APIError, type ContentVersion, type Node } from "./api.js";
  import {
    offerPreparedDownload,
    cancelPreparedDownload,
    prepareCurrentDownload,
    prepareExactDownload,
    prepareVersionDownload,
    type PreparedDownload,
    type DownloadProgress,
  } from "./download.js";
  import { formatBytes } from "./format.js";
  import type { SelectedSource } from "./selectedSource.js";

  let {
    session,
    node,
    version,
    source,
    authorizationRevision,
    label = "Download",
    onauthfailure,
  }: {
    session: string;
    node?: Node;
    version?: ContentVersion;
    source?: SelectedSource;
    authorizationRevision?: number;
    label?: string;
    onauthfailure: (cause: unknown) => void;
  } = $props();

  let controller = $state<AbortController | null>(null);
  let progress = $state<DownloadProgress | null>(null);
  let outcome = $state("");
  let failed = $state(false);

  onDestroy(() => controller?.abort());

  const sourceIdentity = $derived(
    source?.key ?? `${node?.id ?? 0}:${version?.id ?? node?.current_version_id ?? ""}`,
  );

  $effect(() => {
    const identity = `${session}:${sourceIdentity}:${authorizationRevision ?? node?.revision ?? 0}`;
    return () => {
      void identity;
      const active = controller;
      controller = null;
      active?.abort();
      progress = null;
      outcome = "";
      failed = false;
    };
  });

  async function download(): Promise<void> {
    if (controller) {
      controller.abort();
      return;
    }
    const active = new AbortController();
    const activeSession = session;
    controller = active;
    progress = { received: 0, total: source?.size ?? version?.size ?? node?.size ?? 0 };
    outcome = "";
    failed = false;
    let prepared: PreparedDownload | undefined;
    try {
      const report = (next: DownloadProgress) => {
        if (controller === active) progress = next;
      };
      prepared = source
        ? await prepareExactDownload(
            activeSession, source, authorizationRevision ?? source.mutationRevision,
            "native", active.signal, report,
          )
        : version && node
          ? await prepareVersionDownload(activeSession, node, version, active.signal, report)
          : node
            ? await prepareCurrentDownload(activeSession, node, active.signal, report)
            : (() => { throw new Error("The selected document does not have download authority."); })();
      if (controller !== active) {
        await cancelPreparedDownload(activeSession, prepared).catch(() => undefined);
        return;
      }
      offerPreparedDownload(prepared);
      outcome = `Verified ${formatBytes(prepared.size)}; browser save started.`;
    } catch (cause) {
      if (controller !== active) return;
      if (cause instanceof DOMException && cause.name === "AbortError") {
        outcome = "Download cancelled; no file was published.";
      } else if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
      } else {
        failed = true;
        outcome = cause instanceof Error ? cause.message : String(cause);
      }
    } finally {
      if (controller === active) {
        controller = null;
        progress = null;
      }
    }
  }

  const percentage = $derived(
    progress && progress.total > 0
      ? Math.min(100, Math.round((progress.received * 100) / progress.total))
      : 0,
  );
</script>

<div class="download-control">
  <Button
    size="sm"
    tone={controller ? "danger" : "info"}
    surface={controller ? "soft" : "solid"}
    onclick={() => void download()}
  >
    {#if controller}
      <XIcon size="14" aria-hidden="true" />
      Cancel
    {:else}
      <DownloadIcon size="14" aria-hidden="true" />
      {label}
    {/if}
  </Button>
  {#if progress}
    <div class="download-status" aria-live="polite">
      <div
        class="download-progress"
        role="progressbar"
        aria-label="Verifying download"
        aria-valuemin="0"
        aria-valuemax={progress.total}
        aria-valuenow={progress.received}
      >
        <span style={`width: ${percentage}%`}></span>
      </div>
      <span>
        <Spinner size={12} />
        Verifying {formatBytes(progress.received)} / {formatBytes(progress.total)}
      </span>
    </div>
  {:else if outcome}
    <Chip size="xs" tone={failed ? "danger" : "success"} uppercase={false}>{outcome}</Chip>
  {/if}
</div>

<style>
  .download-control {
    display: flex;
    min-width: 0;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .download-status {
    display: grid;
    min-width: min(260px, 100%);
    gap: 4px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .download-status > span {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
  }

  .download-progress {
    width: 100%;
    height: 4px;
    overflow: hidden;
    border-radius: 999px;
    background: var(--bg-inset);
  }

  .download-progress > span {
    display: block;
    height: 100%;
    border-radius: inherit;
    background: var(--accent);
    transition: width 100ms linear;
  }
</style>
