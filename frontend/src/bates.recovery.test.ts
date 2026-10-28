import { afterEach, expect, it, vi } from "vite-plus/test";
import { findBatesExportsByLabel, readBatesRecoveryDetail } from "./bates.js";
import {
  recoveryCandidate as candidate,
  recoveryReceipt,
  recoveryJSON,
} from "./bates-recovery.test-fixtures.js";

afterEach(() => vi.restoreAllMocks());

it("preserves the exact label and binds the requested evidence", async () => {
  const label = " QUAL000002 ";
  const fetcher = vi.spyOn(globalThis, "fetch").mockResolvedValue(
    recoveryJSON({
      items: [{ ...candidate, evidence: [{ ...candidate.evidence[0], label }] }],
    }),
  );
  const result = await findBatesExportsByLabel("session", label, undefined, "next-page");
  const [url, init] = fetcher.mock.calls[0]!;
  expect(new URL(String(url), "http://localhost").searchParams).toEqual(
    new URLSearchParams({ bates_label: label, limit: "50", cursor: "next-page" }),
  );
  expect(new Headers(init?.headers).get("X-Docbank-Web-Session")).toBe("session");
  expect(result.items[0]?.artifact_id).toBe(candidate.artifact_id);
});

it("bounds input without truncating the admitted label", async () => {
  const fetcher = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation(async () => recoveryJSON({ items: [] }));
  await expect(findBatesExportsByLabel("session", "")).rejects.toThrow();
  await expect(findBatesExportsByLabel("session", "A".repeat(267))).rejects.toThrow();
  expect(fetcher).not.toHaveBeenCalled();
  await expect(findBatesExportsByLabel("session", "A".repeat(266))).resolves.toMatchObject({
    items: [],
  });
});

it.each([
  { evidence: [{ ...candidate.evidence[0], label: "QUAL000001" }] },
  { evidence: [{ ...candidate.evidence[0], output_page: 3 }] },
  { artifact_id: "not-an-id" },
  { blob_sha256: "wrong" },
  { media_type: "text/plain" },
  { state: "pending" },
])("rejects a mismatched candidate %j", async (change) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    recoveryJSON({ items: [{ ...candidate, ...change }] }),
  );
  await expect(findBatesExportsByLabel("session", "QUAL000002")).rejects.toThrow();
});

it("rejects more than the requested number of results", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    recoveryJSON({ items: Array(51).fill(candidate) }),
  );
  await expect(findBatesExportsByLabel("session", "QUAL000002")).rejects.toThrow();
});

it.each([
  ["native-pdf-numbered/v1", "Numbered redaction"],
  ["bates-stamp/v1", "Stamped export"],
])("describes the recorded %s recipe without qualifying today's engine", async (contract, kind) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(recoveryJSON(recoveryReceipt(contract)));
  const result = await readBatesRecoveryDetail("session", candidate);
  expect(result.kind).toBe(kind);
  expect(result.namespace.prefix).toBe("QUAL");
  expect(result.namespace.suffix).toBe("");
});

it.each([
  { artifact_id: "55555555-5555-4555-8555-555555555555" },
  { allocation_id: "55555555-5555-4555-8555-555555555555" },
  { size: 12346 },
  { blob_sha256: "f".repeat(64) },
  { namespace: undefined },
  { recipe: undefined },
])("refuses a detail that cannot describe the selected candidate %j", async (change) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    recoveryJSON({ ...recoveryReceipt(), ...change }),
  );
  await expect(readBatesRecoveryDetail("session", candidate)).rejects.toThrow();
});
