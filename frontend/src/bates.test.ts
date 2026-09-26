import { afterEach, expect, it, vi } from "vitest";
import {
  batesRecipe,
  batesRecipeSHA256,
  listBatesSources,
  downloadBatesExport,
  listBatesExports,
  readBatesExport,
  startBatesExport,
} from "./bates.js";

afterEach(() => {
  vi.restoreAllMocks();
});

it("hashes a recipe to the same digest as the Go stamp engine", async () => {
  const recipe = batesRecipe({
    namespace_id: "ns-synthetic",
    prefix: "OUR",
    suffix: "",
    padding: 6,
    created_at: "2026-09-21T00:00:00Z",
  }, 41, "bottom-right", 24);

  // Golden digest from internal/pdfstamp/stamp_test.go validRecipe.
  expect(await batesRecipeSHA256(recipe)).toBe("f3c4cebd1ea16ad54b0784b96ee73f3f2bd8c644288a9ba1a1d67d8dbd720787");
});

it("keeps only sealed package snapshots with pages", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({
    items: [
      { package_id: "one", package_name: "Review set", direction: "received", state: "complete", snapshot_id: "11111111-1111-4111-8111-111111111111", page_count: 3 },
      { package_id: "two", package_name: "No pages", direction: "received", state: "complete", snapshot_id: "22222222-2222-4222-8222-222222222222", page_count: 0 },
      { package_id: "three", package_name: "Pending", direction: "received", state: "running", snapshot_id: "33333333-3333-4333-8333-333333333333", page_count: 2 },
      { package_id: "four", package_name: "Partial review", direction: "received", state: "partial", snapshot_id: "44444444-4444-4444-8444-444444444444", page_count: 1 },
      { package_id: "five", package_name: "Produced review", direction: "produced", state: "sealed", snapshot_id: "55555555-5555-4555-8555-555555555555", page_count: 2 },
    ],
    limit: 250,
  }), { headers: { "Content-Type": "application/json" } }));

  await expect(listBatesSources("session")).resolves.toEqual([
    { package_id: "one", package_name: "Review set", snapshot_id: "11111111-1111-4111-8111-111111111111", page_count: 3 },
    { package_id: "four", package_name: "Partial review", snapshot_id: "44444444-4444-4444-8444-444444444444", page_count: 1 },
    { package_id: "five", package_name: "Produced review", snapshot_id: "55555555-5555-4555-8555-555555555555", page_count: 2 },
  ]);
  expect(fetch).toHaveBeenCalledWith(
    "/api/v1/packages?limit=250",
    expect.objectContaining({ method: "GET" }),
  );
});

it("pages through every package and keeps oversized sources for the drawer to explain", async () => {
  const pageOne = Array.from({ length: 250 }, (_, index) => ({
    package_id: `p${index}`, package_name: `Set ${index}`, direction: "received", state: "complete",
    snapshot_id: "11111111-1111-4111-8111-111111111111", page_count: 1,
  }));
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/packages?limit=250") return new Response(JSON.stringify({ items: pageOne, limit: 250, next_after: "p249" }), { headers: { "Content-Type": "application/json" } });
    if (url === "/api/v1/packages?limit=250&after=p249") {
      return new Response(JSON.stringify({ items: [
        { package_id: "big", package_name: "Oversized", direction: "produced", state: "sealed", snapshot_id: "22222222-2222-4222-8222-222222222222", page_count: 251 },
      ], limit: 250 }), { headers: { "Content-Type": "application/json" } });
    }
    throw new Error(`unexpected request ${url}`);
  });

  const sources = await listBatesSources("session");

  expect(sources).toHaveLength(251);
  expect(sources.at(-1)).toEqual({ package_id: "big", package_name: "Oversized", snapshot_id: "22222222-2222-4222-8222-222222222222", page_count: 251 });
});

it("starts, reads, lists, and prepares a verified publication", async () => {
  const namespace = {
    namespace_id: "11111111-1111-4111-8111-111111111111", prefix: "CASE", suffix: "", padding: 6,
    created_at: "2026-09-21T00:00:00Z",
  };
  const recipe = batesRecipe(namespace, 1);
  const recipeHash = await batesRecipeSHA256(recipe);
  const allocation = {
    allocation_id: "22222222-2222-4222-8222-222222222222", namespace_id: namespace.namespace_id,
    snapshot_id: "33333333-3333-4333-8333-333333333333", recipe_sha256: recipeHash,
    state: "reserved" as const, start_sequence: 1, end_sequence: 1, created_at: "2026-09-21T00:01:00Z",
    labels: [{ ordinal: 1, occurrence_id: "a".repeat(32), source_page: 1, output_page: 1, label: "CASE000001" }],
  };
  const published = {
    artifact_id: "44444444-4444-4444-8444-444444444444", allocation_id: allocation.allocation_id,
    blob_sha256: "a".repeat(64), size: 1200, media_type: "application/pdf", page_count: 1,
    recipe_sha256: recipeHash, manifest_sha256: "b".repeat(64), state: "verified",
    created_at: "2026-09-21T00:02:00Z",
    pages: [{ ordinal: 1, occurrence_id: "a".repeat(32), source_blob_sha256: "c".repeat(64), source_page: 1, output_page: 1, label: "CASE000001" }],
  };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith("/download")) {
      expect(init).toMatchObject({ method: "POST", body: "{}" });
      expect(new Headers(init?.headers).get("Content-Type")).toBe("application/json");
      return new Response(JSON.stringify({
        url: "/api/daemon/web-download/file?ticket=one-use", name: "CASE000001.pdf",
        allocation_id: allocation.allocation_id, blob_sha256: published.blob_sha256, size: published.size,
      }), { headers: { "Content-Type": "application/json" } });
    }
    if (url.includes("?limit=50")) return new Response(JSON.stringify({ items: [published], total: 1 }), { headers: { "Content-Type": "application/json" } });
    if (init?.method === "POST") {
      expect(JSON.parse(String(init.body))).toEqual({ allocation_id: allocation.allocation_id, recipe });
    }
    return new Response(JSON.stringify(published), { headers: { "Content-Type": "application/json" } });
  });

  await expect(startBatesExport("session", allocation, recipe)).resolves.toMatchObject({ state: "verified" });
  await expect(readBatesExport("session", allocation.allocation_id)).resolves.toMatchObject({ allocation_id: allocation.allocation_id });
  await expect(listBatesExports("session")).resolves.toMatchObject({ total: 1, items: [{ artifact_id: published.artifact_id }] });
  const clicked: { href: string; download: string }[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    clicked.push({ href: this.getAttribute("href") ?? "", download: this.download });
  });
  await downloadBatesExport("session", published);
  expect(clicked).toEqual([{ href: "/api/daemon/web-download/file?ticket=one-use", download: "CASE000001.pdf" }]);
});
