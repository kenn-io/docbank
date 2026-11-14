import type {
  PDFRedactionBox,
  PDFRedactionSelector,
  PDFRedactionSpan,
  RedactionMask,
  RedactionPreparation,
} from "./generated/docbank.js";

export interface TextSelectionContext {
  full: string;
  prefix: string;
  selected: string;
  suffix: string;
  separatorBefore: string;
}
export interface TextSelection {
  page: number;
  browserText: string;
  contexts: TextSelectionContext[];
}
export const textSelectionLimits = {
  contexts: 32,
  glyphs: 4096,
  contextBytes: 64 << 10,
  pageBytes: 1 << 20,
};
const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const whitespace = /^\p{White_Space}*$/u;
function unmatched(): never {
  throw new Error("This text could not be matched uniquely. Draw a rectangle instead.");
}
function bounded(value: string, limit: number) {
  const bytes = encoder.encode(value);
  if (bytes.length > limit) throw new Error("This passage is too large. Draw a rectangle instead.");
  if (decoder.decode(bytes) !== value) unmatched();
  return bytes;
}
function unique(text: string, context: string) {
  const at = text.indexOf(context);
  if (!context || at < 0 || text.indexOf(context, at + 1) >= 0) unmatched();
  return at;
}

// Browser indexes never become native offsets; only literal text binds the two extracts.
export function matchTextSelection(
  prep: RedactionPreparation,
  selection: TextSelection,
): PDFRedactionSpan[] {
  const page = prep.map.pages[selection.page - 1];
  if (!page || !selection.contexts.length) unmatched();
  if (selection.contexts.length > textSelectionLimits.contexts)
    throw new Error("Select a shorter passage.");
  bounded(selection.browserText, textSelectionLimits.pageBytes);
  const nativeBytes = encoder.encode(prep.map.text);
  const native = decoder.decode(nativeBytes.subarray(page.span.start, page.span.end));
  bounded(native, textSelectionLimits.pageBytes);
  let contextBytes = 0,
    glyphs = 0,
    hasAtoms = false,
    previousBrowserEnd = -1;
  const mapped = selection.contexts.map((c, order) => {
    contextBytes += bounded(c.full, textSelectionLimits.contextBytes).length;
    contextBytes += bounded(c.separatorBefore, textSelectionLimits.contextBytes).length;
    if (contextBytes > textSelectionLimits.contextBytes)
      throw new Error("This passage is too large. Draw a rectangle instead.");
    for (const text of [c.prefix, c.selected, c.suffix])
      bounded(text, textSelectionLimits.contextBytes);
    if (c.full !== c.prefix + c.selected + c.suffix || !c.selected) unmatched();
    glyphs += Array.from(c.selected).length;
    if (glyphs > textSelectionLimits.glyphs) throw new Error("Select a shorter passage.");
    const browserStart = unique(selection.browserText, c.full) + c.prefix.length;
    if (browserStart < previousBrowserEnd) unmatched();
    if (
      order &&
      selection.browserText.slice(previousBrowserEnd, browserStart) !== c.separatorBefore
    )
      unmatched();
    previousBrowserEnd = browserStart + c.selected.length;
    const at = unique(native, c.full);
    const start = page.span.start + encoder.encode(native.slice(0, at) + c.prefix).length;
    const end = start + encoder.encode(c.selected).length;
    if (start < page.span.start || end > page.span.end) unmatched();
    const actual = decoder.decode(nativeBytes.subarray(start, end));
    if (actual !== c.selected) unmatched();
    for (const atom of prep.map.atoms) {
      if (atom.span.end <= start || atom.span.start >= end) continue;
      if (atom.span.start < start || atom.span.end > end) unmatched();
      hasAtoms = true;
    }
    return { start, end, order, separator: c.separatorBefore };
  });
  if (!hasAtoms) unmatched();
  mapped.sort((a, b) => a.start - b.start);
  const spans: PDFRedactionSpan[] = [];
  let previousOrder = -1;
  for (const current of mapped) {
    const last = spans.at(-1);
    const gap = last ? decoder.decode(nativeBytes.subarray(last.end, current.start)) : "";
    const selectedGap =
      current.order === previousOrder + 1 &&
      current.separator.length > 0 &&
      whitespace.test(current.separator) &&
      gap === current.separator;
    if (last && (current.start <= last.end || selectedGap))
      last.end = Math.max(last.end, current.end);
    else spans.push({ start: current.start, end: current.end });
    previousOrder = current.order;
  }
  return spans;
}

export function maskBoxes(
  prep: RedactionPreparation,
  selector: PDFRedactionSelector,
): PDFRedactionBox[] {
  if (selector.kind === "text" && selector.span) {
    const span = selector.span;
    return prep.map.atoms
      .filter((a) => a.span.start < span.end && a.span.end > span.start)
      .flatMap((a) => a.boxes);
  }
  if (selector.kind === "page")
    return (selector.pages ?? []).map((n) => {
      const p = prep.map.pages[n - 1]!;
      return { page: n, frame_sha256: p.frame_sha256, x0: 0, y0: 0, x1: p.width, y1: p.height };
    });
  return selector.boxes ?? [];
}

export function checkMaskBudget(masks: RedactionMask[]) {
  if (masks.length > 256) throw new Error("Remove masks before adding more.");
  const boxes = masks.reduce((n, m) => n + (m.selector.boxes?.length ?? 0), 0);
  const pages = masks.reduce((n, m) => n + (m.selector.pages?.length ?? 0), 0);
  if (boxes > 1024 || pages > 1024)
    throw new Error("This mask has too many regions. Use a whole-page mask instead.");
  if (encoder.encode(JSON.stringify(masks)).length > 1 << 20)
    throw new Error("Remove masks before adding more.");
}
