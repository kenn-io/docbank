import { afterEach, expect, it, vi } from "vite-plus/test";
import * as panelWidth from "./panelWidth.js";

afterEach(() => vi.unstubAllGlobals());

it("observes each results pane independently and disconnects on unmount", () => {
  const observers: {
    callback: ResizeObserverCallback;
    observe: ReturnType<typeof vi.fn>;
    disconnect: ReturnType<typeof vi.fn>;
  }[] = [];
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe = vi.fn();
      disconnect = vi.fn();
      constructor(callback: ResizeObserverCallback) {
        observers.push({ callback, observe: this.observe, disconnect: this.disconnect });
      }
    },
  );
  const browser = document.createElement("div");
  const advanced = document.createElement("div");
  const browserWidths: number[] = [];
  const advancedWidths: number[] = [];
  const stopBrowser = panelWidth.watchWidth(browser, (width) => browserWidths.push(width));
  const stopAdvanced = panelWidth.watchWidth(advanced, (width) => advancedWidths.push(width));
  observers[0]!.callback(
    [{ contentRect: { width: 800 } } as ResizeObserverEntry],
    {} as ResizeObserver,
  );
  observers[1]!.callback(
    [{ contentRect: { width: 500 } } as ResizeObserverEntry],
    {} as ResizeObserver,
  );
  expect(browserWidths).toEqual([800]);
  expect(advancedWidths).toEqual([500]);
  expect(observers[0]!.observe).toHaveBeenCalledWith(browser);
  expect(observers[1]!.observe).toHaveBeenCalledWith(advanced);
  stopBrowser?.();
  stopAdvanced?.();
  expect(observers[0]!.disconnect).toHaveBeenCalledOnce();
  expect(observers[1]!.disconnect).toHaveBeenCalledOnce();
});
