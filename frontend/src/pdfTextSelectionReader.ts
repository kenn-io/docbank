import type {
  PluginRegistry,
  DocumentManagerPlugin,
  SelectionPlugin,
} from "@embedpdf/svelte-pdf-viewer";
import { PdfErrorCode, type PdfTask } from "@embedpdf/models";
import { textSelectionLimits, type TextSelection } from "./pdfTextSelection.js";

// SDK character indexes are used only for SDK reads, never for the native map.
export async function readTextSelection(
  registry: PluginRegistry,
  signal: AbortSignal,
): Promise<TextSelection> {
  const doc = registry
    .getPlugin<DocumentManagerPlugin>("document-manager")!
    .provides()
    .getActiveDocument();
  if (!doc) throw new Error("Text selection is unavailable. Draw a rectangle instead.");
  const scope = registry.getPlugin<SelectionPlugin>("selection")!.provides().forDocument(doc.id);
  const state = scope.getState(),
    range = state.selection;
  if (!range) throw new Error("Select a passage first.");
  if (range.start.page !== range.end.page) throw new Error("Select text on one page at a time.");
  const page = range.start.page,
    from = Math.min(range.start.index, range.end.index),
    to = Math.max(range.start.index, range.end.index) + 1;
  if (to - from > textSelectionLimits.glyphs) throw new Error("Select a shorter passage.");
  const geometry = state.geometry[page];
  if (!geometry) throw new Error("Text selection is unavailable. Draw a rectangle instead.");
  const count = geometry.runs.reduce((n, r) => Math.max(n, r.charStart + r.glyphs.length), 0);
  if (count > textSelectionLimits.pageBytes)
    throw new Error("This page is too large for text selection. Draw a rectangle instead.");
  const runs = geometry.runs.filter(
    (r) =>
      r.charStart < to &&
      r.charStart + r.glyphs.length > from &&
      !r.glyphs.every((g) => g.flags & 2),
  );
  if (!runs.length) throw new Error("Select a passage first.");
  if (
    runs.length > textSelectionLimits.contexts ||
    runs.reduce((n, r) => n + r.glyphs.length, 0) > textSelectionLimits.contextBytes
  )
    throw new Error("This passage is too large. Draw a rectangle instead.");
  const wait = async <T>(task: PdfTask<T>) => {
    const abort = () =>
      task.abort({ code: PdfErrorCode.Cancelled, message: "Selection canceled." });
    signal.addEventListener("abort", abort, { once: true });
    try {
      if (signal.aborted) abort();
      const result = await task.toPromise();
      signal.throwIfAborted();
      return result;
    } finally {
      signal.removeEventListener("abort", abort);
    }
  };
  // This operation enforces copy permission before any direct engine text read.
  const permission = await wait(scope.getSelectedText());
  if (!permission.some((text) => text.trim())) throw new Error("Select a passage first.");
  const slices: { pageIndex: number; charIndex: number; charCount: number }[] = [];
  const slice = (start: number, length: number) => {
    if (!length) return -1;
    slices.push({ pageIndex: page, charIndex: start, charCount: length });
    return slices.length - 1;
  };
  const whole = slice(0, count);
  let previous = from;
  const parts = runs.map((run) => {
    const start = Math.max(from, run.charStart),
      end = Math.min(to, run.charStart + run.glyphs.length);
    const separator = slice(previous, start - previous);
    previous = end;
    return {
      full: slice(run.charStart, run.glyphs.length),
      prefix: slice(run.charStart, start - run.charStart),
      selected: slice(start, end - start),
      suffix: slice(end, run.charStart + run.glyphs.length - end),
      separator,
    };
  });
  const tail = slice(previous, to - previous);
  const values = await wait(registry.getEngine().getTextSlices(doc, slices));
  if (values.length !== slices.length)
    throw new Error("Text selection is unavailable. Draw a rectangle instead.");
  const value = (i: number) => (i < 0 ? "" : values[i]!);
  // Empty generated runs can separate real runs, but must not hide selected text.
  for (const i of [parts[0]!.separator, tail, ...parts.slice(1).map((p) => p.separator)]) {
    if (!/^\p{White_Space}*$/u.test(value(i)))
      throw new Error("This text could not be matched uniquely. Draw a rectangle instead.");
  }
  return {
    page: page + 1,
    browserText: value(whole),
    contexts: parts.map((p) => ({
      full: value(p.full),
      prefix: value(p.prefix),
      selected: value(p.selected),
      suffix: value(p.suffix),
      separatorBefore: value(p.separator),
    })),
  };
}
