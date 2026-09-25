import { describe, expect, it } from "vitest";
import { utf16ToUTF8Offset, viewportBox } from "./embedpdfAdapter.js";

const frame = { page: 1, sha256: "a".repeat(64), width: 10000, height: 10000 };

describe("production editor selection adapter", () => {
  it("converts browser offsets without splitting Unicode", () => {
    expect(utf16ToUTF8Offset("A😀B", 0)).toBe(0);
    expect(utf16ToUTF8Offset("A😀B", 3)).toBe(5);
    expect(utf16ToUTF8Offset("A😀B", 4)).toBe(6);
    expect(() => utf16ToUTF8Offset("A😀B", 2)).toThrow();
  });

  it("rejects invalid text offsets and malformed surrogate pairs", () => {
    for (const offset of [-1, 1.5, 10])
      expect(() => utf16ToUTF8Offset("A😀B", offset)).toThrow();
    expect(() => utf16ToUTF8Offset("\ud800", 0)).toThrow();
    expect(() => utf16ToUTF8Offset("\udc00", 0)).toThrow();
  });

  it("pins a viewport selection to the exact physical frame", () => {
    expect(viewportBox(frame, { x: 20, y: 20, width: 20, height: 20 }, { width: 100, height: 100 }))
      .toEqual({ page: 1, frame_sha256: frame.sha256, x0: 2000, y0: 2000, x1: 4000, y1: 4000 });
  });

  it("rejects selections outside or smaller than one physical unit", () => {
    expect(() => viewportBox(frame, { x: 90, y: 20, width: 20, height: 20 }, { width: 100, height: 100 })).toThrow();
    expect(() => viewportBox(frame, { x: -1, y: 20, width: 20, height: 20 }, { width: 100, height: 100 })).toThrow();
    expect(() => viewportBox(frame, { x: 0, y: 0, width: 0.001, height: 20 }, { width: 100, height: 100 })).toThrow();
    expect(() => viewportBox({ ...frame, sha256: "wrong" }, { x: 20, y: 20, width: 20, height: 20 }, { width: 100, height: 100 })).toThrow();
  });
});
