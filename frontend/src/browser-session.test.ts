import { afterEach, expect, it, vi } from "vite-plus/test";
import { rememberSession, reloadWithSession, takeFragmentSession } from "./browser-session.js";

afterEach(() => {
  rememberSession(null);
  vi.restoreAllMocks();
  history.replaceState(null, "", "/");
});

it.each([
  {
    url: "/photos?view=grid#web_session=one%20time&node=42&version=11111111-1111-4111-8111-111111111111&query=%7B%7D&photo=image&web_upload_secret=proof",
    remaining:
      "/photos?view=grid#node=42&version=11111111-1111-4111-8111-111111111111&query=%7B%7D&photo=image",
    session: { token: "one time", uploadSecret: "proof" },
    rewrites: 1,
  },
  {
    url: "/#web_session=one%20time&web_upload_secret=proof",
    remaining: "/",
    session: { token: "one time", uploadSecret: "proof" },
    rewrites: 1,
  },
  {
    url: "/#node=42&web_session=",
    remaining: "/#node=42",
    session: null,
    rewrites: 1,
  },
  {
    url: "/#photo=image%20one&query=%7B%7D",
    remaining: "/#photo=image%20one&query=%7B%7D",
    session: null,
    rewrites: 0,
  },
])(
  "removes only credential fields while preserving browser intent and history state: $url",
  ({ url, remaining, session, rewrites }) => {
    history.replaceState({ position: 7 }, "", url);
    const replace = vi.spyOn(history, "replaceState");
    expect(takeFragmentSession()).toEqual(session);
    expect(replace).toHaveBeenCalledTimes(rewrites);
    expect(location.pathname + location.search + location.hash).toBe(remaining);
    expect(sessionStorage.length).toBe(0);
    expect(history.state).toEqual({ position: 7 });
    expect(takeFragmentSession()).toBeNull();
  },
);

it.each([
  {
    name: "command-line session",
    session: { token: "session token", uploadSecret: "upload proof" },
    fragment: "#node=42&web_session=session+token&web_upload_secret=upload+proof",
  },
  { name: "key login", session: null, fragment: "#node=42" },
])(
  "reloads with the $name and clears any credentials after cancellation",
  ({ session, fragment }) => {
    history.replaceState(null, "", "/?view=grid#node=42");
    rememberSession(session);
    let loadedFragment = "";
    const current = {
      get hash() {
        return location.hash;
      },
      get pathname() {
        return location.pathname;
      },
      get search() {
        return location.search;
      },
      reload() {
        loadedFragment = location.hash;
      },
    } as Location;
    reloadWithSession(current, history);
    expect(loadedFragment).toBe(fragment);
    expect(location.pathname + location.search + location.hash).toBe("/?view=grid#node=42");
  },
);
