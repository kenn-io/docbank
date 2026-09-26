<script lang="ts">
  import { EmbedPDF } from "@embedpdf/core/svelte";
  import { createPluginRegistration, type PluginRegistry } from "@embedpdf/core";
  import { usePdfiumEngine } from "@embedpdf/engines/svelte";
  import { DocumentContent, DocumentContext, DocumentManagerPlugin, DocumentManagerPluginPackage } from "@embedpdf/plugin-document-manager/svelte";
  import { Viewport, ViewportPluginPackage } from "@embedpdf/plugin-viewport/svelte";
  import { Scroller, ScrollPluginPackage } from "@embedpdf/plugin-scroll/svelte";
  import type { PageLayout } from "@embedpdf/plugin-scroll";
  import { RenderLayer, RenderPluginPackage } from "@embedpdf/plugin-render/svelte";
  import { PagePointerProvider, InteractionManagerPluginPackage } from "@embedpdf/plugin-interaction-manager/svelte";
  import { SelectionPluginPackage } from "@embedpdf/plugin-selection/svelte";
  import { Button } from "@kenn-io/kit-ui";
  import type { Marquee } from "./SourcePageSelection.svelte";
  import type { Frame, PhysicalBox } from "./embedpdfAdapter.js";
  import SourcePageSelection from "./SourcePageSelection.svelte";
  import pdfiumWasmURL from "@embedpdf/pdfium/pdfium.wasm?url";

  let { bytes, memberID, onmarquee, onpage, highlight = null }: { bytes: Uint8Array<ArrayBuffer>; memberID: string;
    onmarquee: (selection: Marquee) => void; onpage: (page: number) => void;
    highlight?: { page: number; frame: Frame; box?: PhysicalBox } | null } = $props();
  // EmbedPDF's worker bundle uses blob workers, which the web CSP does not allow.
  const engine = usePdfiumEngine({ wasmUrl: pdfiumWasmURL, worker: false, fontFallback: null });
  let loadError = $state("");
  let pageChoice = $state(1);
  let plugins = $derived([
    createPluginRegistration(DocumentManagerPluginPackage),
    createPluginRegistration(ViewportPluginPackage),
    createPluginRegistration(ScrollPluginPackage),
    createPluginRegistration(RenderPluginPackage),
    createPluginRegistration(InteractionManagerPluginPackage),
    createPluginRegistration(SelectionPluginPackage),
  ]);

  async function openVerifiedBuffer(registry: PluginRegistry): Promise<void> {
    const manager = registry.getPlugin<DocumentManagerPlugin>(DocumentManagerPlugin.id);
    if (!manager) { loadError = "The original PDF viewer could not start."; return; }
    manager.provides().openDocumentBuffer({ buffer: bytes.buffer, name: "source.pdf", documentId: memberID, autoActivate: true })
      .wait(({ task }) => task.wait(() => {}, error => { loadError = error.reason.message; }),
        error => { loadError = error.reason.message; });
  }
</script>

{#if engine.isLoading}
  <p role="status">Loading original PDF engine…</p>
{:else if engine.error}
  <p role="alert">The original PDF could not be displayed: {engine.error.message}</p>
{:else if engine.engine}
  <EmbedPDF engine={engine.engine} {plugins} onInitialized={openVerifiedBuffer}>
    {#snippet children()}
      <DocumentContext>
        {#snippet children({ activeDocumentId })}
          {#if activeDocumentId}
            {@const documentId = activeDocumentId}
            <DocumentContent {documentId}>
              {#snippet children(content)}
                {#if content.isError}
                  <p role="alert">The original PDF could not be displayed.</p>
                {:else if content.isLoaded}
                  {@const pages = content.documentState.document?.pages ?? []}
                  <div class="source-toolbar">
                    <label for="production-pdf-page">PDF page
                      <select id="production-pdf-page" bind:value={pageChoice}>
                        {#each pages as pdfPage (pdfPage.index)}<option value={pdfPage.index + 1}>{pdfPage.index + 1}</option>{/each}
                      </select>
                    </label>
                    <Button size="sm" surface="soft" onclick={() => onpage(pageChoice)}>Select whole page</Button>
                    <small>Drag on the PDF to select a rectangle.</small>
                  </div>
                  {#snippet renderPage(page: PageLayout)}
                    <div class="source-page" role="img" aria-label={`Original page ${page.pageNumber}`}
                      style={`width: ${page.width}px; height: ${page.height}px`}>
                      <PagePointerProvider {documentId} pageIndex={page.pageIndex}>
                        <RenderLayer {documentId} pageIndex={page.pageIndex} />
                        <SourcePageSelection {documentId} pageIndex={page.pageIndex}
                          displayed={{ width: page.width, height: page.height }} {onmarquee} />
                      </PagePointerProvider>
                      {#if highlight?.page === page.pageNumber}
                        {@const box = highlight.box}
                        <div class="source-selection-overlay" aria-hidden="true"
                          style={`left: ${box ? box.x0 / highlight.frame.width * 100 : 0}%; top: ${box ? box.y0 / highlight.frame.height * 100 : 0}%; width: ${box ? (box.x1 - box.x0) / highlight.frame.width * 100 : 100}%; height: ${box ? (box.y1 - box.y0) / highlight.frame.height * 100 : 100}%`}></div>
                      {/if}
                    </div>
                  {/snippet}
                  <div class="source-frame">
                    <Viewport {documentId} class="source-viewport">
                      <Scroller {documentId} {renderPage} />
                    </Viewport>
                  </div>
                {:else}
                  <p role="status">Opening original PDF…</p>
                {/if}
              {/snippet}
            </DocumentContent>
          {:else}
            {#if loadError}<p role="alert">The original PDF could not be displayed: {loadError}</p>
            {:else}<p role="status">Opening original PDF…</p>{/if}
          {/if}
        {/snippet}
      </DocumentContext>
    {/snippet}
  </EmbedPDF>
{/if}

<style>
  .source-frame { height: min(50vh, 520px); min-height: 320px; width: 100%; }
  .source-page { position: relative; margin-inline: auto; background: white; box-shadow: 0 2px 14px var(--border-default); }
  .source-page :global(img) { display: block; width: 100%; height: 100%; pointer-events: none; user-select: none; -webkit-user-drag: none; }
  .source-selection-overlay { position: absolute; z-index: 2; box-sizing: border-box; pointer-events: none; border: 2px solid var(--accent-blue); background: color-mix(in srgb, var(--accent-blue) 23%, transparent); }
  .source-toolbar { display: flex; align-items: end; gap: var(--space-2); flex-wrap: wrap; margin-block: var(--space-2); }
  .source-toolbar label { display: grid; gap: var(--space-1); font-size: var(--font-size-xs); color: var(--text-secondary); }
  .source-toolbar select { padding: var(--space-1) var(--space-2); border: 1px solid var(--border-muted); border-radius: var(--radius-sm); background: var(--bg-raised); color: var(--text-primary); }
  .source-toolbar small { color: var(--text-muted); }
</style>
