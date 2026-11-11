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
