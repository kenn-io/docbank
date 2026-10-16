import { expect, it } from "vite-plus/test";
import { hasPersonFilter, photoSearchSort } from "./photoSearch.js";

it("chooses photo search order", () => {
  for (const [syntax, text, field] of [
    ["advanced", "rating:5 pick", "capture_time"],
    ["simple", "pick", "relevance"],
    ["simple", "  ", "capture_time"],
  ] as const) {
    expect(photoSearchSort({ syntax, text }).field).toBe(field);
  }
});

it.each([
  ['person:"Ada Lovelace"', true],
  ["(person : Ada) OR person:Grace", true],
  ['"person:Ada"', false],
  ['"a \\"quoted\\" person:Ada phrase"', false],
  ['caption:"person:Ada"', false],
  ["otherperson:Ada", false],
  ["\\person:Ada", true],
])("recognizes person filters in %s", (text, expected) => {
  expect(hasPersonFilter({ syntax: "advanced", text, filters: {} })).toBe(expected);
});

it("uses explicit people in either syntax and treats simple text as literal", () => {
  expect(hasPersonFilter({ syntax: "simple", text: "person:Ada", filters: {} })).toBe(false);
  expect(
    hasPersonFilter({
      syntax: "simple",
      text: '"person:Ada"',
      filters: { person_ids: ["person-a"] },
    }),
  ).toBe(true);
});
