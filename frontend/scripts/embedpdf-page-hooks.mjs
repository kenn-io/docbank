// Temporary upstream extension for the pinned bundled viewer. Keep this source
// readable: the installer binds it to the bundle's own Preact and plugin hooks.
// Remove it when the viewer exposes decoded-image status and a page-layer slot.
export function createPageHooks({
  h,
  Fragment,
  useLayoutEffect,
  useRef,
  useState,
  useRenderCapability,
  useDocumentState,
  useTilingCapability,
  cancelError,
}) {
  function useImage(documentId, pageIndex, kind, render, dependencies) {
    const anchor = useRef(null);
    const [image, setImage] = useState(null);
    const [attempt, retry] = useState(0);
    useLayoutEffect(() => {
      if (!render) return;
      const host = anchor.current.getRootNode().host;
      const token = {};
      let disposed = false,
        url,
        task;
      const publish = (status) =>
        host.dispatchEvent(
          new CustomEvent("embedpdf:page-render", {
            detail: {
              documentId,
              pageIndex,
              kind,
              token,
              status,
              retry: () => retry((n) => n + 1),
            },
          }),
        );
      setImage(null);
      publish("pending");
      const error = () => {
        if (!disposed) publish("error");
      };
      try {
        task = render();
        task.wait((blob) => {
          if (disposed) return;
          url = URL.createObjectURL(blob);
          setImage({
            url,
            error,
            loaded: (event) => {
              const element = event.currentTarget;
              // Engine success is not proof that the displayed image decoded.
              void element.decode().then(() => {
                if (!disposed)
                  publish(element.naturalWidth && element.naturalHeight ? "ready" : "error");
              }, error);
            },
          });
        }, error);
      } catch {
        error();
      }
      return () => {
        disposed = true;
        task?.abort(cancelError());
        if (url) URL.revokeObjectURL(url);
        publish("disposed");
      };
    }, [...dependencies, attempt]);
    return { anchor, image };
  }

  function RenderLayer({
    documentId,
    pageIndex,
    scale,
    dpr = window.devicePixelRatio,
    style,
    ...props
  }) {
    const { provides } = useRenderCapability();
    const state = useDocumentState(documentId);
    const actualScale = scale ?? state?.scale ?? 1;
    const version = state?.pageRefreshVersions[pageIndex] ?? 0;
    const { anchor, image } = useImage(
      documentId,
      pageIndex,
      "page",
      provides &&
        (() =>
          provides
            .forDocument(documentId)
            .renderPage({ pageIndex, options: { scaleFactor: actualScale, dpr } })),
      [documentId, pageIndex, actualScale, dpr, provides, version],
    );
    return h(
      Fragment,
      null,
      h("span", { ref: anchor, hidden: true }),
      image &&
        h("img", {
          ...props,
          src: image.url,
          onLoad: image.loaded,
          onError: image.error,
          style: { width: "100%", height: "100%", ...style },
        }),
    );
  }

  function TileImg({ documentId, pageIndex, tile, dpr, scale }) {
    const { provides } = useTilingCapability();
    const ratio = scale / tile.srcScale;
    const { anchor, image } = useImage(
      documentId,
      pageIndex,
      "tile",
      provides && (() => provides.forDocument(documentId).renderTile({ pageIndex, tile, dpr })),
      [documentId, pageIndex, tile.id, dpr, provides],
    );
    return h(
      Fragment,
      null,
      h("span", { ref: anchor, hidden: true }),
      image &&
        h("img", {
          src: image.url,
          onLoad: image.loaded,
          onError: image.error,
          style: {
            position: "absolute",
            left: tile.screenRect.origin.x * ratio,
            top: tile.screenRect.origin.y * ratio,
            width: tile.screenRect.size.width * ratio,
            height: tile.screenRect.size.height * ratio,
            display: "block",
          },
        }),
    );
  }

  function PageLayer({ documentId, pageIndex }) {
    const element = useRef(null);
    useLayoutEffect(() => {
      const target = element.current,
        host = target.getRootNode().host;
      const publish = (status) =>
        host.dispatchEvent(
          new CustomEvent("embedpdf:page-layer", {
            detail: { documentId, pageIndex, target, status },
          }),
        );
      publish("mounted");
      return () => publish("disposed");
    }, [documentId, pageIndex]);
    // This empty slot belongs to the consumer; the viewer never reconciles its children.
    return h("div", {
      ref: element,
      style: { position: "absolute", inset: 0, zIndex: 2, pointerEvents: "none" },
    });
  }
  return { RenderLayer, TileImg, PageLayer };
}
