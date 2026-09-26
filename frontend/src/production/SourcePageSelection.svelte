<script lang="ts">
  import { useDocumentState } from "@embedpdf/core/svelte";
  import { SelectionLayer, useSelectionCapability } from "@embedpdf/plugin-selection/svelte";
  import type { PdfPageObject, Rect } from "@embedpdf/models";

  export type Marquee = { pageIndex: number; rect: Rect; page: PdfPageObject; displayed: { width: number; height: number } };
  let { documentId, pageIndex, displayed, onmarquee }: {
    documentId: string; pageIndex: number; displayed: { width: number; height: number };
    onmarquee: (selection: Marquee) => void;
  } = $props();
  const selection = useSelectionCapability();
  const documentState = useDocumentState(() => documentId);

  $effect(() => {
    const capability = selection.provides;
    if (!capability) return;
    return capability.forDocument(documentId).onMarqueeEnd(event => {
      if (event.pageIndex !== pageIndex) return;
      const page = documentState.current?.document?.pages[pageIndex];
      if (page) onmarquee({ pageIndex, rect: event.rect, page, displayed });
    });
  });
</script>

<SelectionLayer {documentId} {pageIndex} />
