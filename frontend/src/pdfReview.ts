import type {
  PluginRegistry,
  DocumentManagerPlugin,
  RedactionPlugin,
  ScrollPlugin,
  ZoomPlugin,
  RotatePlugin,
  ViewportPlugin,
  CommandsPlugin,
  UIPlugin,
  SelectionPlugin,
  ZoomLevel,
} from "@embedpdf/svelte-pdf-viewer";
import { ZoomMode } from "@embedpdf/svelte-pdf-viewer";
import type {
  RedactionPreparation,
  PDFRedactionBox,
  PDFRedactionGap,
  PDFRedactionSpan,
} from "./generated/docbank.js";
import { bindReaderPages, readerBox, overlayBox } from "./pdfReviewGeometry.js";
import { matchTextSelection } from "./pdfTextSelection.js";
import { readTextSelection } from "./pdfTextSelectionReader.js";

export interface TextSelectionStatus {
  candidate?: { page: number; spans: PDFRedactionSpan[] };
  busy: boolean;
  error: string;
}

export interface ReaderReview {
  preparation?: RedactionPreparation;
  boxes: (PDFRedactionBox & { outline?: boolean })[];
  gaps: PDFRedactionGap[];
  proposed: PDFRedactionBox[];
  drawing: boolean;
  visible: boolean;
  editable: boolean;
  oncommit: (box: PDFRedactionBox) => void;
  ondrag: (active: boolean) => void;
  ondrawing: (active: boolean) => void;
  onselection: (status: TextSelectionStatus) => void;
}
export interface ReaderReviewStatus {
  page: number;
  ready: boolean;
  error: string;
}
interface ImageEvent {
  documentId: string;
  pageIndex: number;
  kind: "page" | "tile";
  token: object;
  status: "pending" | "ready" | "error" | "disposed";
  retry: () => void;
}
interface LayerEvent {
  documentId: string;
  pageIndex: number;
  target: HTMLElement;
  status: "mounted" | "disposed";
}
type PageIdentity = Pick<LayerEvent, "documentId" | "pageIndex">;
interface Layer {
  event: LayerEvent;
  svg: SVGSVGElement;
  cursor: { x: number; y: number };
  start?: { x: number; y: number };
  remove: () => void;
}
const svgNS = "http://www.w3.org/2000/svg";
export class PDFReviewBridge {
  private registry?: PluginRegistry;
  private images = new Map<object, ImageEvent>();
  private layers = new Map<HTMLElement, Layer>();
  private subscriptions: (() => void)[] = [];
  private review?: ReaderReview;
  private bound?: RedactionPreparation;
  private geometryError = "";
  private disposed = false;
  private saved?: {
    zoom: ZoomLevel;
    rotation: number;
    x: number;
    y: number;
    page?: number;
  };
  private updating = false;
  private settingMode = false;
  private pointer?: PageIdentity & { pointerId: number };
  private lastStatus = "";
  private frame = 0;
  private textController?: AbortController;
  private textWork?: Promise<void>;
  private textGeneration = 0;
  private textPage?: PageIdentity;
  private textPointer?: PageIdentity & { pointerId: number };
  private textCandidate?: TextSelectionStatus["candidate"];
  private textRange = "";
  private lastTextStatus = "";
  constructor(
    private host: HTMLElement,
    private changed: (status: ReaderReviewStatus) => void,
    private announce: (position: string) => void,
  ) {
    host.addEventListener("embedpdf:page-render", this.renderEvent);
    host.addEventListener("embedpdf:page-layer", this.layerEvent);
    host.addEventListener("keydown", this.key, true);
  }
  private manager() {
    return this.registry?.getPlugin<DocumentManagerPlugin>("document-manager")?.provides();
  }
  private document() {
    return this.manager()?.getActiveDocument();
  }
  private redaction() {
    const d = this.document();
    return (
      d && this.registry?.getPlugin<RedactionPlugin>("redaction")?.provides().forDocument(d.id)
    );
  }
  private scroll() {
    const d = this.document();
    return d && this.registry?.getPlugin<ScrollPlugin>("scroll")?.provides().forDocument(d.id);
  }
  private zoom() {
    const d = this.document();
    return d && this.registry?.getPlugin<ZoomPlugin>("zoom")?.provides().forDocument(d.id);
  }
  private selection() {
    const d = this.document();
    return (
      d && this.registry?.getPlugin<SelectionPlugin>("selection")?.provides().forDocument(d.id)
    );
  }
  private canSelect() {
    return (
      !this.disposed &&
      !!this.review?.editable &&
      !!this.review.visible &&
      !this.review.drawing &&
      !!this.bound
    );
  }
  private textStatus(status: TextSelectionStatus) {
    const key = JSON.stringify(status);
    if (key === this.lastTextStatus) return;
    this.lastTextStatus = key;
    this.review?.onselection(status);
  }
  private invalidateSelection() {
    this.textGeneration++;
    this.textController?.abort();
    this.textCandidate = undefined;
    this.textRange = "";
    this.textStatus({ busy: false, error: "" });
  }
  clearSelection() {
    this.invalidateSelection();
    this.textPage = undefined;
    this.textPointer = undefined;
    if (this.review) this.selection()?.clear();
  }
  selectedText() {
    const candidate = this.textCandidate;
    return candidate &&
      this.canSelect() &&
      this.isReady(candidate.page - 1) &&
      JSON.stringify(this.selection()?.getState().selection) === this.textRange
      ? candidate
      : undefined;
  }
  private matchSelection() {
    const scope = this.selection(),
      state = scope?.getState(),
      doc = this.document(),
      prep = this.bound;
    if (
      this.textWork ||
      !this.canSelect() ||
      !scope ||
      !state?.selection ||
      state.selecting ||
      !doc ||
      !prep
    )
      return;
    const pageIndex = state.selection.start.page;
    if (pageIndex !== state.selection.end.page) {
      this.textStatus({ busy: false, error: "Select text on one page at a time." });
      return;
    }
    if (!this.isReady(pageIndex)) return;
    const generation = this.textGeneration,
      range = JSON.stringify(state.selection);
    const controller = new AbortController();
    this.textController = controller;
    this.textPage = { documentId: doc.id, pageIndex };
    this.textStatus({ busy: true, error: "" });
    this.textWork = (async () => {
      try {
        const selection = await readTextSelection(this.registry!, controller.signal);
        controller.signal.throwIfAborted();
        const spans = matchTextSelection(prep, selection);
        if (
          generation !== this.textGeneration ||
          this.bound !== prep ||
          this.document()?.id !== doc.id ||
          !this.canSelect() ||
          !this.isReady(pageIndex) ||
          JSON.stringify(scope.getState().selection) !== range
        )
          return;
        this.textRange = range;
        this.textCandidate = { page: selection.page, spans };
        this.textStatus({ candidate: this.textCandidate, busy: false, error: "" });
      } catch (cause) {
        if (!controller.signal.aborted && generation === this.textGeneration)
          this.textStatus({
            busy: false,
            error:
              cause instanceof Error
                ? cause.message
                : "Text selection could not finish. Draw a rectangle instead.",
          });
      } finally {
        this.textWork = undefined;
        if (!this.disposed && generation !== this.textGeneration)
          queueMicrotask(() => this.matchSelection());
      }
    })();
  }
  ready(registry: PluginRegistry) {
    this.registry = registry;
    const selection = registry.getPlugin<SelectionPlugin>("selection")!.provides();
    this.subscriptions.push(
      selection.onSelectionChange(({ documentId, selection: range }) => {
        if (!this.review || documentId !== this.document()?.id) return;
        this.invalidateSelection();
        if (!range) this.textPage = undefined;
        queueMicrotask(() => this.matchSelection());
      }),
      selection.onBeginSelection(({ documentId, page }) => {
        if (!this.review || documentId !== this.document()?.id) return;
        this.invalidateSelection();
        this.textPage = { documentId, pageIndex: page };
      }),
      selection.onEndSelection(({ documentId }) => {
        if (documentId === this.document()?.id) this.matchSelection();
      }),
    );
    const redaction = registry.getPlugin<RedactionPlugin>("redaction")!.provides();
    this.subscriptions.push(
      redaction.onStateChange(({ documentId, state }) => {
        if (
          this.settingMode ||
          documentId !== this.document()?.id ||
          !this.review?.drawing ||
          state.isRedacting
        )
          return;
        // A reader tool (pan or pointer) takes ownership from marquee marking.
        this.review = { ...this.review, drawing: false };
        this.pointer = undefined;
        for (const layer of this.layers.values()) layer.start = undefined;
        this.review.ondrawing(false);
        this.review.ondrag(false);
        this.update();
      }),
    );
    this.subscriptions.push(
      redaction.onPendingChange((e) => {
        if (
          this.updating ||
          !this.review ||
          !Object.values(e.pending).some((items) => items.length)
        )
          return;
        this.updating = true;
        try {
          const d = this.document(),
            prep = this.review.preparation;
          for (const item of Object.values(e.pending).flat()) {
            if (
              !d ||
              !prep ||
              item.kind !== "area" ||
              !this.isReady(item.page) ||
              !this.review.drawing ||
              !this.review.visible
            )
              continue;
            const box = readerBox(item.rect, d.pages[item.page]!, prep.frames[item.page]!);
            if (box.x1 > box.x0 && box.y1 > box.y0)
              this.review.oncommit({
                page: item.page + 1,
                frame_sha256: prep.map.pages[item.page]!.frame_sha256,
                ...box,
              });
          }
          this.review.ondrag([...this.layers.values()].some((l) => l.start));
        } finally {
          // Let the library finish its pointer-up and clear its drag rectangle
          // before retiring the transient mark. Docbank owns the committed box.
          queueMicrotask(() => {
            try {
              if (!this.disposed) redaction.forDocument(e.documentId).clearPending();
            } finally {
              this.updating = false;
            }
          });
        }
      }),
    );
    this.subscriptions.push(
      registry
        .getPlugin<ScrollPlugin>("scroll")!
        .provides()
        .onPageChange(() => {
          this.clearSelection();
          this.cancelDraft();
          this.update();
        }),
    );
    this.subscriptions.push(this.manager()!.onDocumentOpened(() => this.update()));
    this.subscriptions.push(
      registry
        .getPlugin<ZoomPlugin>("zoom")!
        .provides()
        .onZoomChange(({ documentId }) => {
          this.clearSelection();
          this.cancelDraft();
          if (!this.review || documentId !== this.document()?.id) return;
          const navigation = this.scroll()?.getPageChangeState();
          if (!navigation?.isChanging) return;
          const target = navigation.targetPage;
          // Fit recalculation scrolls instantly and interrupts a smooth page
          // turn. Restore its destination after the zoom scroll is applied.
          requestAnimationFrame(() => {
            const scroll = this.scroll();
            if (
              !this.disposed &&
              this.review &&
              this.document()?.id === documentId &&
              scroll?.getPageChangeState().targetPage === target
            )
              this.goToPage(target);
          });
        }),
    );
    this.subscriptions.push(
      registry
        .getPlugin<RotatePlugin>("rotate")!
        .provides()
        .onRotateChange(() => {
          this.clearSelection();
          this.cancelDraft();
        }),
    );
    const viewport = registry.getPlugin<ViewportPlugin>("viewport")!.provides();
    // General viewport events also announce idle scrolling without any movement.
    this.subscriptions.push(
      viewport.onScrollChange(() => this.cancelDraft()),
      viewport.onViewportResize(() => {
        this.clearSelection();
        this.cancelDraft();
      }),
    );
    this.update();
  }
  configure(review?: ReaderReview) {
    const leaving = !review && !!this.review;
    if (
      this.review &&
      (!review ||
        !review.editable ||
        !review.visible ||
        review.drawing ||
        this.review.preparation?.map.sha256 !== review.preparation?.map.sha256)
    )
      this.clearSelection();
    this.review = review;
    if (!review?.drawing || !review.visible) {
      this.cancelDraft();
      this.announce("");
    }
    if (leaving) {
      this.bound = undefined;
      this.geometryError = "";
      this.redaction()?.clearPending();
      if (this.saved && this.document()) {
        const id = this.document()!.id,
          saved = this.saved;
        this.registry!.getPlugin<RotatePlugin>("rotate")!
          .provides()
          .forDocument(id)
          .setRotation(saved.rotation);
        this.zoom()!.requestZoom(saved.zoom);
        requestAnimationFrame(() => {
          if (!this.disposed && !this.review) {
            if (saved.page) this.goToPage(saved.page);
            else
              this.registry!.getPlugin<ViewportPlugin>("viewport")!
                .provides()
                .forDocument(id)
                .scrollTo({ x: saved.x, y: saved.y, behavior: "instant" });
          }
        });
      }
      this.saved = undefined;
    }
    this.update();
  }
  private enterReview() {
    if (this.review && !this.saved && this.document()) {
      const id = this.document()!.id,
        viewport = this.registry!.getPlugin<ViewportPlugin>("viewport")!.provides().forDocument(id),
        rotation = this.registry!.getPlugin<RotatePlugin>("rotate")!.provides().forDocument(id);
      const metrics = viewport.getMetrics(),
        navigation = this.scroll()!.getPageChangeState();
      this.saved = {
        zoom: this.zoom()!.getState().zoomLevel,
        rotation: rotation.getRotation(),
        x: metrics.scrollLeft,
        y: metrics.scrollTop,
        // A page turn announces its destination before smooth scrolling reaches it.
        page: navigation.isChanging ? navigation.targetPage : undefined,
      };
      this.zoom()!.requestZoom(ZoomMode.FitPage);
      if (this.saved.page) this.goToPage(this.saved.page);
    }
    for (const category of ["document-print", "document-export"]) {
      const commands = this.registry?.getPlugin<CommandsPlugin>("commands")?.provides(),
        ui = this.registry?.getPlugin<UIPlugin>("ui")?.provides();
      if (this.review) {
        commands?.disableCategory(category);
        ui?.disableCategory(category);
      } else {
        commands?.enableCategory(category);
        ui?.enableCategory(category);
      }
    }
  }
  goToPage(page: number) {
    this.scroll()?.scrollToPage({ pageNumber: page, behavior: "instant" });
  }
  retry(page: number) {
    for (const event of this.images.values())
      if (event.pageIndex === page - 1 && event.status === "error") event.retry();
  }
  private setDrawingMode(active: boolean) {
    this.settingMode = true;
    try {
      if (active) this.redaction()?.enableMarqueeRedact();
      else this.redaction()?.endRedact();
    } finally {
      this.settingMode = false;
    }
  }
  private isReady(index: number) {
    if (!this.bound || this.geometryError) return false;
    const entries = [...this.images.values()].filter(
      (e) => e.documentId === this.document()?.id && e.pageIndex === index,
    );
    return (
      entries.some((e) => e.kind === "page" && e.status === "ready") &&
      entries.every((e) => e.status === "ready")
    );
  }
  private cancelDraft(page?: PageIdentity) {
    const matches = (other: PageIdentity) =>
      !page || (other.documentId === page.documentId && other.pageIndex === page.pageIndex);
    const pointer = this.pointer && matches(this.pointer);
    const drafts = [...this.layers.values()].filter((l) => l.start && matches(l.event));
    if (!pointer && !drafts.length) return;
    if (pointer) {
      this.pointer = undefined;
      this.setDrawingMode(false);
    }
    for (const layer of drafts) layer.start = undefined;
    this.review?.ondrag(!!this.pointer || [...this.layers.values()].some((l) => l.start));
    this.update();
  }
  private renderEvent = (raw: Event) => {
    const event = (raw as CustomEvent<ImageEvent>).detail;
    if (
      event.status !== "ready" &&
      this.textPage?.documentId === event.documentId &&
      this.textPage.pageIndex === event.pageIndex
    )
      this.clearSelection();
    if (event.status === "disposed") this.images.delete(event.token);
    else this.images.set(event.token, event);
    if (event.status !== "ready" && !this.isReady(event.pageIndex)) this.cancelDraft(event);
    this.update();
  };
  private layerEvent = (raw: Event) => {
    const event = (raw as CustomEvent<LayerEvent>).detail,
      old = this.layers.get(event.target);
    if (old) {
      if (
        this.textPage?.documentId === old.event.documentId &&
        this.textPage.pageIndex === old.event.pageIndex
      )
        this.clearSelection();
      this.cancelDraft(old.event);
      old.remove();
      old.svg.remove();
      this.layers.delete(event.target);
    }
    if (event.status === "mounted") {
      const svg = document.createElementNS(svgNS, "svg");
      svg.style.cssText =
        "position:absolute;inset:0;width:100%;height:100%;pointer-events:none;overflow:visible";
      event.target.append(svg);
      event.target.setAttribute("aria-hidden", "true");
      event.target.dataset.reviewPage = String(event.pageIndex + 1);
      const surface = event.target.parentElement!;
      const down = (e: PointerEvent) => {
        if (!this.review?.drawing) {
          if (this.canSelect() && this.isReady(event.pageIndex))
            this.textPointer = { ...event, pointerId: e.pointerId };
          return;
        }
        if (!this.review.visible || !this.isReady(event.pageIndex)) {
          e.preventDefault();
          e.stopImmediatePropagation();
          return;
        }
        this.pointer = { ...event, pointerId: e.pointerId };
        this.review.ondrag(true);
        this.host.focus({ preventScroll: true });
      };
      surface.addEventListener("pointerdown", down, true);
      const up = (e: PointerEvent) => {
        if (this.textPointer?.pointerId === e.pointerId) this.textPointer = undefined;
        if (this.pointer?.pointerId !== e.pointerId) return;
        this.pointer = undefined;
        this.review?.ondrag([...this.layers.values()].some((l) => l.start));
      };
      const cancel = (e: PointerEvent) => {
        if (this.textPointer?.pointerId === e.pointerId) this.clearSelection();
        if (this.pointer?.pointerId === e.pointerId) this.cancelDraft(this.pointer);
      };
      window.addEventListener("pointerup", up);
      window.addEventListener("pointercancel", cancel);
      this.layers.set(event.target, {
        event,
        svg,
        cursor: { x: 0, y: 0 },
        remove: () => {
          surface.removeEventListener("pointerdown", down, true);
          window.removeEventListener("pointerup", up);
          window.removeEventListener("pointercancel", cancel);
        },
      });
    }
    this.update();
  };
  private update() {
    if (this.disposed) return;
    this.enterReview();
    const doc = this.document(),
      prep = this.review?.preparation;
    if (doc && prep && prep !== this.bound) {
      try {
        bindReaderPages(doc, prep.frames);
        this.bound = prep;
        this.geometryError = "";
      } catch (e) {
        this.geometryError = (e as Error).message;
        this.bound = undefined;
      }
    }
    const page = this.scroll()?.getCurrentPage() ?? 1,
      ready = this.isReady(page - 1);
    const failed = [...this.images.values()].some(
      (e) => e.pageIndex === page - 1 && e.status === "error",
    );
    const status = {
      page,
      ready,
      error:
        this.geometryError ||
        (failed ? `Page ${page} could not render. Retry the page before marking.` : ""),
    };
    const key = JSON.stringify(status);
    if (key !== this.lastStatus) {
      this.lastStatus = key;
      this.changed(status);
    }
    const draw = !!this.review?.drawing && !!this.review.visible && ready;
    const redaction = this.redaction();
    if (draw && !redaction?.isMarqueeRedactActive()) this.setDrawingMode(true);
    if (!draw && redaction?.isMarqueeRedactActive()) this.setDrawingMode(false);
    cancelAnimationFrame(this.frame);
    this.frame = requestAnimationFrame(() => this.paint());
  }
  private paint() {
    const prep = this.bound,
      doc = this.document();
    for (const layer of this.layers.values()) {
      const { svg, event } = layer;
      svg.replaceChildren();
      event.target.hidden = !this.review?.visible;
      if (!prep || !doc || !this.review) continue;
      const index = event.pageIndex,
        frame = prep.frames[index]!,
        page = doc.pages[index]!;
      svg.setAttribute("viewBox", `0 0 ${page.size.width} ${page.size.height}`);
      const rect = (
        box: Pick<PDFRedactionBox, "x0" | "y0" | "x1" | "y1">,
        stroke: string,
        fill: string,
        dashed = false,
      ) => {
        const b = overlayBox(box, page, frame),
          r = document.createElementNS(svgNS, "rect");
        for (const [k, v] of Object.entries(b)) r.setAttribute(k, String(v));
        r.setAttribute("fill", fill);
        r.setAttribute("stroke", stroke);
        r.setAttribute("stroke-width", "2");
        r.setAttribute("vector-effect", "non-scaling-stroke");
        if (dashed) r.setAttribute("stroke-dasharray", "4 4");
        svg.append(r);
      };
      for (const g of this.review.gaps)
        if (g.box.page === index + 1) rect(g.box, "#c58000", "#c5800026", true);
      for (const b of this.review.boxes)
        if (b.page === index + 1) rect(b, b.outline === false ? "none" : "#111", "#00000040");
      for (const b of this.review.proposed)
        if (b.page === index + 1) rect(b, "#0076d6", "#0076d626");
      if (this.review.drawing && this.isReady(index)) {
        const { cursor, start } = layer;
        if (start)
          rect(
            {
              x0: Math.min(start.x, cursor.x),
              y0: Math.min(start.y, cursor.y),
              x1: Math.max(start.x, cursor.x),
              y1: Math.max(start.y, cursor.y),
            },
            "#0076d6",
            "#0076d626",
          );
        if (index !== (this.scroll()?.getCurrentPage() ?? 1) - 1) continue;
        const point = overlayBox(
          { x0: cursor.x, y0: cursor.y, x1: cursor.x, y1: cursor.y },
          page,
          frame,
        );
        const matrix = svg.getScreenCTM();
        if (matrix) {
          // Keep the crosshair visible at every zoom and viewing rotation.
          const arm = 8 / Math.hypot(matrix.a, matrix.b);
          for (const [color, width] of [
            ["white", "4"],
            ["#0076d6", "2"],
          ]) {
            const crosshair = document.createElementNS(svgNS, "path");
            crosshair.setAttribute(
              "d",
              `M ${point.x - arm} ${point.y} h ${arm * 2} M ${point.x} ${point.y - arm} v ${arm * 2}`,
            );
            crosshair.setAttribute("stroke", color!);
            crosshair.setAttribute("stroke-width", width!);
            crosshair.setAttribute("vector-effect", "non-scaling-stroke");
            svg.append(crosshair);
          }
        }
      } else layer.start = undefined;
    }
  }
  private key = (event: KeyboardEvent) => {
    const review = this.review,
      prep = this.bound;
    if (
      event.composedPath()[0] !== this.host ||
      !review?.drawing ||
      !review.visible ||
      !prep ||
      event.ctrlKey ||
      event.metaKey ||
      event.altKey
    )
      return;
    const index = (this.scroll()?.getCurrentPage() ?? 1) - 1;
    const layer = [...this.layers.values()].find((l) => l.event.pageIndex === index);
    if (!layer || !this.isReady(index)) return;
    const frame = prep.frames[index]!,
      step = event.shiftKey ? 1000 : 100;
    if (event.key.startsWith("Arrow")) {
      event.preventDefault();
      event.stopImmediatePropagation();
      const rotation = this.registry!.getPlugin<RotatePlugin>("rotate")!
        .provides()
        .forDocument(this.document()!.id)
        .getRotation();
      const dx = event.key === "ArrowRight" ? step : event.key === "ArrowLeft" ? -step : 0,
        dy = event.key === "ArrowDown" ? step : event.key === "ArrowUp" ? -step : 0;
      // Physical coordinates already include the source's intrinsic rotation.
      const [x, y] = [
        [dx, dy],
        [dy, -dx],
        [-dx, -dy],
        [-dy, dx],
      ][rotation]!;
      layer.cursor = {
        x: Math.max(0, Math.min(frame.width, layer.cursor.x + x!)),
        y: Math.max(0, Math.min(frame.height, layer.cursor.y + y!)),
      };
    } else if (event.key === "Enter") {
      event.preventDefault();
      event.stopImmediatePropagation();
      const { start, cursor } = layer;
      if (start) {
        if (start.x !== cursor.x && start.y !== cursor.y)
          review.oncommit({
            page: index + 1,
            frame_sha256: prep.map.pages[index]!.frame_sha256,
            x0: Math.min(start.x, cursor.x),
            y0: Math.min(start.y, cursor.y),
            x1: Math.max(start.x, cursor.x),
            y1: Math.max(start.y, cursor.y),
          });
        layer.start = undefined;
        review.ondrag(false);
      } else {
        layer.start = { ...cursor };
        review.ondrag(true);
      }
    } else if (event.key === "Escape" && (layer.start || this.pointer)) {
      event.preventDefault();
      event.stopImmediatePropagation();
      layer.start = undefined;
      this.pointer = undefined;
      review.ondrag(false);
      this.setDrawingMode(false);
      this.update();
    } else return;
    this.announce(
      `Page ${index + 1}, cursor ${(layer.cursor.x / 10000).toFixed(2)}, ${(layer.cursor.y / 10000).toFixed(2)} inches.`,
    );
    this.paint();
  };
  focus() {
    this.host.focus({ preventScroll: true });
  }
  dispose() {
    this.clearSelection();
    this.disposed = true;
    this.announce("");
    cancelAnimationFrame(this.frame);
    this.setDrawingMode(false);
    this.subscriptions.forEach((f) => f());
    this.layers.forEach((l) => {
      l.remove();
      l.svg.remove();
    });
    this.layers.clear();
    this.images.clear();
    this.host.removeEventListener("embedpdf:page-render", this.renderEvent);
    this.host.removeEventListener("embedpdf:page-layer", this.layerEvent);
    this.host.removeEventListener("keydown", this.key, true);
  }
}
