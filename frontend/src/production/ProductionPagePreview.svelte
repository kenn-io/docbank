<script lang="ts">
  import { onDestroy, tick } from "svelte";
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "../api-transport.js";
  import { loadProductionPreview, StaleProductionPreviewError } from "./preview.js";
  import type { ProductionMember } from "./api.js";

  let { session, setID, revision, etag, member, page, onstale, onauthfailure }: {
    session: string; setID: string; revision: number; etag: number;
    member: ProductionMember; page: number;
    onstale: () => void; onauthfailure: (cause: unknown) => void;
  } = $props();
  let loading = $state(false);
  let error = $state("");
  let imageURL = $state("");
  let previewText = $state("");
  let textTruncated = $state(false);
  let previewInputSHA256 = $state("");
  let pendingOperationID = "";
  let controller = new AbortController();

  onDestroy(() => { controller.abort(); clearPreview(); });

  function clearPreview(): void {
    if (imageURL) URL.revokeObjectURL(imageURL);
    imageURL = "";
    previewText = "";
    textTruncated = false;
    previewInputSHA256 = "";
  }

  async function requestPreview(): Promise<void> {
    if (loading) return;
    clearPreview();
    controller.abort();
    controller = new AbortController();
    const signal = controller.signal;
    const operationID = pendingOperationID || crypto.randomUUID();
    pendingOperationID = operationID;
    loading = true;
    error = "";
    try {
      const result = await loadProductionPreview(session, setID, revision, etag, member.id, page,
        operationID, signal);
      signal.throwIfAborted();
      imageURL = URL.createObjectURL(new Blob([result.image], { type: "image/png" }));
      previewText = result.text.slice(0, 20_000);
      if (previewText.length && /[\uD800-\uDBFF]/.test(previewText.at(-1) ?? "")) previewText = previewText.slice(0, -1);
      textTruncated = result.text.length > previewText.length;
      previewInputSHA256 = result.previewInputSHA256;
      pendingOperationID = "";
      await tick();
      document.getElementById("production-output-preview")?.focus();
    } catch (cause) {
      if (signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else if (cause instanceof StaleProductionPreviewError || cause instanceof APIError && cause.status === 409 &&
          (cause.code === "production_revision_conflict" || cause.code === "production_preview_stale" || cause.code === "source_stale")) onstale();
      else error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!signal.aborted) loading = false;
    }
  }
</script>

<div class="preview-control">
  <Button size="sm" surface="soft" disabled={loading} onclick={() => void requestPreview()}>
    {imageURL ? "Refresh production preview" : "Preview production output"}
  </Button>
  {#if loading}<p role="status"><Spinner size={14} /> Verifying production preview…</p>{/if}
  {#if error}<p class="error" role="alert">{error}</p>{/if}
  {#if imageURL}
    <section id="production-output-preview" class="output-preview" tabindex="-1"
      aria-label={`Production preview for member ${member.ordinal} page ${page}`}>
      <div class="preview-heading"><strong>Production preview · page {page}</strong>
        <Button size="sm" surface="soft" onclick={clearPreview}>Clear preview</Button></div>
      <p>Unnumbered output from draft revision {revision}, change version {etag}. Input {previewInputSHA256.slice(0, 12)}…</p>
      <a href={imageURL} target="_blank" rel="noopener noreferrer">Open full-size image</a>
      <img src={imageURL} alt={`Verified produced page ${page} for member ${member.ordinal}`} />
      <strong>Output text</strong>
      <pre>{previewText}</pre>
      {#if textTruncated}<small>Showing the first 20,000 characters of the verified text.</small>{/if}
    </section>
  {/if}
</div>

<style>
  .preview-control{display:grid;gap:var(--space-2)}
  .preview-control p{margin:0}
  .output-preview{display:grid;gap:var(--space-2);padding:var(--space-3);border:1px solid var(--border-default);border-radius:var(--radius-md);background:var(--bg-raised)}
  .preview-heading{display:flex;align-items:center;justify-content:space-between;gap:var(--space-2);flex-wrap:wrap}
  .output-preview img{display:block;max-width:100%;max-height:420px;object-fit:contain;background:white}
  .output-preview pre{margin:0;max-height:180px;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;font:inherit}
  .output-preview small{color:var(--text-muted)}
  .error{color:var(--accent-red)}
</style>
