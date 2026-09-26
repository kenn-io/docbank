import { describe, expect, it } from "vitest";
import { embedpdfMarqueeBox, utf16ToUTF8Offset, viewportBox } from "./embedpdfAdapter.js";

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

  it("maps a cropped page marquee through intrinsic rotation into the retained frame", () => {
    const selection = { origin: { x: 18, y: 36 }, size: { width: 18, height: 36 } };
    const page = { size: { width: 72, height: 144 }, rotation: 1,
      boxes: { crop: { left: 10, top: 164, right: 82, bottom: 20 } } };
    expect(embedpdfMarqueeBox({ ...frame, width: 20000 }, selection, page, { width: 288, height: 144 }))
      .toEqual({ page: 1, frame_sha256: frame.sha256, x0: 10000, y0: 2500, x1: 15000, y1: 5000 });
    expect(embedpdfMarqueeBox({ ...frame, width: 20000 }, selection, page, { width: 576, height: 288 }))
      .toEqual({ page: 1, frame_sha256: frame.sha256, x0: 10000, y0: 2500, x1: 15000, y1: 5000 });
  });

  it("maps all quarter turns to the same physical page orientation", () => {
    const selection = { origin: { x: 18, y: 36 }, size: { width: 18, height: 36 } };
    const size = { width: 72, height: 144 };
    expect(embedpdfMarqueeBox({ ...frame, height: 20000 }, selection, { size, rotation: 0 }, size))
      .toEqual({ page: 1, frame_sha256: frame.sha256, x0: 2500, y0: 5000, x1: 5000, y1: 10000 });
    expect(embedpdfMarqueeBox({ ...frame, height: 20000 }, selection, { size, rotation: 2 }, size))
      .toEqual({ page: 1, frame_sha256: frame.sha256, x0: 5000, y0: 10000, x1: 7500, y1: 15000 });
    expect(embedpdfMarqueeBox({ ...frame, width: 20000 }, selection, { size, rotation: 3 }, { width: 144, height: 72 }))
      .toEqual({ page: 1, frame_sha256: frame.sha256, x0: 5000, y0: 5000, x1: 10000, y1: 7500 });
  });

  it("rejects mismatched frame aspect, crop and display geometry", () => {
    const rect = { origin: { x: 18, y: 36 }, size: { width: 18, height: 36 } };
    const size = { width: 72, height: 144 };
    expect(() => embedpdfMarqueeBox(frame, rect, { size, rotation: 0 }, size)).toThrow();
    expect(() => embedpdfMarqueeBox({ ...frame, height: 20000 }, rect,
      { size, rotation: 0, boxes: { crop: { left: 0, top: 144, right: 70, bottom: 0 } } }, size)).toThrow();
    expect(() => embedpdfMarqueeBox({ ...frame, height: 20000 }, rect,
      { size, rotation: 0 }, { width: 100, height: 100 })).toThrow();
  });

  it("rejects a same-aspect PDF page with the wrong physical frame size", () => {
    const page = { size: { width: 72, height: 72 }, rotation: 0 };
    const selection = { origin: { x: 18, y: 18 }, size: { width: 18, height: 18 } };
    expect(() => embedpdfMarqueeBox({ ...frame, width: 20000, height: 20000 }, selection, page,
      { width: 144, height: 144 })).toThrow(/geometry/i);
  });
});
