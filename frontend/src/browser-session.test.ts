import { afterEach, expect, it } from "vite-plus/test";
import { takeFragmentSession } from "./browser-session.js";

afterEach(() => history.replaceState(null, "", "/"));

it("removes only credential fields while preserving browser intent and history state", () => {
  history.replaceState(
    { position: 7 },
    "",
    "/photos?view=grid#web_session=token&node=42&version=11111111-1111-4111-8111-111111111111&query=%7B%7D&photo=image&web_upload_secret=proof",
  );
  expect(takeFragmentSession()).toEqual({ token: "token", uploadSecret: "proof" });
  expect(location.pathname + location.search + location.hash).toBe(
    "/photos?view=grid#node=42&version=11111111-1111-4111-8111-111111111111&query=%7B%7D&photo=image",
  );
  expect(history.state).toEqual({ position: 7 });
});

it("scrubs incomplete credentials too", () => {
  history.replaceState(null, "", "/#node=42&web_session=token&web_session=other");
  expect(takeFragmentSession()).toBeNull();
  expect(location.hash).toBe("#node=42");
});

it("leaves unrelated fragments untouched", () => {
  history.replaceState(null, "", "/#photo=image%20one&query=%7B%7D");
  expect(takeFragmentSession()).toBeNull();
  expect(location.hash).toBe("#photo=image%20one&query=%7B%7D");
});
