import { APIError, sessionJSON, sessionResponse } from "./api-transport.js";
import { decodePageFrame, pageDigest, type PageBinding } from "./pages.js";
import type {
  PDFRedactionBox,
  PDFRedactionPage,
  RedactionPreparation,
  RedactionMask,
  RedactionReview,
  RedactionPreview,
  PDFRedactionSelector,
} from "./generated/docbank.js";

export const redactionRoot = "/api/v1/redaction";
export const maxRedactionJSON = 16 << 20;
export function requireRedaction(condition: unknown): asserts condition {
  if (!condition) throw new Error("The redaction response disagrees with this document or review.");
}
export function redactionObject(
  value: unknown,
  keys: string[],
  optional: string[] = [],
): Record<string, unknown> {
  requireRedaction(!!value && typeof value === "object" && !Array.isArray(value));
  const o = value as Record<string, unknown>;
  requireRedaction(
    keys.every((k) => Object.hasOwn(o, k)) &&
      Object.keys(o).every((k) => keys.includes(k) || optional.includes(k)),
  );
  return o;
}
export function redactionInteger(value: unknown, min = 0, max = Number.MAX_SAFE_INTEGER): number {
  requireRedaction(
    typeof value === "number" && Number.isSafeInteger(value) && value >= min && value <= max,
  );
  return value;
}
export function redactionHash(value: unknown): string {
  requireRedaction(typeof value === "string" && /^[0-9a-f]{64}$/.test(value));
  return value;
}
function array(value: unknown, max = 1000000): unknown[] {
  requireRedaction(Array.isArray(value) && value.length <= max);
  return value;
}
const same = (a: unknown, b: unknown) => pageDigest(a) === pageDigest(b);
function span(value: unknown, max: number) {
  const o = redactionObject(value, ["start", "end"]);
  redactionInteger(o.start, 0, max);
  redactionInteger(o.end, o.start as number, max);
}
export function checkRedactionBox(value: unknown, pages: PDFRedactionPage[]): PDFRedactionBox {
  const o = redactionObject(value, ["page", "frame_sha256", "x0", "y0", "x1", "y1"]);
  const p = pages[redactionInteger(o.page, 1, pages.length) - 1]!;
  requireRedaction(o.frame_sha256 === p.frame_sha256);
  const x0 = redactionInteger(o.x0, 0, p.width),
    y0 = redactionInteger(o.y0, 0, p.height);
  redactionInteger(o.x1, x0 + 1, p.width);
  redactionInteger(o.y1, y0 + 1, p.height);
  return o as unknown as PDFRedactionBox;
}
function checkPages(value: unknown): PDFRedactionPage[] {
  const pages = array(value, 16);
  requireRedaction(pages.length > 0);
  for (const [i, p] of pages.entries()) {
    const o = redactionObject(p, ["number", "frame_sha256", "width", "height", "span"]);
    requireRedaction(o.number === i + 1);
    redactionHash(o.frame_sha256);
    redactionInteger(o.width, 1, 1 << 30);
    redactionInteger(o.height, 1, 1 << 30);
    span(o.span, maxRedactionJSON);
  }
  return pages as PDFRedactionPage[];
}

export function decodePreparation(value: unknown, expected: PageBinding): RedactionPreparation {
  const o = redactionObject(
    value,
    ["contract", "selection", "frames", "evidence", "evidence_sha256", "map"],
    ["$schema"],
  );
  requireRedaction(o.contract === "native-pdf-preparation/v1" && same(o.selection, expected));
  const map = redactionObject(o.map, [
    "contract",
    "sha256",
    "pdf_sha256",
    "evidence_sha256",
    "text",
    "pages",
    "atoms",
    "units",
    "gaps",
  ]);
  requireRedaction(
    map.contract === "aligned-text/v1" &&
      map.pdf_sha256 === expected.source.sha256 &&
      map.evidence_sha256 === o.evidence_sha256 &&
      typeof map.text === "string",
  );
  const { sha256: mapHash, ...mapIdentity } = map;
  requireRedaction(redactionHash(mapHash) === pageDigest(mapIdentity));
  const pages = checkPages(map.pages),
    frames = array(o.frames, 16);
  requireRedaction(frames.length === pages.length);
  frames.forEach((f, i) => {
    const frame = decodePageFrame(f, expected.source, i + 1),
      p = pages[i]!;
    requireRedaction(
      pageDigest(frame) === p.frame_sha256 && frame.width === p.width && frame.height === p.height,
    );
  });
  const textSize = new TextEncoder().encode(map.text as string).length;
  for (const atom of array(map.atoms)) {
    const a = redactionObject(atom, ["span", "boxes"]);
    span(a.span, textSize);
    array(a.boxes).forEach((b) => checkRedactionBox(b, pages));
  }
  requireRedaction(array(map.units).length === 0);
  for (const gap of array(map.gaps)) {
    const g = redactionObject(gap, ["box", "anchor"], ["unordered"]);
    checkRedactionBox(g.box, pages);
    redactionInteger(g.anchor, 0, textSize);
    requireRedaction(g.unordered === undefined || typeof g.unordered === "boolean");
  }
  const evidence = redactionObject(o.evidence, [
    "contract",
    "source",
    "page_runtime_fingerprint",
    "renderer_sha256",
    "pages",
  ]);
  requireRedaction(
    evidence.contract === "native-pdf-evidence/v1" && same(evidence.source, expected.source),
  );
  redactionHash(evidence.page_runtime_fingerprint);
  redactionHash(evidence.renderer_sha256);
  requireRedaction(redactionHash(o.evidence_sha256) === pageDigest(evidence));
  const observations = array(evidence.pages, 16);
  requireRedaction(observations.length === pages.length);
  for (const [i, p] of observations.entries()) {
    const e = redactionObject(p, ["number", "glyphs", "non_text_bounds"]);
    requireRedaction(e.number === i + 1);
    for (const g of array(e.glyphs)) {
      const glyph = redactionObject(g, ["text", "bounds", "gap_reason"]);
      requireRedaction(typeof glyph.text === "string" && typeof glyph.gap_reason === "string");
      requireRedaction(array(glyph.bounds, 4).length === 4);
      (glyph.bounds as unknown[]).forEach((n) => redactionInteger(n, -Number.MAX_SAFE_INTEGER));
    }
    for (const b of array(e.non_text_bounds)) {
      requireRedaction(array(b, 4).length === 4);
      (b as unknown[]).forEach((n) => redactionInteger(n, -Number.MAX_SAFE_INTEGER));
    }
  }
  const { $schema: _, ...prep } = o;
  return prep as unknown as RedactionPreparation;
}

export function decodeReview(
  value: unknown,
  prep: RedactionPreparation,
  masks: RedactionMask[],
): RedactionReview {
  const o = redactionObject(
    value,
    [
      "contract",
      "selection",
      "observed_node_revision",
      "preparation_sha256",
      "decisions_sha256",
      "recipe",
      "resolved",
      "sha256",
    ],
    ["$schema"],
  );
  const selection = { node_id: prep.selection.node_id, source: prep.selection.source };
  requireRedaction(o.contract === "native-pdf-redaction-review/v1" && same(o.selection, selection));
  redactionInteger(o.observed_node_revision, 1);
  requireRedaction(o.preparation_sha256 === pageDigest({ ...prep, selection }));
  const decisions = masks
    .map((m) => ({
      id: m.id,
      member_id: selection.source.version_id,
      action: "redact",
      reason: "",
      label: "",
      uncertain: false,
      selector: m.selector,
    }))
    .sort((a, b) => a.id.localeCompare(b.id));
  requireRedaction(o.decisions_sha256 === pageDigest(decisions));
  const recipe = redactionObject(o.recipe, [
    "contract",
    "dpi",
    "font_sha256",
    "max_axis",
    "max_pixels",
    "max_staging_bytes",
    "padding_pixels",
    "page_timeout_seconds",
    "qualified_peak_rss_bytes",
    "renderer_sha256",
    "wasm_memory_bytes",
    "writer_version",
  ]);
  requireRedaction(recipe.contract === "raster-redaction/v1");
  redactionHash(recipe.font_sha256);
  redactionHash(recipe.renderer_sha256);
  for (const key of [
    "dpi",
    "max_axis",
    "max_pixels",
    "max_staging_bytes",
    "padding_pixels",
    "page_timeout_seconds",
    "qualified_peak_rss_bytes",
    "wasm_memory_bytes",
  ])
    redactionInteger(recipe[key]);
  requireRedaction(recipe.dpi === 300 && typeof recipe.writer_version === "string");
  const resolved = redactionObject(o.resolved, [
    "contract",
    "sha256",
    "map_sha256",
    "recipe_sha256",
    "page_count",
    "pages",
    "gaps",
    "redact_boxes",
    "removed",
    "runs",
    "uncertain_decision_ids",
    "regions",
  ]);
  requireRedaction(
    resolved.contract === "redaction-plan/v1" &&
      resolved.map_sha256 === prep.map.sha256 &&
      resolved.recipe_sha256 === pageDigest(recipe),
  );
  const pages = checkPages(resolved.pages);
  requireRedaction(same(pages, prep.map.pages) && resolved.page_count === pages.length);
  array(resolved.redact_boxes).forEach((b) => checkRedactionBox(b, pages));
  requireRedaction(same(resolved.gaps, prep.map.gaps));
  array(resolved.removed);
  array(resolved.runs);
  array(resolved.uncertain_decision_ids);
  array(resolved.regions);
  const { sha256: resolvedSHA, ...resolvedIdentity } = resolved;
  requireRedaction(redactionHash(resolvedSHA) === pageDigest(resolvedIdentity));
  const { $schema: _, observed_node_revision: __, sha256: reviewSHA, ...identity } = o;
  requireRedaction(redactionHash(reviewSHA) === pageDigest(identity));
  const { $schema: ___, ...review } = o;
  return review as unknown as RedactionReview;
}

export function decodeExpansion(
  value: unknown,
  prep: RedactionPreparation,
  masks: RedactionMask[],
): RedactionMask {
  const o = redactionObject(
    value,
    ["code", "decision_ids", "expanded"],
    ["expanded_boxes", "expanded_map_sha256", "expanded_spans"],
  );
  requireRedaction(
    o.code === "selection_expansion_required" && o.expanded_map_sha256 === prep.map.sha256,
  );
  const ids = array(o.decision_ids, 1);
  requireRedaction(ids.length === 1 && masks.some((m) => m.id === ids[0]));
  const selector = redactionObject(
    o.expanded,
    ["kind", "map_sha256"],
    ["boxes", "pages", "span", "unit_id"],
  );
  requireRedaction(selector.map_sha256 === prep.map.sha256);
  if (selector.kind === "rectangle") {
    const boxes = array(selector.boxes, 1024);
    requireRedaction(boxes.length > 0);
    boxes.forEach((b) => checkRedactionBox(b, prep.map.pages));
    requireRedaction(
      selector.pages === undefined && selector.span === undefined && selector.unit_id === undefined,
    );
  } else if (selector.kind === "page") {
    const pages = array(selector.pages, 16);
    requireRedaction(pages.length > 0);
    pages.forEach((p) => redactionInteger(p, 1, prep.frames.length));
    requireRedaction(
      selector.boxes === undefined && selector.span === undefined && selector.unit_id === undefined,
    );
  } else requireRedaction(false);
  return { id: ids[0] as string, selector: selector as unknown as PDFRedactionSelector };
}

export async function prepareRedaction(
  session: string,
  selection: PageBinding,
  signal: AbortSignal,
) {
  return decodePreparation(
    await sessionJSON<unknown>(`${redactionRoot}/prepare`, {
      session,
      signal,
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ selection }),
      responseLimit: maxRedactionJSON,
    }),
    selection,
  );
}
export async function resolveRedaction(
  session: string,
  preparation: RedactionPreparation,
  masks: RedactionMask[],
  signal: AbortSignal,
) {
  return decodeReview(
    await sessionJSON<unknown>(`${redactionRoot}/resolve`, {
      session,
      signal,
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ preparation, masks }),
      responseLimit: maxRedactionJSON,
    }),
    preparation,
    masks,
  );
}
export async function discardRedaction(
  session: string,
  preview: RedactionPreview,
  keepalive = false,
) {
  try {
    await sessionResponse(`${redactionRoot}/previews/${preview.id}`, {
      session,
      method: "DELETE",
      keepalive,
    });
  } catch (cause) {
    if (!(cause instanceof APIError && cause.status === 410)) throw cause;
  }
}
