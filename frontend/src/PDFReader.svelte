<script lang="ts">
  import { getThemeMode } from "@kenn-io/kit-ui";
  import { PDFViewer } from "@embedpdf/svelte-pdf-viewer";
  import wasmUrl from "@embedpdf/pdfium/pdfium.wasm?url";
  import fallbackFont from "../../internal/pdfproduction/fonts/NotoSansJP.ttf?url";
  let { url, name }: { url: string; name: string } = $props();
</script>

{#key url}
  <PDFViewer
    style="width: 100%; height: 100%;"
    oninit={(container) => {
      container.tabIndex = 0;
      container.setAttribute("aria-label", `PDF document: ${name}`);
    }}
    config={{
      // The engine runs in a blob worker, whose URL cannot resolve relative assets.
      wasmUrl: new URL(wasmUrl, window.location.href).href,
      fontFallback: {
        fonts: {},
        defaultFont: new URL(fallbackFont, window.location.href).href,
      },
      fonts: { ui: null, signature: null },
      stamp: { manifests: [] },
      documentManager: { initialDocuments: [{ url, name }], maxDocuments: 1 },
      tabBar: "never",
      theme: { preference: getThemeMode() },
      disabledCategories: [
        "annotation",
        "redaction",
        "form",
        "insert",
        "history",
        "document-open",
        "document-close",
        "document-protect",
        "document-capture",
        "panel-comment",
      ],
    }}
  />
{/key}
