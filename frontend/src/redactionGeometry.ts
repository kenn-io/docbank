export interface Point {
  x: number;
  y: number;
}
export interface ContentRect {
  left: number;
  top: number;
  width: number;
  height: number;
}
export function physicalRectangle(
  start: Point,
  end: Point,
  rect: ContentRect,
  width: number,
  height: number,
) {
  if (
    ![
      start.x,
      start.y,
      end.x,
      end.y,
      rect.left,
      rect.top,
      rect.width,
      rect.height,
      width,
      height,
    ].every(Number.isFinite) ||
    rect.width <= 0 ||
    rect.height <= 0
  )
    return;
  const point = (p: Point) => ({
    x: Math.max(0, Math.min(width, ((p.x - rect.left) * width) / rect.width)),
    y: Math.max(0, Math.min(height, ((p.y - rect.top) * height) / rect.height)),
  });
  const a = point(start),
    b = point(end);
  if (a.x === b.x || a.y === b.y) return;
  return {
    x0: Math.floor(Math.min(a.x, b.x)),
    y0: Math.floor(Math.min(a.y, b.y)),
    x1: Math.ceil(Math.max(a.x, b.x)),
    y1: Math.ceil(Math.max(a.y, b.y)),
  };
}
