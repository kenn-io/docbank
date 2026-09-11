import { afterEach, expect, it } from "vitest";
import { takeFragmentSession } from "./api.js";
import { queryFromFragment, replaceQueryURL } from "./queryURL.js";
import { parseQuery } from "./query.js";

afterEach(() => history.replaceState(null, "", "/"));
const query = parseQuery('{"text":"α OR beta","syntax":"advanced","mode":"hybrid","filters":{"extensions":["pdf"],"no_tags":true},"sort":{"field":"size","direction":"desc"}}');

it("round-trips the full query without serializing session credentials", () => {
  history.replaceState(null, "", "/#web_session=secret&web_upload_secret=proof");
  replaceQueryURL(query);
  expect(queryFromFragment(location.hash)).toEqual(query);
  expect(location.hash).not.toContain("secret");
  expect(location.hash).not.toContain("proof");
  expect(sessionStorage.length).toBe(0);
});

it("allows query restoration before consuming a combined sign-in fragment", () => {
  history.replaceState(null, "", `/#web_session=secret&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`);
  const restored = queryFromFragment(location.hash);
  expect(takeFragmentSession()).toEqual({ token: "secret", uploadSecret: "proof" });
  expect(location.hash).toBe("");
  replaceQueryURL(restored);
  expect(queryFromFragment(location.hash)).toEqual(query);
  expect(location.hash).not.toContain("web_session");
});

it("rejects ambiguous, oversized and unknown query state", () => {
  expect(() => queryFromFragment("#query={}&query={}")).toThrow();
  expect(() => queryFromFragment(`#query=${"x".repeat(400000)}`)).toThrow();
  expect(() => queryFromFragment("#query=%7B%22future%22%3Atrue%7D")).toThrow();
  expect(queryFromFragment("#unrelated=value")).toBeNull();
});

it("removes the draft URL explicitly", () => {
  replaceQueryURL(query);
  replaceQueryURL(null);
  expect(location.hash).toBe("");
});
