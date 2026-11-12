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

it("hands the command-line session to the reloaded page and clears the fragment", () => {
  history.replaceState(null, "", "/?view=grid#node=42");
  rememberSession({ token: "session token", uploadSecret: "upload proof" });
  let loaded: { token: string; uploadSecret: string } | null = null;
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
      loaded = takeFragmentSession();
    },
  } as Location;
  reloadWithSession(current, history);
  expect(loaded).toEqual({ token: "session token", uploadSecret: "upload proof" });
  expect(location.pathname + location.search + location.hash).toBe("/?view=grid#node=42");
});

it("does not carry a key login into the next load", () => {
  rememberSession(null);
  history.replaceState(null, "", "/#node=42");
  let fragment = "";
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
      fragment = location.hash;
    },
  } as Location;
  reloadWithSession(current, history);
  expect(fragment).toBe("#node=42");
});
