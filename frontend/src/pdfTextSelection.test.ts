import { describe, expect, it } from "vite-plus/test";
import { readFileSync } from "node:fs";
import type { RedactionPreparation, RedactionMask, PDFRedactionBox } from "./generated/docbank.js";
import { matchTextSelection, maskBoxes, checkMaskBudget } from "./pdfTextSelection.js";

const base = JSON.parse(
  readFileSync("../cmd/docbank/testdata/redaction-preparation.json", "utf8"),
) as RedactionPreparation;
const box: PDFRedactionBox = {
  page: 1,
  frame_sha256: base.map.pages[0]!.frame_sha256,
  x0: 100,
  y0: 200,
  x1: 300,
  y1: 400,
};
function preparation(text: string): RedactionPreparation {
  let offset = 0;
  const atoms = Array.from(text).flatMap((c) => {
    const start = offset;
    offset += new TextEncoder().encode(c).length;
    return /\s/u.test(c) ? [] : [{ span: { start, end: offset }, boxes: [{ ...box }] }];
  });
  return {
    ...base,
    map: {
      ...base.map,
      text,
      pages: [{ ...base.map.pages[0]!, span: { start: 0, end: offset } }],
      atoms,
      gaps: [],
    },
  };
}
const context = (
  full: string,
  prefix: string,
  selected: string,
  suffix = "",
  separatorBefore = "",
) => ({ full, prefix, selected, suffix, separatorBefore });

describe("native text selection", () => {
  it("matches an accented partial selection at literal UTF-8 offsets", () => {
    const text = "café naïve résumé";
    expect(
      matchTextSelection(preparation(text), {
        page: 1,
        browserText: text,
        contexts: [context(text, "café naïve ", "résumé")],
      }),
    ).toEqual([{ start: 13, end: 21 }]);
  });
  it("preserves a literal BOM when calculating native byte offsets", () => {
    const text = "\uFEFFAAAAAAAAAB",
      browserText = "AAAAAAAAAB";
    expect(
      matchTextSelection(preparation(text), {
        page: 1,
        browserText,
        contexts: [context(browserText, "AAA", "AAA", "AAAB")],
      }),
    ).toEqual([{ start: 6, end: 9 }]);
  });
  it("joins only the identical selected line separator", () => {
    const browserText = "KEEP FIRST SECRET\r\nSECRET KEEP SECOND";
    const contexts = [
      context("KEEP FIRST SECRET", "KEEP FIRST ", "SECRET"),
      context("SECRET KEEP SECOND", "", "SECRET", " KEEP SECOND", "\r\n"),
    ];
    expect(
      matchTextSelection(preparation(browserText), { page: 1, browserText, contexts }),
    ).toEqual([{ start: 11, end: 25 }]);
    const reversed = "SECRET KEEP SECOND\r\nKEEP FIRST SECRET";
    expect(matchTextSelection(preparation(reversed), { page: 1, browserText, contexts })).toEqual([
      { start: 0, end: 6 },
      { start: 31, end: 37 },
    ]);
    expect(
      matchTextSelection(preparation(browserText.replace("\r\n", "\n")), {
        page: 1,
        browserText,
        contexts,
      }),
    ).toEqual([
      { start: 11, end: 17 },
      { start: 18, end: 24 },
    ]);
  });
  it("refuses a browser duplicate even when crop filtering left one native match", () => {
    const full = "KEEP FIRST KEEP FIRST SECRET";
    expect(() =>
      matchTextSelection(preparation(full), {
        page: 1,
        browserText: `${full}\r\n${full}`,
        contexts: [context(full, "KEEP FIRST KEEP FIRST ", "SE", "CRET")],
      }),
    ).toThrow(/matched uniquely/);
  });
  it("refuses the entire addition if any context disagrees or is ambiguous", () => {
    const first = context("FIRST SECRET", "FIRST ", "SECRET");
    for (const text of [
      "FIRST SECRET SECOND DIFFERENT",
      "FIRST SECRET SECOND SECRET SECOND SECRET",
    ]) {
      expect(() =>
        matchTextSelection(preparation(text), {
          page: 1,
          browserText: "FIRST SECRET SECOND SECRET",
          contexts: [first, context("SECOND SECRET", "SECOND ", "SECRET", "", " ")],
        }),
      ).toThrow();
    }
  });
  it("does not accept broken concatenation, Unicode or atom boundaries", () => {
    const p = preparation("office");
    p.map.atoms = [{ span: { start: 0, end: 2 }, boxes: [box] }, ...p.map.atoms.slice(2)];
    expect(() =>
      matchTextSelection(p, {
        page: 1,
        browserText: "office",
        contexts: [context("office", "", "o", "ffice")],
      }),
    ).toThrow();
    expect(() =>
      matchTextSelection(p, {
        page: 1,
        browserText: "office",
        contexts: [context("office", "", "other", "")],
      }),
    ).toThrow();
    expect(() =>
      matchTextSelection(p, {
        page: 1,
        browserText: "office",
        contexts: [context("office", "\ud800", "o", "ffice")],
      }),
    ).toThrow();
    expect(() =>
      matchTextSelection(preparation(" "), {
        page: 1,
        browserText: " ",
        contexts: [context(" ", "", " ")],
      }),
    ).toThrow();
  });
  it("keeps page authority and input-work caps", () => {
    const p = preparation("SECRET");
    expect(() =>
      matchTextSelection(p, {
        page: 2,
        browserText: "SECRET",
        contexts: [context("SECRET", "", "SECRET")],
      }),
    ).toThrow();
    expect(() =>
      matchTextSelection(p, {
        page: 1,
        browserText: "SECRET",
        contexts: Array(33).fill(context("SECRET", "", "SECRET")),
      }),
    ).toThrow();
    expect(() =>
      matchTextSelection(p, {
        page: 1,
        browserText: "x".repeat((1 << 20) + 1),
        contexts: [context("SECRET", "", "SECRET")],
      }),
    ).toThrow();
    const overGlyphs = "A".repeat(4097);
    expect(() =>
      matchTextSelection(preparation(overGlyphs), {
        page: 1,
        browserText: overGlyphs,
        contexts: [context(overGlyphs, "", overGlyphs)],
      }),
    ).toThrow(/shorter passage/);
    const full = "A".repeat((64 << 10) + 1);
    expect(() =>
      matchTextSelection(preparation(full), {
        page: 1,
        browserText: full,
        contexts: [context(full, "", "A", full.slice(1))],
      }),
    ).toThrow();
  });
  it("can add over 1,024 selected glyphs without consuming input boxes", () => {
    const text = "A".repeat(1025),
      p = preparation(text);
    const spans = matchTextSelection(p, {
      page: 1,
      browserText: text,
      contexts: [context(text, "", text)],
    });
    expect(spans).toEqual([{ start: 0, end: 1025 }]);
    const masks = spans.map((span) => ({
      id: "00000000-0000-4000-8000-000000000001",
      selector: { kind: "text", map_sha256: p.map.sha256, span },
    }));
    expect(() => checkMaskBudget(masks)).not.toThrow();
    expect(masks[0]!.selector).not.toHaveProperty("boxes");
  });
  it("draws native atom boxes rather than a hull", () => {
    const p = preparation("A B");
    p.map.atoms[1]!.boxes = [{ ...box, x0: 700, x1: 900 }];
    expect(
      maskBoxes(p, { kind: "text", map_sha256: p.map.sha256, span: { start: 0, end: 3 } }),
    ).toEqual([box, { ...box, x0: 700, x1: 900 }]);
  });
  it("refuses shared mask and box budgets before any addition", () => {
    const mask: RedactionMask = {
      id: "00000000-0000-4000-8000-000000000001",
      selector: { kind: "rectangle", map_sha256: base.map.sha256, boxes: [box] },
    };
    expect(() => checkMaskBudget(Array(257).fill(mask))).toThrow(/Remove masks/);
    expect(() =>
      checkMaskBudget([{ ...mask, selector: { ...mask.selector, boxes: Array(1025).fill(box) } }]),
    ).toThrow();
  });
});
