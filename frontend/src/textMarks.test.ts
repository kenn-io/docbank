import { expect, it } from "vitest";
import { markText } from "./textMarks.js";

it("uses deterministic source priority for overlaps and preserves UTF-16 offsets", () => {
  const result = markText("😀ALPHA alphabet", {
    find: "😀a",
    highlightTerms: [{ text: "alphabet", color: "#00ff00" }, { text: "alpha", color: "#ff0000" }],
    queryTerms: ["alpha"],
  });

  expect(result.matches.map(({ start, end, source, text }) => ({ start, end, source, text }))).toEqual([
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
