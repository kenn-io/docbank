export function rotatedDimensions(width: number, height: number, rotation = 0) {
  return rotation === 90 || rotation === 270 ? { width: height, height: width } : { width, height };
}

export function photoRotationTransform(rotation: number) {
  return `translate(-50%,-50%) rotate(${rotation}deg)`;
}

export function photoRotationStyle(rotation: number, width: number, height: number) {
  const size = rotatedDimensions(width, height, rotation);
  return `width:${size.width}px;height:${size.height}px;transform:${photoRotationTransform(rotation)}`;
}

export function observePhotoSize(
  element: HTMLElement,
  resized: (width: number, height: number) => void,
) {
  const update = () => resized(element.clientWidth, element.clientHeight);
  const observer = new ResizeObserver(update);
  observer.observe(element);
  update();
  return () => observer.disconnect();
}
