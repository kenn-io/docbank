import { describe, expect, it } from "vite-plus/test";
import { bindReaderPages, readerBox, overlayBox } from "./pdfReviewGeometry.js";
import geometry from "../../internal/redactiontest/testdata/page-geometry.json" with { type: "json" };
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
      /differs from the server/,
    );
    expect(() => bindReaderPages({ normalizedRotation: false, pages }, frames)).toThrow(/rotation/);
  });
  it("binds the observed float32 boxes without changing stored frames", () => {
    for (const fixture of geometry) {
      const [left, bottom, right, top] = fixture.observedCrop;
      const page = {
        index: 0,
        rotation: 0,
        size: {
          width: fixture.crop[2]! - fixture.crop[0]!,
          height: fixture.crop[3]! - fixture.crop[1]!,
        },
        boxes: {
          media: {
            left: fixture.observedMedia[0]!,
            bottom: fixture.observedMedia[1]!,
            right: fixture.observedMedia[2]!,
            top: fixture.observedMedia[3]!,
          },
          crop: { left: left!, bottom: bottom!, right: right!, top: top! },
        },
      };
      const frame = {
        ...frames[0]!,
        width: fixture.physical[0]!,
        height: fixture.physical[1]!,
        media_box: fixture.media.map((v) => Math.round(v * 10000)),
        crop_box: fixture.crop.map((v) => Math.round(v * 10000)),
      };
      const before = structuredClone(frame);
      bindReaderPages({ normalizedRotation: true, pages: [page] }, [frame]);
      expect(frame).toEqual(before);
    }
  });
  it("preserves the admitted fractional crop and refuses an unexplained adjacent edge", () => {
    const page = {
      index: 0,
      rotation: 0,
      size: { width: 1152, height: 792 },
      boxes: {
        media: { left: 0, bottom: 0, right: 1296, top: 1008 },
        crop: {
          left: 38.55110168457031,
          bottom: 25.12339973449707,
          right: 1190.5511474609375,
          top: 817.1234130859375,
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
    bindReaderPages({ normalizedRotation: true, pages: [page] }, [frame]);
    frame.crop_box = [385512, 251234, 11905512, 8171234];
    page.boxes.crop.left = 38.55120086669922;
    // This adjacent edge rounds to 11905513, not the frame's projection 11905511.
    page.boxes.crop.right = 1190.55126953125;
    expect(() => bindReaderPages({ normalizedRotation: true, pages: [page] }, [frame])).toThrow(
      /differs from the server/,
    );
  });
  it("refuses an excessive projection even when the small crop fits the raster limits", () => {
    const page = {
      index: 0,
      rotation: 0,
      size: { width: 168, height: 96 },
      boxes: {
        media: { left: 0, bottom: 0, right: 9000, top: 144 },
        crop: { left: 8192, bottom: 24, right: 8360, top: 120 },
      },
    };
    const frame = {
      ...frames[0]!,
      width: 23333,
      height: 13333,
      media_box: [0, 0, 90000000, 1440000],
      crop_box: [81920004, 240000, 83600004, 1200000],
    };
    expect(() => bindReaderPages({ normalizedRotation: true, pages: [page] }, [frame])).toThrow(
      /differs from the server/,
    );
  });
  it("maps marks on the newly admitted crop through every intrinsic rotation", () => {
    const crop = {
      left: 38.55120086669922,
      bottom: 25.12339973449707,
      right: 1190.5511474609375,
      top: 817.1234130859375,
    };
    const values = [
      [1, 0, 0, -1, -385512, 8171234],
      [0, 1, 1, 0, -251234, -385512],
      [-1, 0, 0, 1, 11905512, -251234],
      [0, -1, -1, 0, 8171234, 11905512],
    ];
    const expected = [
      [20000, 19999, 30001, 30000],
      [80000, 20000, 90001, 30001],
      [129999, 80000, 140000, 90001],
      [19999, 129999, 30000, 140000],
    ];
    for (let i = 0; i < 4; i++) {
      const frame = {
        ...frames[i]!,
        width: i % 2 ? 110000 : 160000,
        height: i % 2 ? 160000 : 110000,
        transform: values[i]!.map((numerator) => ({ numerator, denominator: 72 })),
      };
      const box = readerBox(
        { origin: { x: 144, y: 144 }, size: { width: 72, height: 72 } },
        { ...pages[i]!, boxes: { ...pages[i]!.boxes, crop } },
        frame,
      );
      expect([box.x0, box.y0, box.x1, box.y1]).toEqual(expected[i]);
    }
  });
});
