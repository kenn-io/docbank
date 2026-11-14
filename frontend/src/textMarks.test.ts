import { expect, it } from "vite-plus/test";
import { markText, nextTextMatch } from "./textMarks.js";

const supportsNativeHegel = (platform: string, arch: string): boolean =>
  platform !== "darwin" || arch !== "x64";

const hegelIt = supportsNativeHegel(process.platform, process.arch) ? it : it.skip;

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

hegelIt("wraps match navigation in both directions for every bounded match count", async () => {
  const [hegel, gs] = await Promise.all([
    import("@hegeldev/hegel"),
    import("@hegeldev/hegel/generators"),
  ]);

  await hegel.test(
    (tc) => {
      const count = tc.draw(gs.integers({ minValue: 1, maxValue: 5000 }));
      const index = tc.draw(gs.integers({ minValue: 0, maxValue: count - 1 }));
      const next = nextTextMatch(index, 1, count);
      const previous = nextTextMatch(index, -1, count);
      expect(next).toBeGreaterThanOrEqual(0);
      expect(next).toBeLessThan(count);
      expect(nextTextMatch(next, -1, count)).toBe(index);
      expect(nextTextMatch(previous, 1, count)).toBe(index);
      expect(nextTextMatch(count - 1, 1, count)).toBe(0);
      expect(nextTextMatch(0, -1, count)).toBe(count - 1);
    },
    { testCases: 2000, database: hegel.Database.disabled },
  );
});

hegelIt("preserves arbitrary Unicode context around literal search terms", async () => {
  const [hegel, gs] = await Promise.all([
    import("@hegeldev/hegel"),
    import("@hegeldev/hegel/generators"),
  ]);

  await hegel.test(
    (tc) => {
      const before = tc.draw(gs.text({ maxSize: 500 }));
      const term = tc.draw(gs.text({ minSize: 1, maxSize: 30 }));
      const after = tc.draw(gs.text({ maxSize: 500 }));
      const text = `${before}\n${term}\n${after}`;
      const marked = markText(text, { queryTerms: [term] });
      expect(marked.segments.map((segment) => segment.text).join("")).toBe(text);
      expect(marked.matches.length).toBeGreaterThan(0);
      for (const match of marked.matches) {
        expect(text.slice(match.start, match.end)).toBe(match.text);
        expect(match.source).toBe("query");
      }
    },
    { testCases: 2000, database: hegel.Database.disabled },
  );
});

it("keeps navigation at zero when no text matches are available", () => {
  expect(nextTextMatch(0, 1, 0)).toBe(0);
  expect(nextTextMatch(0, -1, 0)).toBe(0);
});
