<script lang="ts">
  import { onDestroy, untrack } from "svelte";
  import { PDFReviewBridge, type ReaderReview, type ReaderReviewStatus } from "./pdfReview.js";
  import { getThemeMode } from "@kenn-io/kit-ui";
  import { PDFViewer } from "@embedpdf/svelte-pdf-viewer";
  import wasmUrl from "@embedpdf/pdfium/pdfium.wasm?url";
  import fallbackFont from "../../internal/pdfproduction/fonts/NotoSansJP.ttf?url";
  let {
    url,
    name,
    review,
    onstatus,
    onbridge,
  }: {
    url: string;
    name: string;
    review?: ReaderReview;
    onstatus?: (status: ReaderReviewStatus) => void;
    onbridge?: (bridge: PDFReviewBridge) => void;
  } = $props();
  let bridge = $state<PDFReviewBridge>();
  let position = $state("");
  $effect(() => {
    const b = bridge,
      r = review;
    untrack(() => b?.configure(r));
  });
  onDestroy(() => bridge?.dispose());
</script>

{#key url}
  <PDFViewer
    style="width: 100%; height: 100%;"
    oninit={(container) => {
      container.tabIndex = 0;
      bridge = new PDFReviewBridge(
        container,
        (status) => untrack(() => onstatus?.(status)),
        (value) => (position = value),
      );
      onbridge?.(bridge);
      container.setAttribute("aria-label", `PDF document: ${name}`);
    }}
    onready={(registry) => bridge?.ready(registry)}
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
      redaction: { useAnnotationMode: false },
      selection: { marquee: { minDragPx: 0 } },
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
<span
  class="kit-sr-only"
  role="status"
  aria-label="Mask cursor"
  aria-live="polite"
  aria-atomic="true">{position}</span
>
