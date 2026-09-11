<script lang="ts">
  import { Button, Chip, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import { PageViewerSession, type PageViewerState } from "./pageViewer.js";
  import type { SelectedSource } from "./selectedSource.js";
  let { session, source, authorizationRevision, onauthfailure }: {
    session: string; source: SelectedSource; authorizationRevision: number; onauthfailure: (cause: unknown) => void;
  } = $props();
  const identity = $derived(`${session}:${source.key}:${authorizationRevision}`);
  let viewState = $state<PageViewerState>({ identity: "", page: 1, status: "loading" });
  const shown = $derived(viewState.identity === identity ? viewState : { identity, page: 1, status: "loading" } as PageViewerState);
  let viewer: PageViewerSession | undefined;
  let zoom = $state("fit");
  const count = $derived(shown.inventory?.inventory.page_count ?? 0);
  const available = $derived(new Set(shown.inventory?.inventory.images.map(v => v.page)).size);
  const failureMessages: Record<string, string> = { unavailable: "Page runtime unavailable.", unsupported: "Unsupported page geometry or density.",
    invalid_output: "Invalid page image. Verification failed.", stale_source: "The selected source changed. Refresh the document selection.",
    interrupted: "Rendering was interrupted.", storage: "The rendered page could not be retained." };
  $effect(() => {
    viewer = new PageViewerSession(session, source, authorizationRevision, identity, next => { viewState = next; }, onauthfailure);
    zoom = "fit";
    void viewer.load();
    const current = viewer;
    return () => current.dispose();
  });
</script>

<section class="page-viewer" aria-label={`Pages of ${source.name}`}>
  <div class="heading"><strong>Verified page</strong>{#if shown.url}<Chip size="xs" tone="success" dot>SHA-256 verified</Chip>{/if}</div>
  {#if count > 0}
    <nav class="page-controls" aria-label="Page navigation">
      <Button size="sm" surface="soft" disabled={shown.page <= 1} onclick={() => void viewer?.load(shown.page - 1)}>Previous page</Button>
      <SelectDropdown title="Page" value={String(shown.page)} options={Array.from({ length: count }, (_, i) => ({ value: String(i + 1), label: `${i + 1} of ${count}` }))}
        onchange={value => void viewer?.load(Number(value))} />
      <Button size="sm" surface="soft" disabled={shown.page >= count} onclick={() => void viewer?.load(shown.page + 1)}>Next page</Button>
    </nav>
    <div class="page-summary"><span aria-live="polite">Page {shown.page} of {count}</span>
      <SelectDropdown title="Zoom" value={zoom} options={[{ value: "fit", label: "Fit width" }, ...[50, 75, 100, 125, 150, 200].map(v => ({ value: String(v), label: `${v}%` }))]}
        onchange={value => { zoom = value; }} />
    </div>
    {#if available < count}<p class="notice">Partial image inventory · {available} of {count} pages rendered</p>{/if}
  {/if}
  {#if shown.url && shown.frame}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex (keyboard users must be able to scroll the image) -->
    <div class="page-scroll" tabindex="0" role="region" aria-label="Scrollable page image">
      <div class="page-content" style:width={zoom === "fit" ? "100%" : `${shown.frame.width / 10000 * 96 * Number(zoom) / 100}px`}
        style:aspect-ratio={`${shown.frame.width} / ${shown.frame.height}`}>
        <img src={shown.url} alt={`Verified page ${shown.page} of ${source.name}`} />
      </div>
    </div>
  {:else if shown.status === "loading" || shown.status === "rendering"}
    <p class="notice" role="status"><Spinner size={14} /> {shown.status === "rendering" ? "Rendering selected page…" : "Verifying page…"}</p>
    {#if shown.status === "rendering" && shown.job}<Button size="sm" onclick={() => void viewer?.cancel()}>Cancel render</Button>{/if}
  {:else if shown.status === "external"}
    <p class="notice" role="status">Another request is rendering this page. Refresh to check its retained image.</p>
    <Button size="sm" onclick={() => void viewer?.load()}>Refresh pages</Button>
  {:else if shown.status === "canceled" || shown.status === "failed"}
    <p class="notice" role="status">{shown.status === "canceled" ? "Render canceled." : `Render failed. ${failureMessages[shown.message ?? ""] ?? ""}`}</p>
    <Button size="sm" onclick={() => void viewer?.load()}>Refresh pages</Button>
  {:else if shown.status === "error"}
    <p class="notice" role="alert">{shown.message}</p>
    <Button size="sm" onclick={() => shown.retry ? void viewer?.render() : void viewer?.load()}>{shown.retry ? "Retry request" : "Refresh pages"}</Button>
  {:else}
    <p class="notice" role="status">{count === 0 ? "No page inventory has been retained for this version." : "This page has not been rendered."}</p>
    {#if shown.inventory?.runtime_available}
      <Button size="sm" tone="info" onclick={() => void viewer?.render()}>Render page {shown.page}</Button>
    {:else}<p class="notice" role="status">Page runtime unavailable. Retained pages remain readable.</p>{/if}
  {/if}
</section>

<style>
  .page-viewer { display: grid; gap: var(--space-2); min-width: 0; }
  .heading, .page-controls, .page-summary { display: flex; align-items: center; justify-content: space-between; gap: var(--space-1); flex-wrap: wrap; }
  .heading { font-size: var(--font-size-sm); color: var(--text-primary); }
  .page-summary, .notice { font-size: var(--font-size-xs); color: var(--text-muted); }
  .notice { display: flex; align-items: center; gap: var(--space-2); margin: 0; }
  .page-scroll { overflow: auto; max-height: 480px; background: var(--bg-inset); }
  .page-content { position: relative; background: white; }
  img { display: block; width: 100%; height: 100%; position: absolute; inset: 0; }
</style>
