// jsdom has no media-query evaluator. Individual responsive tests supply matches.
if (typeof window !== "undefined") {
  window.matchMedia = (media: string) =>
    Object.assign(new EventTarget(), {
      media,
      matches: false,
      onchange: null,
      addListener() {},
      removeListener() {},
    }) as MediaQueryList;
}

// jsdom has no Web Animations API. Svelte transitions finish immediately in tests.
if (typeof Element !== "undefined" && !Element.prototype.animate) {
  Element.prototype.animate = function () {
    const animation = {
      currentTime: 0,
      playState: "finished",
      onfinish: null as (() => void) | null,
      cancel() {},
    };
    queueMicrotask(() => animation.onfinish?.());
    return animation as unknown as Animation;
  };
}
