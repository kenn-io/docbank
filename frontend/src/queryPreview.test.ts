import { afterEach, describe, expect, it, vi } from "vitest";
import fixture from "../../internal/query/testdata/identity-vectors.json";
import { parseQuery } from "./query.js";
import { previewQuery, querySelection } from "./queryPreview.js";

const defaults = fixture.queries.find((vector) => vector.name === "query_defaults") ?? (() => {
  throw new Error("missing query_defaults identity fixture");
})();

const tagID = "11111111-1111-4111-8111-111111111111";
const savedID = "22222222-2222-4222-8222-222222222222";
const collectionID = "33333333-3333-4333-8333-333333333333";
const query = parseQuery(defaults.input_json);

function response(overrides: Record<string, unknown> = {}): Response {
  return new Response(JSON.stringify({
    $schema: "http://localhost/schemas/QueryPreview.json",
    query: JSON.parse(defaults.canonical_utf8),
    query_fingerprint: defaults.fingerprint,
    dependencies: [
      { kind: "tag", id: tagID, revision: 3 },
      { kind: "saved", id: savedID, revision: 8 },
      { kind: "collection", id: collectionID, revision: 5 },
    ],
    ...overrides,
  }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("query preview transport", () => {
  afterEach(() => vi.restoreAllMocks());

  it("posts the canonical query with JSON content type and the caller's abort signal", async () => {
    const controller = new AbortController();
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(response());

    await expect(previewQuery("session", query, controller.signal)).resolves.toEqual({
      query,
      query_fingerprint: defaults.fingerprint,
      dependencies: [
        { kind: "tag", id: tagID, revision: 3 },
        { kind: "saved", id: savedID, revision: 8 },
        { kind: "collection", id: collectionID, revision: 5 },
      ],
    });

    const [path, request] = fetchMock.mock.calls[0] ?? [];
    expect(path).toBe("/api/v1/queries/parse");
    expect(request?.method).toBe("POST");
    expect(request?.body).toBe(defaults.canonical_utf8);
    expect(new Headers(request?.headers).get("Content-Type")).toBe("application/json");
    expect(request?.signal).toBe(controller.signal);
  });

  it.each([
    ["a different canonical query", { query: { ...JSON.parse(defaults.canonical_utf8), text: "changed" } }],
    ["a different fingerprint", { query_fingerprint: `sha256:${"0".repeat(64)}` }],
  ])("rejects a receipt with %s", async (_name, overrides) => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(response(overrides));
    await expect(previewQuery("session", query, new AbortController().signal)).rejects.toThrow(
      /query preview receipt/i,
    );
  });

  it.each([
    ["a non-object envelope", null],
    ["a missing field", { query: JSON.parse(defaults.canonical_utf8), query_fingerprint: defaults.fingerprint }],
    ["an unknown envelope field", {
      query: JSON.parse(defaults.canonical_utf8), query_fingerprint: defaults.fingerprint,
      dependencies: [], future: true,
    }],
    ["an invalid schema link", {
      $schema: 7, query: JSON.parse(defaults.canonical_utf8),
      query_fingerprint: defaults.fingerprint, dependencies: [],
    }],
    ["an incomplete query", { query: {}, query_fingerprint: defaults.fingerprint, dependencies: [] }],
    ["an invalid QueryV1 value", {
      query: { ...JSON.parse(defaults.canonical_utf8), syntax: "future" },
      query_fingerprint: defaults.fingerprint, dependencies: [],
    }],
    ["an unknown dependency field", {
      query: JSON.parse(defaults.canonical_utf8), query_fingerprint: defaults.fingerprint,
      dependencies: [{ kind: "tag", id: tagID, revision: 1, name: "Synthetic" }],
    }],
    ["an invalid dependency ID", {
      query: JSON.parse(defaults.canonical_utf8), query_fingerprint: defaults.fingerprint,
      dependencies: [{ kind: "tag", id: "not-an-id", revision: 1 }],
    }],
  ])("rejects %s", async (_name, receipt) => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(receipt), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    await expect(previewQuery("session", query, new AbortController().signal)).rejects.toThrow(
      /query preview receipt/i,
    );
  });

  it.each([
    ["unknown dependency kind", [{ kind: "folder", id: tagID, revision: 1 }]],
    ["coerced dependency kind", [{ kind: ["tag"], id: tagID, revision: 1 }]],
    ["duplicate dependency identity", [
      { kind: "tag", id: tagID, revision: 1 },
      { kind: "tag", id: tagID, revision: 2 },
    ]],
    ["unsafe dependency revision", [{ kind: "tag", id: tagID, revision: 9_007_199_254_740_992 }]],
    ["fractional dependency revision", [{ kind: "tag", id: tagID, revision: 1.5 }]],
    ["non-positive dependency revision", [{ kind: "tag", id: tagID, revision: 0 }]],
    [">256 dependencies", Array.from({ length: 257 }, (_, index) => ({
      kind: "tag",
      id: `00000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
      revision: 1,
    }))],
  ])("rejects %s", async (_name, dependencies) => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(response({ dependencies }));
    await expect(previewQuery("session", query, new AbortController().signal)).rejects.toThrow(
      /query preview receipt/i,
    );
  });
});

describe("query error selection", () => {
  it("maps exact UTF-8 byte boundaries to UTF-16 editor offsets", () => {
    expect(querySelection("alpha", { offset: 1, end: 4 })).toEqual({ start: 1, end: 4 });
    expect(querySelection("😀x", { offset: 4, end: 5 })).toEqual({ start: 2, end: 3 });
  });

  it("preserves an empty half-open span as an editor caret", () => {
    expect(querySelection("x", { offset: 0, end: 0 })).toEqual({ start: 0, end: 0 });
    expect(querySelection("😀x", { offset: 4, end: 4 })).toEqual({ start: 2, end: 2 });
  });

  it("rejects spans inside a scalar, outside the text, or in reverse", () => {
    expect(querySelection("😀x", { offset: 1, end: 4 })).toBeNull();
    expect(querySelection("x", { offset: 0, end: 2 })).toBeNull();
    expect(querySelection("x", { offset: 1, end: 0 })).toBeNull();
  });

  it.each([null, [], {}, { offset: 0 }, { offset: 0.5, end: 1 }, { offset: -1, end: 1 }])(
    "rejects an untrusted position %#",
    (position) => expect(querySelection("x", position)).toBeNull(),
  );
});
