/** Exact page-frame coordinates used by the production draft API. */
export type Frame = { page: number; sha256: string; width: number; height: number };
export type PhysicalBox = {
  page: number; frame_sha256: string; x0: number; y0: number; x1: number; y1: number;
};

type Rect = { x: number; y: number; width: number; height: number };
type Size = { width: number; height: number };

/**
 * Scale an already unrotated, frame-aligned viewport rectangle into the
 * physical page frame. The caller must apply the PDF viewer's crop and
 * rotation transform before calling this function.
 */
export function viewportBox(frame: Frame, box: Rect, viewport: Size): PhysicalBox {
  if (!Number.isSafeInteger(frame.page) || frame.page < 1 ||
      !/^[0-9a-f]{64}$/.test(frame.sha256) ||
      !Number.isSafeInteger(frame.width) || frame.width < 1 ||
      !Number.isSafeInteger(frame.height) || frame.height < 1 ||
      !Number.isFinite(viewport.width) || viewport.width <= 0 ||
      !Number.isFinite(viewport.height) || viewport.height <= 0 ||
      ![box.x, box.y, box.width, box.height].every(Number.isFinite) ||
      box.x < 0 || box.y < 0 || box.width <= 0 || box.height <= 0 ||
      box.x + box.width > viewport.width || box.y + box.height > viewport.height)
    throw new Error("Selection is outside the verified page frame");

  const x0 = Math.round(box.x / viewport.width * frame.width);
  const y0 = Math.round(box.y / viewport.height * frame.height);
  const x1 = Math.round((box.x + box.width) / viewport.width * frame.width);
  const y1 = Math.round((box.y + box.height) / viewport.height * frame.height);
  if (![x0, y0, x1, y1].every(Number.isSafeInteger) || x0 < 0 || y0 < 0 ||
      x1 > frame.width || y1 > frame.height || x0 >= x1 || y0 >= y1)
    throw new Error("Selection cannot be represented in physical page units");
  return { page: frame.page, frame_sha256: frame.sha256, x0, y0, x1, y1 };
}

/** Convert a browser UTF-16 offset to the UTF-8 byte offset used by the map. */
export function utf16ToUTF8Offset(text: string, offset: number): number {
  if (!Number.isSafeInteger(offset) || offset < 0 || offset > text.length)
    throw new Error("Invalid browser text offset");
  let bytes = 0;
  for (let i = 0; i < text.length; i++) {
    const unit = text.charCodeAt(i);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = text.charCodeAt(++i);
      if (!(next >= 0xdc00 && next <= 0xdfff) || offset === i)
        throw new Error("Selection splits or contains an invalid surrogate pair");
      bytes += i < offset ? 4 : 0;
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      throw new Error("Selection contains an invalid surrogate pair");
    } else if (i < offset) {
      bytes += unit < 0x80 ? 1 : unit < 0x800 ? 2 : 3;
    }
  }
  return bytes;
}
