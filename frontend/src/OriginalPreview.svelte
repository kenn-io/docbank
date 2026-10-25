<script lang="ts">
  import { Spinner } from "@kenn-io/kit-ui";
  import AppTooltip from "./AppTooltip.svelte";
  import ShieldCheckIcon from "@lucide/svelte/icons/shield-check";
  import { APIError } from "./api-transport.js";
  import {
    readVerifiedPreview,
    type DownloadProgress,
    type VerifiedPreview as Preview,
  } from "./download.js";
  import { formatBytes } from "./format.js";
  import type { SelectedSource } from "./selectedSource.js";

  let {
    session,
    source,
    authorizationRevision,
    onauthfailure,
  }: {
    session: string;
    source: SelectedSource;
    authorizationRevision: number;
    onauthfailure: (cause: unknown) => void;
  } = $props();
  let preview = $state<Preview | null>(null);
  let progress = $state<DownloadProgress | null>(null);
  let loading = $state(false);
  let error = $state("");
  let publishedURL = "";

  function revokePublishedURL(): void {
    if (!publishedURL) return;
    URL.revokeObjectURL(publishedURL);
    publishedURL = "";
  }

  $effect(() => {
    const epoch = `${session}:${source.key}:${authorizationRevision}`;
    const controller = new AbortController();
    let current = true;
    void epoch;
    revokePublishedURL();
    preview = null;
    progress = { received: 0, total: source.size };
    loading = true;
    error = "";
    void readVerifiedPreview(session, source, authorizationRevision, controller.signal, (next) => {
      if (current) progress = next;
    })
      .then((next) => {
        if (!current) {
          if (next.kind === "image") URL.revokeObjectURL(next.url);
          return;
        }
        preview = next;
        if (next.kind === "image") publishedURL = next.url;
      })
      .catch((cause: unknown) => {
        if (!current || (cause instanceof DOMException && cause.name === "AbortError")) return;
        if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
        else error = cause instanceof Error ? cause.message : String(cause);
      })
      .finally(() => {
        if (current) {
          loading = false;
          progress = null;
        }
      });
    return () => {
      current = false;
      controller.abort();
      revokePublishedURL();
      preview = null;
      progress = null;
      error = "";
    };
  });
</script>

<div class="original-preview">
  {#if loading}
    <div class="preview-loading" role="status">
      <Spinner size={14} />
      <span
        >Verifying {formatBytes(progress?.received ?? 0)} / {formatBytes(
          progress?.total ?? source.size,
        )}</span
      >
    </div>
  {:else if preview?.kind === "text"}
    <pre>{preview.text}</pre>
  {:else if preview?.kind === "image"}
    <div class="image-frame">
      <img src={preview.url} alt={`Verified preview of ${source.name}`} />
    </div>
  {:else if error}
    <p class="preview-unavailable" role="status">{error}</p>
  {/if}
  {#if preview}<div class="verified">
      <AppTooltip text="Matches the stored SHA-256" focusable align="end"
        ><ShieldCheckIcon size={14} aria-label="Verified" /></AppTooltip
      >
    </div>{/if}
</div>

<style>
  .original-preview {
    display: grid;
    position: relative;
    min-width: 0;
  }
  .preview-loading,
  .preview-unavailable {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .preview-loading {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-height: 96px;
  }
  .verified {
    position: absolute;
    top: var(--space-4);
    right: var(--space-4);
    display: flex;
    color: var(--accent-green);
  }
  pre,
  .image-frame {
    max-height: 320px;
    overflow: auto;
    margin: 0;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }
  pre {
    padding: var(--space-4) calc(var(--space-4) * 2 + 14px) var(--space-4) var(--space-4);
    color: var(--text-primary);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    line-height: 1.55;
    overflow-wrap: anywhere;
    white-space: pre-wrap;
  }
  .image-frame {
    display: grid;
    min-height: 120px;
    place-items: center;
    padding: var(--space-4);
  }
  img {
    display: block;
    max-width: 100%;
    max-height: 300px;
    object-fit: contain;
  }
  .preview-unavailable {
    margin: 0;
    padding: var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }
</style>
