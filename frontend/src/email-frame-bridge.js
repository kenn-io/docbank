// Opaque-frame gesture bridge, adapted from Msgvault (see email-viewer.LICENSE).
// This is the only script the email frame's CSP permits.
(() => {
  const { emailNonce: nonce, emailOrigin: origin } = document.documentElement.dataset;
  if (!nonce || !origin) return;
  const send = (action) => parent.postMessage({ channel: "docbank-email", nonce, action }, origin);
  document.addEventListener("pointerdown", () => send("inspect"));
  document.addEventListener("wheel", () => send("inspect"), { passive: true });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      event.preventDefault();
      send("escape");
    } else if (
      !event.altKey &&
      !event.ctrlKey &&
      !event.metaKey &&
      [
        "ArrowUp",
        "ArrowDown",
        "ArrowLeft",
        "ArrowRight",
        "PageUp",
        "PageDown",
        "Home",
        "End",
        " ",
        "j",
        "k",
      ].includes(event.key)
    )
      send("inspect");
  });
})();
