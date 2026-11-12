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
  it("adds calendar days across months, leap days and years below 100", () => {
    expect(addCivilDays("2027-01-31", 31)).toBe("2027-03-03");
    expect(addCivilDays("2028-02-28", 1)).toBe("2028-02-29");
    expect(addCivilDays("2027-12-31", 1)).toBe("2028-01-01");
    expect(addCivilDays("0099-12-31", 1)).toBe("0100-01-01");
    expect(addCivilDays("0001-01-01", 30)).toBe("0001-01-31");
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
    const [a, b, c] = ["a", "b", "c"].map((d) => `${d.repeat(8)}-0000-4000-8000-000000000000`);
    const items = [
      contract({ contract_id: a, expires_on: "2026-12-31", effective_status: "expired" }),
      contract({ contract_id: b, expires_on: "2027-01-01" }),
      contract({ contract_id: c, expires_on: "2027-01-31" }),
    ];
    const window = { ...noContractFilters, expiringDays: 30 as const };
    expect(visibleContracts(items, window, "2027-01-01").map((c) => c.contract_id)).toStrictEqual([
      b,
      c,
    ]);
    expect(visibleContracts(items, noContractFilters, "2027-01-01")).toHaveLength(3);
  });

  it("formats civil dates in UTC regardless of the browser time zone or a year below 100", () => {
    vi.stubEnv("TZ", "America/Los_Angeles");
    const utc = (year: number, month: number, day: number) => {
      const value = new Date(0);
      value.setUTCFullYear(year, month, day);
      return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeZone: "UTC" }).format(
        value,
      );
    };
    expect(formatCivilDate("2027-01-01")).toBe(utc(2027, 0, 1));
    expect(formatCivilDate("0099-01-01")).toBe(utc(99, 0, 1));
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

  it("gives every valid type a visible label distinct from the others", () => {
    // Store labels are lower-case letters, digits and underscores, so lease
    // and lease_ are different types and any_type is not the placeholder.
    const options = contractTypeOptions(["lease_", "_", "any_type", "lease"]);
    expect(new Set(options.map((option) => option.value))).toStrictEqual(
      new Set(["", "service", "lease", "loan", "nda", "offer_letter", "lease_", "_", "any_type"]),
    );
    expect(options[0]).toStrictEqual({ value: "", label: "Any type" });
    for (const value of ["", "lease", "lease_", "_", "any_type"])
      expect(options.filter((option) => option.value === value)).toHaveLength(1);
    // Labels compare as displayed: surrounding spaces are not visible.
    const labels = options.map((option) => option.label.trim());
    expect(new Set(labels).size).toBe(labels.length);
    expect(contractTypeLabel("lease_").trim()).not.toBe(contractTypeLabel("lease").trim());
    expect(contractTypeLabel("_").trim()).not.toBe("");
    expect(contractTypeLabel("_")).not.toBe(contractTypeLabel(""));
    expect(contractTypeLabel("any_type")).not.toBe(options[0]!.label);
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

  it("returns the source for the recorded identity at any size", async () => {
    // Larger than the attachment limit, which contract links do not share.
    const link = contractLink({ size: 200 * 1024 * 1024 });
    serve(versionFixture(link), nodeFixture());
    await expect(
      contractDocumentSource("synthetic", link, new AbortController().signal),
    ).resolves.toMatchObject({
      kind: "related",
      nodeID: link.node_id,
      versionID: link.content_version_id,
      blobHash: link.blob_sha256,
      size: link.size,
      name: "lease-signed.pdf",
    });
  });

  it("accepts a legal 16,384-byte node path and refuses a longer one", async () => {
    const link = contractLink();
    const path = "/" + "x".repeat(16383);
    const fetch = serve(versionFixture(link), nodeFixture({ path }));
    await expect(
      contractDocumentSource("synthetic", link, new AbortController().signal),
    ).resolves.toMatchObject({
      nodeID: link.node_id,
      versionID: link.content_version_id,
      blobHash: link.blob_sha256,
      size: link.size,
      path,
    });
    expect(fetch.mock.calls.map(([url]) => String(url))).toStrictEqual([
      `/api/v1/versions/${link.content_version_id}`,
      `/api/v1/nodes/${link.node_id}`,
    ]);
    vi.restoreAllMocks();
    serve(versionFixture(link), nodeFixture({ path: path + "x" }));
    await expect(
      contractDocumentSource("synthetic", link, new AbortController().signal),
    ).rejects.toThrow("no longer matches its saved version");
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
