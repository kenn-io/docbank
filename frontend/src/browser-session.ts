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
  if (params.has("web_session") || params.has("web_upload_secret")) {
    params.delete("web_session");
    params.delete("web_upload_secret");
    const fragment = params.toString();
    history.replaceState(
      history.state,
      "",
      `${location.pathname}${location.search}${fragment ? `#${fragment}` : ""}`,
    );
  }
  return token && uploadSecret ? { token, uploadSecret } : null;
}
