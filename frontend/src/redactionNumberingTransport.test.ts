import { pageDigest } from "./pages.js";
import type { RedactionPreparation, RedactionReview } from "./generated/docbank.js";
import { afterEach, expect, it, vi } from "vite-plus/test";
import {
  decodeNumberedReservation,
  readBatesNamespaceDetail,
  numberedLabels,
} from "./redactionNumbering.js";
const ns = {
  namespace_id: "11111111-1111-4111-8111-111111111111",
  prefix: "CASE",
  suffix: "",
  padding: 2,
  created_at: "2026-01-01T00:00:00Z",
};
afterEach(() => vi.restoreAllMocks());
it("reads an advisory cursor bound to the selected namespace, including exhaustion", async () => {
  const fetch = vi.spyOn(globalThis, "fetch");
  for (const next of [3, 100]) {
    fetch.mockResolvedValueOnce(
      new Response(JSON.stringify({ namespace: ns, next_sequence: next })),
    );
    expect((await readBatesNamespaceDetail("tab", ns.namespace_id)).next_sequence).toBe(next);
  }
  for (const value of [0, 101, 1.5]) {
    fetch.mockResolvedValueOnce(
      new Response(JSON.stringify({ namespace: ns, next_sequence: value })),
    );
    await expect(readBatesNamespaceDetail("tab", ns.namespace_id)).rejects.toThrow();
  }
  fetch.mockResolvedValueOnce(
    new Response(
      JSON.stringify({
        namespace: { ...ns, namespace_id: "22222222-2222-4222-8222-222222222222" },
        next_sequence: 3,
      }),
    ),
  );
  await expect(readBatesNamespaceDetail("tab", ns.namespace_id)).rejects.toThrow();
});
it("shows exact labels for an explicit range and refuses exhaustion without choosing another range", () => {
  expect(numberedLabels(ns, 3, 2)).toEqual(["CASE03", "CASE04"]);
  for (const start of [0, 99, 100, NaN]) expect(numberedLabels(ns, start, 2)).toEqual([]);
});

// Fixed independent review expectations; the response may only supply ledger identities.
const review = {
  selection: {
    node_id: 7,
    source: {
      version_id: "33333333-3333-4333-8333-333333333333",
      sha256: "a".repeat(64),
      size: 100,
    },
  },
  preparation_sha256: "b".repeat(64),
  decisions_sha256: "c".repeat(64),
  sha256: "d".repeat(64),
  recipe: { font_sha256: "e".repeat(64) },
  resolved: {
    sha256: "f".repeat(64),
    page_count: 1,
    pages: [
      {
        number: 1,
        frame_sha256: "0".repeat(64),
        width: 85000,
        height: 110000,
        span: { start: 0, end: 6 },
      },
    ],
  },
} as unknown as RedactionReview;
const frozen = {
  preparation: {} as RedactionPreparation,
  masks: [],
  review,
  namespace: ns,
  start: 3,
};
function response() {
  const vault = "44444444-4444-4444-8444-444444444444",
    snapshot = "55555555-5555-4555-8555-555555555555";
  const occurrence = pageDigest({
    vault_uid: vault,
    node_id: 7,
    version_id: review.selection.source.version_id,
  }).slice(0, 32);
  const recipe = {
    contract: "native-pdf-numbered/v1",
    vault_uid: vault,
    selection: review.selection,
    preparation_sha256: review.preparation_sha256,
    decisions_sha256: review.decisions_sha256,
    review_sha256: review.sha256,
    resolved_sha256: review.resolved.sha256,
    renderer: review.recipe,
    namespace: { id: ns.namespace_id, prefix: "CASE", suffix: "", padding: 2, start: 3 },
    page_count: 1,
    pages: review.resolved.pages,
    footer: {
      strip_height: 5000,
      inset_x: 2000,
      inset_top: 1000,
      inset_bottom: 1000,
      font_sha256: review.recipe.font_sha256,
      font_size_milli_points: 10000,
    },
    original_retention: "indefinite",
    original_release_available: false,
  };
  const recipe_sha256 = pageDigest(recipe);
  return {
    contract: "native-pdf-numbered-reservation/v1",
    allocation_id: "66666666-6666-4666-8666-666666666666",
    snapshot_id: snapshot,
    recipe,
    recipe_sha256,
    state: "reserved",
    request_sha256: pageDigest({
      namespace_id: ns.namespace_id,
      snapshot_id: snapshot,
      recipe_sha256,
      start_at: 3,
      pages: [
        {
          OccurrenceID: occurrence,
          UnstampedSHA256: "a".repeat(64),
          SourcePage: 1,
          VerifiedPageCount: 1,
        },
      ],
    }),
    pages: [
      {
        ordinal: 1,
        occurrence_id: occurrence,
        source_blob_sha256: "a".repeat(64),
        source_page: 1,
        output_page: 1,
        label: "CASE03",
      },
    ],
  };
}
it("binds a reservation to reviewed content and exact labels, beyond internally consistent hashes", () => {
  expect(decodeNumberedReservation(response(), frozen).pages[0]!.label).toBe("CASE03");
  for (const change of [
    (r: ReturnType<typeof response>) => {
      r.recipe.namespace.start = 4;
    },
    (r: ReturnType<typeof response>) => {
      r.recipe.review_sha256 = "9".repeat(64);
    },
    (r: ReturnType<typeof response>) => {
      r.recipe.pages = [{ ...r.recipe.pages[0]!, height: 110001 }];
    },
    (r: ReturnType<typeof response>) => {
      r.pages[0]!.label = "CASE04";
    },
    (r: ReturnType<typeof response>) => {
      r.recipe.original_release_available = true;
    },
  ]) {
    const changed = response();
    change(changed);
    changed.recipe_sha256 = pageDigest(changed.recipe);
    expect(() => decodeNumberedReservation(changed, frozen)).toThrow();
  }
});
