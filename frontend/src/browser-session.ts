export interface BrowserSession {
  token: string;
  uploadSecret: string;
}

export function takeFragmentSession(
  location: Location = window.location,
  history: History = window.history,
): BrowserSession | null {
  const params = new URLSearchParams(location.hash.replace(/^#/, ""));
  const token = params.get("web_session") ?? "";
  const uploadSecret = params.get("web_upload_secret") ?? "";
  if (token || uploadSecret) {
    history.replaceState(null, "", `${location.pathname}${location.search}`);
  }
  return token && uploadSecret ? { token, uploadSecret } : null;
}
