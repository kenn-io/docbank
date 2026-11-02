import type { PageFrameV1, PDFRedactionBox } from "./generated/docbank.js";
export interface ReaderPage {
  index: number;
  rotation: number;
  size: { width: number; height: number };
  boxes?: {
    media: { left: number; bottom: number; right: number; top: number };
    crop: { left: number; bottom: number; right: number; top: number };
  };
}
export interface ReaderRect {
  origin: { x: number; y: number };
  size: { width: number; height: number };
}
const quantize = (value: number) => Math.sign(value) * Math.round(Math.abs(value * 10000));
export function bindReaderPages(
  doc: { normalizedRotation: boolean; pages: ReaderPage[] },
  frames: PageFrameV1[],
) {
  if (!doc.normalizedRotation) throw new Error("The reader's page rotation is not normalized.");
  if (doc.pages.length !== frames.length)
    throw new Error("The reader's page count differs from preparation.");
  frames.forEach((frame, index) => {
    const page = doc.pages[index]!;
    const boxes = [page.boxes?.media, page.boxes?.crop];
    const expected = [frame.media_box, frame.crop_box];
    const sameBoxes = boxes.every(
      (box, i) =>
        box &&
        [box.left, box.bottom, box.right, box.top].every(
          (v, j) =>
            Number.isFinite(v) &&
            Number.isSafeInteger(quantize(v)) &&
            quantize(v) === expected[i]![j],
        ),
    );
    const rotated = frame.rotation === 90 || frame.rotation === 270;
    if (
      page.index !== index ||
      frame.page !== index + 1 ||
      page.rotation * 90 !== frame.rotation ||
      !sameBoxes ||
      !Number.isFinite(page.size.width) ||
      !Number.isFinite(page.size.height) ||
      page.size.width <= 0 ||
      page.size.height <= 0 ||
      Math.round((page.size.width * 10000) / 72) !== (rotated ? frame.height : frame.width) ||
      Math.round((page.size.height * 10000) / 72) !== (rotated ? frame.width : frame.height)
    )
      throw new Error(
        `Page ${index + 1} geometry differs from preparation. Drawing is unavailable.`,
      );
  });
}
function corners(x0: number, y0: number, x1: number, y1: number) {
  return [
    [x0, y0],
    [x1, y0],
    [x0, y1],
    [x1, y1],
  ] as [number, number][];
}
export function readerBox(rect: ReaderRect, page: ReaderPage, frame: PageFrameV1) {
  const crop = page.boxes!.crop,
    {
      values: [a, b, c, d, e, f],
      denominator,
    } = coefficients(frame);
  const points = corners(
    rect.origin.x,
    rect.origin.y,
    rect.origin.x + rect.size.width,
    rect.origin.y + rect.size.height,
  ).map(([x, y]) => {
    const px = (crop.left + x) * 10000,
      py = (crop.top - y) * 10000;
    return [(a * px + c * py + e) / denominator, (b * px + d * py + f) / denominator];
  });
  if (points.flat().some((v) => !Number.isFinite(v))) throw new Error("Invalid reader rectangle.");
  return {
    x0: Math.max(0, Math.floor(Math.min(...points.map((p) => p[0]!)))),
    y0: Math.max(0, Math.floor(Math.min(...points.map((p) => p[1]!)))),
    x1: Math.min(frame.width, Math.ceil(Math.max(...points.map((p) => p[0]!)))),
    y1: Math.min(frame.height, Math.ceil(Math.max(...points.map((p) => p[1]!)))),
  };
}
export function overlayBox(
  box: Pick<PDFRedactionBox, "x0" | "y0" | "x1" | "y1">,
  page: ReaderPage,
  frame: PageFrameV1,
) {
  const crop = page.boxes!.crop,
    {
      values: [a, b, c, d, e, f],
      denominator,
    } = coefficients(frame);
  const determinant = a * d - b * c;
  const points = corners(box.x0, box.y0, box.x1, box.y1).map(([x, y]) => {
    const px = (d * (x * denominator - e) - c * (y * denominator - f)) / determinant,
      py = (-b * (x * denominator - e) + a * (y * denominator - f)) / determinant;
    return [px / 10000 - crop.left, crop.top - py / 10000];
  });
  const x = Math.min(...points.map((p) => p[0]!)),
    y = Math.min(...points.map((p) => p[1]!));
  return {
    x,
    y,
    width: Math.max(...points.map((p) => p[0]!)) - x,
    height: Math.max(...points.map((p) => p[1]!)) - y,
  };
}

function coefficients(frame: PageFrameV1) {
  const gcd = (a: number, b: number): number => (b ? gcd(b, a % b) : a);
  const denominator = frame.transform.reduce(
    (d, r) => (d / gcd(d, r.denominator)) * r.denominator,
    1,
  );
  return {
    denominator,
    values: frame.transform.map((r) => r.numerator * (denominator / r.denominator)) as [
      number,
      number,
      number,
      number,
      number,
      number,
    ],
  };
}
