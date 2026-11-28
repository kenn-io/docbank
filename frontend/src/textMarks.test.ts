import { expect, it } from "vite-plus/test";
import { markText, nextTextMatch } from "./textMarks.js";

it("uses deterministic source priority for overlaps and preserves UTF-16 offsets", () => {
  const result = markText("😀ALPHA alphabet", {
    find: "😀a",
    highlightTerms: [
      { text: "alphabet", color: "#00ff00" },
      { text: "alpha", color: "#ff0000" },
    ],
    queryTerms: ["alpha"],
  });

  expect(
    result.matches.map(({ start, end, source, text }) => ({ start, end, source, text })),
  ).toEqual([
    { start: 0, end: 3, source: "find", text: "😀A" },
    { start: 8, end: 16, source: "highlight", text: "alphabet" },
  ]);
  expect(result.segments.map((segment) => segment.text).join("")).toBe("😀ALPHA alphabet");
});

it("uses Unicode regex case matching without normalizing offsets or combining sequences", () => {
  const result = markText("Cafe\u0301 CAFÉ", { queryTerms: ["café"] });
  expect(result.matches.map(({ start, end, text }) => ({ start, end, text }))).toEqual([
    { start: 6, end: 10, text: "CAFÉ" },
  ]);
});

it("bounds matches without fabricating partial success", () => {
  const result = markText("x ".repeat(5_010), { queryTerms: ["x"] });
  expect(result.matches).toHaveLength(5_000);
  expect(result.truncated).toBe(true);
});

it("wraps match navigation in both directions", () => {
  expect(nextTextMatch(0, 1, 3)).toBe(1);
  expect(nextTextMatch(2, 1, 3)).toBe(0);
  expect(nextTextMatch(0, -1, 3)).toBe(2);
  expect(nextTextMatch(1, -1, 3)).toBe(0);
  expect(nextTextMatch(0, 1, 1)).toBe(0);
  expect(nextTextMatch(0, -1, 1)).toBe(0);
  expect(nextTextMatch(4_999, 1, 5_000)).toBe(0);
});

it("preserves Unicode context around a literal search term", () => {
  const text = "前文 😀\nCafé RÉSUMÉ\n後文";
  const marked = markText(text, { queryTerms: ["résumé"] });
  expect(marked.segments.map((segment) => segment.text).join("")).toBe(text);
  expect(marked.matches).toHaveLength(1);
  const [match] = marked.matches;
  expect(text.slice(match!.start, match!.end)).toBe("RÉSUMÉ");
  expect(match!.source).toBe("query");
});

it("keeps navigation at zero when no text matches are available", () => {
  expect(nextTextMatch(0, 1, 0)).toBe(0);
  expect(nextTextMatch(0, -1, 0)).toBe(0);
});
