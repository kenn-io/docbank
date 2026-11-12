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

let rememberedSession: BrowserSession | null = null;

export const readerReloadContext = Symbol("reader reload");

export function rememberSession(session: BrowserSession | null): void {
  rememberedSession = session;
}

export function reloadWithSession(
  location: Location = window.location,
  history: History = window.history,
): void {
  if (rememberedSession) {
    const params = new URLSearchParams(location.hash.slice(1));
    params.set("web_session", rememberedSession.token);
    params.set("web_upload_secret", rememberedSession.uploadSecret);
    history.replaceState(history.state, "", `${location.pathname}${location.search}#${params}`);
  }
  location.reload();
  takeFragmentSession(location, history);
}
