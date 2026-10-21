import { afterEach, expect, it } from "vite-plus/test";
import { queryFromFragment, replaceQueryURL } from "./queryURL.js";
import { parseQuery } from "./query.js";

afterEach(() => history.replaceState(null, "", "/"));
const query = parseQuery(
  '{"text":"α OR beta","syntax":"advanced","mode":"hybrid","filters":{"extensions":["pdf"],"no_tags":true},"sort":{"field":"size","direction":"desc"}}',
);

it("round-trips the full query and preserves other fragment keys and history state", () => {
  history.replaceState(
    { fixtureIndex: 3 },
    "",
    `/reports?view=list#node=42&version=example&photo=photo-1&source=example&query=${encodeURIComponent(JSON.stringify(query))}`,
  );
  const restored = queryFromFragment(location.hash);
  expect(restored).toEqual(query);
  replaceQueryURL(restored);
  expect(queryFromFragment(location.hash)).toEqual(query);
  expect(history.state).toEqual({ fixtureIndex: 3 });
  expect(location.pathname).toBe("/reports");
  expect(location.search).toBe("?view=list");
  const params = new URLSearchParams(location.hash.slice(1));
  expect(params.get("node")).toBe("42");
  expect(params.get("version")).toBe("example");
  expect(params.get("photo")).toBe("photo-1");
  expect(params.get("source")).toBe("example");
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

it("removes only the query when other fragment keys are present", () => {
  history.replaceState(
    null,
    "",
    "/#node=42&version=example&photo=photo-1&source=example&query=%7B%7D",
  );
  replaceQueryURL(null);
  expect(location.hash).toBe("#node=42&version=example&photo=photo-1&source=example");
});
