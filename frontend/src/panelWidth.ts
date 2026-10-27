import { localPreferenceStorage } from "./browser-storage.js";

const prefix = "docbank.panelWidth.";

export function watchWidth(element: HTMLElement, onwidth: (width: number) => void) {
  if (typeof ResizeObserver === "undefined") return;
  const observer = new ResizeObserver(([entry]) => {
    if (entry) onwidth(entry.contentRect.width);
  });
  observer.observe(element);
  return () => observer.disconnect();
}

// A saved width is a number of pixels; anything else means "use the default".
export function readPanelWidth(name: string): number | undefined {
  try {
    const value = Number(localPreferenceStorage()?.getItem(prefix + name));
    return Number.isFinite(value) && value > 0 ? value : undefined;
  } catch {
    return undefined;
  }
}

export function savePanelWidth(name: string, width: number | undefined): void {
  try {
    const storage = localPreferenceStorage();
    if (width === undefined) storage?.removeItem(prefix + name);
    else storage?.setItem(prefix + name, String(Math.round(width)));
  } catch {
    /* Browser storage is optional. */
  }
}

export function clampPanelWidth(width: number, min: number, max: number): number {
  return Math.round(Math.min(Math.max(width, min), Math.max(min, max)));
}
