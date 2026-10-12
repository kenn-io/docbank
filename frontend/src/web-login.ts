import { sessionJSON } from "./api-transport.js";
import type { BrowserSession } from "./browser-session.js";

export interface SignInStatus {
  enabled: boolean;
  session?: { token: string; upload_secret: string };
}

export function getSignInStatus(): Promise<SignInStatus> {
  return sessionJSON<SignInStatus>("/api/daemon/web-auth", {});
}

export function signIn(apiKey: string): Promise<SignInStatus> {
  return sessionJSON<SignInStatus>("/api/daemon/web-auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ api_key: apiKey }),
  });
}

export function tabSession(status: SignInStatus): BrowserSession | null {
  if (!status.session) return null;
  return { token: status.session.token, uploadSecret: status.session.upload_secret };
}
