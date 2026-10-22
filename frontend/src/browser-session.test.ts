import { afterEach, expect, it } from "vite-plus/test";
import { takeFragmentSession } from "./browser-session.js";

afterEach(() => history.replaceState(null, "", "/"));

it.each([
  {
    url: "/photos?view=grid#web_session=one%20time&node=42&version=11111111-1111-4111-8111-111111111111&query=%7B%7D&photo=image&web_upload_secret=proof",
    remaining:
      "/photos?view=grid#node=42&version=11111111-1111-4111-8111-111111111111&query=%7B%7D&photo=image",
  },
  {
    url: "/#web_session=one%20time&web_upload_secret=proof",
    remaining: "/",
  },
])(
  "removes only credential fields while preserving browser intent and history state: $url",
  ({ url, remaining }) => {
    history.replaceState({ position: 7 }, "", url);
    expect(takeFragmentSession()).toEqual({ token: "one time", uploadSecret: "proof" });
    expect(location.pathname + location.search + location.hash).toBe(remaining);
    expect(sessionStorage.length).toBe(0);
    expect(history.state).toEqual({ position: 7 });
    expect(takeFragmentSession()).toBeNull();
  },
);

it("scrubs incomplete credentials too", () => {
  history.replaceState(null, "", "/#node=42&web_session=");
  expect(takeFragmentSession()).toBeNull();
  expect(location.hash).toBe("#node=42");
});

it("leaves unrelated fragments untouched", () => {
  history.replaceState(null, "", "/#photo=image%20one&query=%7B%7D");
  expect(takeFragmentSession()).toBeNull();
  expect(location.hash).toBe("#photo=image%20one&query=%7B%7D");
});
