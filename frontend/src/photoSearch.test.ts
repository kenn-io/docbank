import { expect, it } from "vite-plus/test";
import { photoSearchSort } from "./photoSearch.js";

it("chooses photo search order", () => {
  for (const [syntax, text, field] of [
    ["advanced", "rating:5 pick", "capture_time"],
    ["simple", "pick", "relevance"],
    ["simple", "  ", "capture_time"],
  ] as const) {
    expect(photoSearchSort({ syntax, text }).field).toBe(field);
  }
});
