<script lang="ts">
  import { onDestroy } from "svelte";
  import DownloadIcon from "@lucide/svelte/icons/download";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, Chip, Spinner } from "@kenn-io/kit-ui";
  import { APIError, type ContentVersion, type Node } from "./api.js";
  import {
    offerPreparedDownload,
    prepareCurrentDownload,
    prepareVersionDownload,
    type DownloadProgress,
  } from "./download.js";
  import { formatBytes } from "./format.js";

  let {
    session,
    node,
    version,
    label = "Download",
    onauthfailure,
  }: {
    session: string;
    node: Node;
    version?: ContentVersion;
    label?: string;
    onauthfailure: (cause: unknown) => void;
  } = $props();

  let controller = $state<AbortController | null>(null);
  let progress = $state<DownloadProgress | null>(null);
  let outcome = $state("");
  let failed = $state(false);

  onDestroy(() => controller?.abort());

  async function download(): Promise<void> {
    if (controller) {
      controller.abort();
      return;
    }
    const active = new AbortController();
    controller = active;
    progress = { received: 0, total: version?.size ?? node.size };
    outcome = "";
    failed = false;
    try {
      const report = (next: DownloadProgress) => {
        if (controller === active) progress = next;
      };
      const prepared = version
        ? await prepareVersionDownload(session, node, version, active.signal, report)
        : await prepareCurrentDownload(session, node, active.signal, report);
      if (controller !== active) return;
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
