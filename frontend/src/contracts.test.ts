import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import {
  addCivilDays,
  contractDocumentSource,
  contractListParams,
  contractTypeOptions,
  contractStatusLabel,
  contractTypeLabel,
  counterpartyNames,
  documentRoleLabel,
  formatCivilDate,
  noContractFilters,
  visibleContracts,
} from "./contracts.js";
import { contract, contractLink, nodeFixture, versionFixture } from "./contract-test-fixtures.js";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllEnvs();
});

describe("contract helpers", () => {
  it("adds calendar days across months and leap days", () => {
    expect(addCivilDays("2027-01-31", 31)).toBe("2027-03-03");
    expect(addCivilDays("2028-02-28", 1)).toBe("2028-02-29");
    expect(addCivilDays("2027-12-31", 1)).toBe("2028-01-01");
  });

  it("sends an inclusive window as an exclusive cutoff at the daemon date", () => {
    expect(
      contractListParams({ status: "active", type: "", expiringDays: 30 }, "2027-01-01", 0),
    ).toStrictEqual({
      status: "active",
      expiring_before: "2027-02-01",
      as_of: "2027-01-01",
      limit: 100,
      offset: 0,
    });
    expect(contractListParams(noContractFilters, "", 0)).toStrictEqual({ limit: 100, offset: 0 });
    expect(
      contractListParams({ status: "", type: "lease", expiringDays: 0 }, "2027-01-01", 200),
    ).toStrictEqual({ type: "lease", as_of: "2027-01-01", limit: 100, offset: 200 });
  });

  it("hides agreements that expired before the daemon date when a window is set", () => {
    const items = [
      contract({ contract_id: "a", expires_on: "2026-12-31" }),
      contract({ contract_id: "b", expires_on: "2027-01-01" }),
      contract({ contract_id: "c", expires_on: "2027-01-31" }),
    ];
    const window = { ...noContractFilters, expiringDays: 30 as const };
    expect(visibleContracts(items, window, "2027-01-01").map((c) => c.contract_id)).toStrictEqual(
      ["b", "c"],
    );
    expect(visibleContracts(items, noContractFilters, "2027-01-01")).toHaveLength(3);
  });

  it("formats civil dates in UTC regardless of the browser time zone", () => {
    vi.stubEnv("TZ", "America/Los_Angeles");
    expect(formatCivilDate("2027-01-01")).toBe(
      new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeZone: "UTC" }).format(
        Date.UTC(2027, 0, 1),
      ),
    );
    expect(formatCivilDate(null)).toBe("—");
  });

  it("labels types, statuses and roles", () => {
    expect(contractTypeLabel("offer_letter")).toBe("Offer letter");
    expect(contractTypeLabel("nda")).toBe("NDA");
    expect(contractTypeLabel("")).toBe("—");
    expect(contractStatusLabel("pending_signature")).toBe("Pending signature");
    expect(contractStatusLabel("paused")).toBe("paused");
    expect(documentRoleLabel("audit_trail")).toBe("Audit trail");
  });

  it("joins counterparties in order and ignores own entities", () => {
    const value = contract();
    expect(counterpartyNames(value)).toBe("Northwind Storage Ltd, Riverbend Analytics LLC");
    expect(counterpartyNames({ ...value, parties: value.parties.slice(0, 1) })).toBe("—");
  });

  it("offers the suggested types and seen labels once", () => {
    expect(contractTypeOptions(["licence", "lease", "", "licence"])).toStrictEqual([
      { value: "", label: "Any type" },
      { value: "service", label: "Service" },
      { value: "lease", label: "Lease" },
      { value: "loan", label: "Loan" },
      { value: "nda", label: "NDA" },
      { value: "offer_letter", label: "Offer letter" },
      { value: "licence", label: "Licence" },
    ]);
  });
});

describe("contract document source", () => {
  function serve(version: unknown, node: unknown) {
    return vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const url = String(input);
      if (url.startsWith("/api/v1/versions/")) return Response.json(version);
      if (url.startsWith("/api/v1/nodes/")) return Response.json(node);
      throw new Error(`unexpected ${url}`);
    });
  }

  it("binds the reader to the recorded older version", async () => {
    const link = contractLink({ current: false });
    const fetch = serve(versionFixture(link), nodeFixture());
    const source = await contractDocumentSource("synthetic", link, new AbortController().signal);
    expect(source).toMatchObject({
      kind: "related",
      nodeID: link.node_id,
      versionID: link.content_version_id,
      blobHash: link.blob_sha256,
      size: link.size,
      name: "lease-signed.pdf",
    });
    expect(fetch.mock.calls.map(([url]) => String(url))).toStrictEqual([
      `/api/v1/versions/${link.content_version_id}`,
      `/api/v1/nodes/${link.node_id}`,
    ]);
  });

  it("accepts sizes beyond the attachment limit", async () => {
    const link = contractLink({ size: 200 * 1024 * 1024 });
    serve(versionFixture(link), nodeFixture());
    await expect(
      contractDocumentSource("synthetic", link, new AbortController().signal),
    ).resolves.toMatchObject({ size: link.size });
  });

  it("refuses changed, trashed and unavailable sources", async () => {
    const link = contractLink();
    serve({ ...versionFixture(link), blob_hash: "c".repeat(64) }, nodeFixture());
    await expect(
      contractDocumentSource("synthetic", link, new AbortController().signal),
    ).rejects.toThrow("no longer matches its saved version");
    vi.restoreAllMocks();
    serve(versionFixture(link), nodeFixture({ trashed_at: "2027-01-02T00:00:00Z" }));
    await expect(
      contractDocumentSource("synthetic", link, new AbortController().signal),
    ).rejects.toThrow("no longer available");
    vi.restoreAllMocks();
    const fetch = vi.spyOn(globalThis, "fetch");
    await expect(
      contractDocumentSource(
        "synthetic",
        contractLink({ available: false }),
        new AbortController().signal,
      ),
    ).rejects.toThrow("no longer available");
    expect(fetch).not.toHaveBeenCalled();
  });
});
