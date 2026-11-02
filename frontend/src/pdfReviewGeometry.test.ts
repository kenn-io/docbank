import { describe, expect, it } from "vite-plus/test";
import { bindReaderPages, readerBox, overlayBox } from "./pdfReviewGeometry.js";
import type { PageFrameV1 } from "./generated/docbank.js";

const transforms = [
  [1, 0, 0, -1, -240000, 1080000],
  [0, 1, 1, 0, -360000, -240000],
  [-1, 0, 0, 1, 1680000, -360000],
  [0, -1, -1, 0, 1080000, 1680000],
];
const frames = transforms.map(
  (values, i) =>
    ({
      page: i + 1,
      rotation: i * 90,
      width: i % 2 ? 10000 : 20000,
      height: i % 2 ? 20000 : 10000,
      crop_box: [240000, 360000, 1680000, 1080000],
      media_box: [0, 0, 2160000, 1440000],
      transform: values.map((numerator) => ({ numerator, denominator: 72 })),
    }) as PageFrameV1,
);
const pages = frames.map((_, index) => ({
  index,
  rotation: index,
  size: { width: 144, height: 72 },
  boxes: {
    media: { left: 0, bottom: 0, right: 216, top: 144 },
    crop: { left: 24, bottom: 36, right: 168, top: 108 },
  },
}));
describe("reader coordinate authority", () => {
  it("maps crop-relative marks and overlays through all intrinsic rotations", () => {
    bindReaderPages({ normalizedRotation: true, pages }, frames);
    const expected = [
      [2500, 2500, 10000, 7500],
      [2500, 2500, 7500, 10000],
      [10000, 2500, 17500, 7500],
      [2500, 10000, 7500, 17500],
    ];
    for (let i = 0; i < 4; i++) {
      const b = readerBox(
        { origin: { x: 18, y: 18 }, size: { width: 54, height: 36 } },
        pages[i]!,
        frames[i]!,
      );
      expect([b.x0, b.y0, b.x1, b.y1]).toEqual(expected[i]);
      expect(overlayBox(b, pages[i]!, frames[i]!)).toEqual({ x: 18, y: 18, width: 54, height: 36 });
    }
  });
  it("refuses an otherwise usable page with a different crop", () => {
    const changed = structuredClone(pages);
    changed[0]!.boxes.crop.left = 25;
    expect(() => bindReaderPages({ normalizedRotation: true, pages: changed }, frames)).toThrow(
      /geometry/,
    );
    expect(() => bindReaderPages({ normalizedRotation: false, pages }, frames)).toThrow(/rotation/);
  });
  it("compares fractional boxes at native precision", () => {
    const page = {
      index: 0,
      rotation: 0,
      size: { width: 1152, height: 792 },
      boxes: {
        media: { left: 0, bottom: 0, right: 1296, top: 1008 },
        crop: {
          left: Math.fround(38.5511),
          bottom: Math.fround(25.1234),
          right: Math.fround(1190.5511),
          top: Math.fround(817.1234),
        },
      },
    };
    const frame = {
      ...frames[0]!,
      width: 160000,
      height: 110000,
      media_box: [0, 0, 12960000, 10080000],
      crop_box: [385511, 251234, 11905511, 8171234],
    };
    expect(() =>
      bindReaderPages({ normalizedRotation: true, pages: [page] }, [frame]),
    ).not.toThrow();
    expect(() =>
      bindReaderPages({ normalizedRotation: true, pages: [page] }, [
        { ...frame, crop_box: [385512, 251234, 11905512, 8171234] },
      ]),
    ).toThrow(/geometry/);
  });
});
