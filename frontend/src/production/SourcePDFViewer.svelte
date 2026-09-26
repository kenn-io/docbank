<script lang="ts">
  import { EmbedPDF } from "@embedpdf/core/svelte";
  import { createPluginRegistration, type PluginRegistry } from "@embedpdf/core";
  import { usePdfiumEngine } from "@embedpdf/engines/svelte";
  import { DocumentContent, DocumentContext, DocumentManagerPlugin, DocumentManagerPluginPackage } from "@embedpdf/plugin-document-manager/svelte";
  import { Viewport, ViewportPluginPackage } from "@embedpdf/plugin-viewport/svelte";
  import { Scroller, ScrollPluginPackage } from "@embedpdf/plugin-scroll/svelte";
  import type { PageLayout } from "@embedpdf/plugin-scroll";
  import { RenderLayer, RenderPluginPackage } from "@embedpdf/plugin-render/svelte";
  import pdfiumWasmURL from "@embedpdf/pdfium/pdfium.wasm?url";

  let { bytes, memberID }: { bytes: Uint8Array<ArrayBuffer>; memberID: string } = $props();
  // EmbedPDF's worker bundle uses blob workers, which the web CSP does not allow.
  const engine = usePdfiumEngine({ wasmUrl: pdfiumWasmURL, worker: false, fontFallback: null });
  let loadError = $state("");
  let plugins = $derived([
    createPluginRegistration(DocumentManagerPluginPackage),
    createPluginRegistration(ViewportPluginPackage),
    createPluginRegistration(ScrollPluginPackage),
    createPluginRegistration(RenderPluginPackage),
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
                  {#snippet renderPage(page: PageLayout)}
                    <div class="source-page" role="img" aria-label={`Original page ${page.pageNumber}`}
                      style={`width: ${page.width}px; height: ${page.height}px`}>
                      <RenderLayer {documentId} pageIndex={page.pageIndex} />
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
  .source-frame { height: min(70vh, 740px); min-height: 360px; width: 100%; }
  .source-page { position: relative; margin-inline: auto; background: white; box-shadow: 0 2px 14px var(--border-default); }
  .source-page :global(img) { display: block; width: 100%; height: 100%; }
</style>
