// Opaque-frame escape bridge, adapted from Msgvault (see email-viewer.LICENSE).
// This is the only script the email frame's CSP permits.
(() => {
  const { emailNonce: nonce, emailOrigin: origin } = document.documentElement.dataset;
  if (!nonce || !origin) return;
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") return;
    event.preventDefault();
    parent.postMessage({ channel: "docbank-email", nonce, action: "escape" }, origin);
  });
})();
