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
} from "@embedpdf/svelte-pdf-viewer";
import { ZoomMode } from "@embedpdf/svelte-pdf-viewer";
import type {
  RedactionPreparation,
  PDFRedactionBox,
  PDFRedactionGap,
} from "./generated/docbank.js";
import { bindReaderPages, readerBox, overlayBox } from "./pdfReviewGeometry.js";

export interface ReaderReview {
  preparation?: RedactionPreparation;
  boxes: PDFRedactionBox[];
  gaps: PDFRedactionGap[];
  proposed: PDFRedactionBox[];
  drawing: boolean;
  visible: boolean;
  oncommit: (box: PDFRedactionBox) => void;
  ondrag: (active: boolean) => void;
  ondrawing: (active: boolean) => void;
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
    zoom: number;
    rotation: number;
    x: number;
    y: number;
  };
  private updating = false;
  private settingMode = false;
  private pointerActive = false;
  private lastStatus = "";
  private frame = 0;
  constructor(
    private host: HTMLElement,
    private changed: (status: ReaderReviewStatus) => void,
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
  ready(registry: PluginRegistry) {
    this.registry = registry;
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
        this.pointerActive = false;
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
          this.review.ondrag(false);
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
          this.cancelDraft();
          this.update();
        }),
    );
    this.subscriptions.push(this.manager()!.onDocumentOpened(() => this.update()));
    this.subscriptions.push(
      registry
        .getPlugin<ZoomPlugin>("zoom")!
        .provides()
        .onZoomChange(() => this.cancelDraft()),
    );
    this.subscriptions.push(
      registry
        .getPlugin<RotatePlugin>("rotate")!
        .provides()
        .onRotateChange(() => this.cancelDraft()),
    );
    this.subscriptions.push(
      registry
        .getPlugin<ViewportPlugin>("viewport")!
        .provides()
        .onViewportChange(() => this.cancelDraft()),
    );
    this.update();
  }
  configure(review?: ReaderReview) {
    const leaving = !review && !!this.review;
    this.review = review;
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
          if (!this.disposed && !this.review)
            this.registry!.getPlugin<ViewportPlugin>("viewport")!
              .provides()
              .forDocument(id)
              .scrollTo({ x: saved.x, y: saved.y, behavior: "instant" });
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
      const metrics = viewport.getMetrics();
      this.saved = {
        zoom: this.zoom()!.getState().currentZoomLevel,
        rotation: rotation.getRotation(),
        x: metrics.scrollLeft,
        y: metrics.scrollTop,
      };
      this.zoom()!.requestZoom(ZoomMode.FitPage);
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
  private cancelDraft() {
    if (!this.pointerActive && ![...this.layers.values()].some((l) => l.start)) return;
    this.pointerActive = false;
    for (const layer of this.layers.values()) layer.start = undefined;
    this.setDrawingMode(false);
    this.review?.ondrag(false);
    this.update();
  }
  private renderEvent = (raw: Event) => {
    const event = (raw as CustomEvent<ImageEvent>).detail;
    if (event.status === "disposed") this.images.delete(event.token);
    else this.images.set(event.token, event);
    if (event.status !== "ready" && this.review?.drawing) {
      this.pointerActive = false;
      this.setDrawingMode(false);
      this.review.ondrag(false);
      for (const layer of this.layers.values()) layer.start = undefined;
    }
    this.update();
  };
  private layerEvent = (raw: Event) => {
    const event = (raw as CustomEvent<LayerEvent>).detail,
      old = this.layers.get(event.target);
    if (old) {
      old.remove();
      old.svg.remove();
      this.layers.delete(event.target);
    }
    if (event.status === "mounted") {
      const svg = document.createElementNS(svgNS, "svg");
      svg.style.cssText =
        "position:absolute;inset:0;width:100%;height:100%;pointer-events:none;overflow:visible";
      event.target.append(svg);
      event.target.setAttribute("role", "group");
      event.target.setAttribute("aria-label", `Page ${event.pageIndex + 1} review layer`);
      const surface = event.target.parentElement!;
      const down = (e: PointerEvent) => {
        if (!this.review?.drawing) return;
        if (!this.review.visible || !this.isReady(event.pageIndex)) {
          e.preventDefault();
          e.stopImmediatePropagation();
          return;
        }
        this.pointerActive = true;
        this.review.ondrag(true);
        this.host.focus({ preventScroll: true });
      };
      surface.addEventListener("pointerdown", down, true);
      const up = () => {
        this.pointerActive = false;
        this.review?.ondrag(false);
      };
      window.addEventListener("pointerup", up);
      this.layers.set(event.target, {
        event,
        svg,
        cursor: { x: 0, y: 0 },
        remove: () => {
          surface.removeEventListener("pointerdown", down, true);
          window.removeEventListener("pointerup", up);
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
      for (const b of this.review.boxes) if (b.page === index + 1) rect(b, "#111", "#00000040");
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
        rect(
          {
            x0: Math.max(0, cursor.x - 50),
            y0: Math.max(0, cursor.y - 50),
            x1: Math.min(frame.width, cursor.x + 50),
            y1: Math.min(frame.height, cursor.y + 50),
          },
          "#0076d6",
          "#0076d6",
        );
      } else layer.start = undefined;
    }
  }
  private key = (event: KeyboardEvent) => {
    const review = this.review,
      prep = this.bound;
    if (
      !review?.drawing ||
      !review.visible ||
      !prep ||
      event.ctrlKey ||
      event.metaKey ||
      event.altKey
    )
      return;
    if (
      event
        .composedPath()
        .some(
          (x) =>
            x instanceof HTMLElement && (x.matches("input,textarea,select") || x.isContentEditable),
        )
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
      layer.cursor = {
        x: Math.max(
          0,
          Math.min(
            frame.width,
            layer.cursor.x +
              (event.key === "ArrowRight" ? step : event.key === "ArrowLeft" ? -step : 0),
          ),
        ),
        y: Math.max(
          0,
          Math.min(
            frame.height,
            layer.cursor.y +
              (event.key === "ArrowDown" ? step : event.key === "ArrowUp" ? -step : 0),
          ),
        ),
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
    } else if (event.key === "Escape" && (layer.start || this.pointerActive)) {
      event.preventDefault();
      event.stopImmediatePropagation();
      layer.start = undefined;
      this.pointerActive = false;
      review.ondrag(false);
      this.setDrawingMode(false);
      this.update();
    }
    this.paint();
  };
  focus() {
    this.host.focus({ preventScroll: true });
  }
  dispose() {
    this.disposed = true;
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
