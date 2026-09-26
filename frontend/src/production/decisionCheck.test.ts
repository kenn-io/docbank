import { afterEach, expect, it, vi } from "vitest";
import type { ProductionDecision } from "./api.js";
import { assessSelectionDecision } from "./decisionCheck.js";

const setID = "11111111-1111-4111-8111-111111111111";
const memberID = "22222222-2222-4222-8222-222222222222";
const decisionID = "33333333-3333-4333-8333-333333333333";
const mapSHA = "a".repeat(64);
const frameSHA = "b".repeat(64);
const frame = { page: 1, sha256: frameSHA, width: 10000, height: 10000 };
const decision: ProductionDecision = { id: decisionID, member_id: memberID, action: "redact",
  uncertain: false, reason: "Synthetic private reason", label: "Synthetic label", selector: {
    kind: "rectangle", map_sha256: mapSHA, boxes: [{ page: 1, frame_sha256: frameSHA,
      x0: 1000, y0: 2000, x1: 5000, y1: 6000 }],
  } };
const draft = { set_id: setID, revision: 2, etag: 4, state: "draft", membership_sealed: false };
const base = { set_id: setID, revision: 2, etag: 4, member_id: memberID, decision_id: decisionID };
const expanded = { kind: "rectangle", map_sha256: mapSHA,
  boxes: [{ page: 1, frame_sha256: frameSHA, x0: 0, y0: 0, x1: 10000, y1: 10000 }] };

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function respond(check: object, currentDraft: object = draft): void {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith("/decisions/check")) {
      expect(init?.method).toBe("POST");
      expect(new Headers(init?.headers).get("If-Match")).toBe("4");
      expect(JSON.parse(String(init?.body))).toEqual({ decision });
      return Response.json(check);
    }
    if (url.endsWith("/revisions/2")) return Response.json(currentDraft);
    throw new Error(`unexpected request ${url}`);
  });
}

it("checks a proposal without changing it and requires the current draft", async () => {
  respond({ ...base, outcome: "ready" });
  expect(await assessSelectionDecision("synthetic", setID, 2, 4, decision, frame,
    new AbortController().signal)).toEqual({ kind: "ready" });
});

it("returns only a same-page, frame-bound expansion for explicit acceptance", async () => {
  respond({ ...base, outcome: "selection_expansion_required", expanded_box_count: 1, expanded });
  expect(await assessSelectionDecision("synthetic", setID, 2, 4, decision, frame,
    new AbortController().signal)).toEqual({ kind: "expansion", selector: expanded });
});

it("rejects mismatched or out-of-frame expanded geometry", async () => {
  for (const bad of [
    { ...expanded, map_sha256: "f".repeat(64) },
    { ...expanded, boxes: [{ ...expanded.boxes[0], frame_sha256: "f".repeat(64) }] },
    { ...expanded, boxes: [{ ...expanded.boxes[0], page: 2 }] },
    { ...expanded, boxes: [{ ...expanded.boxes[0], x1: 10001 }] },
  ]) {
    respond({ ...base, outcome: "selection_expansion_required", expanded_box_count: 1, expanded: bad });
    await expect(assessSelectionDecision("synthetic", setID, 2, 4, decision, frame,
      new AbortController().signal)).rejects.toThrow(/expanded|frame|map/i);
    vi.restoreAllMocks();
  }
});

it("rejects a changed draft or a response bound to another proposal", async () => {
  respond({ ...base, outcome: "ready" }, { ...draft, etag: 5 });
  await expect(assessSelectionDecision("synthetic", setID, 2, 4, decision, frame,
    new AbortController().signal)).rejects.toThrow(/changed/i);
  vi.restoreAllMocks();
  respond({ ...base, decision_id: "44444444-4444-4444-8444-444444444444", outcome: "ready" });
  await expect(assessSelectionDecision("synthetic", setID, 2, 4, decision, frame,
    new AbortController().signal)).rejects.toThrow(/decision|proposal/i);
});

it("keeps conflicts and oversized expansions from becoming ready decisions", async () => {
  respond({ ...base, outcome: "decision_conflict", decision_ids: [decisionID] });
  expect(await assessSelectionDecision("synthetic", setID, 2, 4, decision, frame,
    new AbortController().signal)).toEqual({ kind: "conflict", decisionIDs: [decisionID] });
  vi.restoreAllMocks();
  respond({ ...base, outcome: "selection_expansion_required", expanded_box_count: 65 });
  expect(await assessSelectionDecision("synthetic", setID, 2, 4, decision, frame,
    new AbortController().signal)).toEqual({ kind: "expansion_unavailable", boxCount: 65 });
});
