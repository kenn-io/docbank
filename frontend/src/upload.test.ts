import { describe, expect, it } from "vitest";
import type { UploadReceipt } from "./api.js";
import { hashFile, validateUploadReceipt } from "./upload.js";

describe("verified browser upload", () => {
  it("hashes file bytes incrementally and reports terminal progress", async () => {
    const file = new File(["abc"], "report.txt", { type: "text/plain" });
    const progress: number[] = [];

    const digest = await hashFile(
      file,
      new AbortController().signal,
      (current) => progress.push(current.processed),
    );

    expect(digest).toBe(
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
    );
    expect(progress).toEqual([0, 3]);
  });

  it("rejects a server receipt that does not bind the declared authority", () => {
    const hash = "a".repeat(64);
    const receipt: UploadReceipt = {
      status: "added",
      computed_hash: hash,
      computed_size: 3,
      node: {
        id: 9,
        parent_id: 2,
        name: "report.txt",
        kind: "file",
        blob_hash: "b".repeat(64),
        size: 3,
        revision: 1,
        created_at: "2026-07-28T00:00:00Z",
        modified_at: "2026-07-28T00:00:00Z",
      },
    };

    expect(() =>
      validateUploadReceipt(receipt, 2, "report.txt", hash, 3),
    ).toThrow(/did not match the declared file authority/);
  });

  it("accepts added and skipped receipts in the requested suffix family", () => {
    const hash = "a".repeat(64);
    const receipt: UploadReceipt = {
      status: "added",
      computed_hash: hash,
      computed_size: 3,
      node: {
        id: 9,
        parent_id: 2,
        name: "report (2).txt",
        kind: "file",
        blob_hash: hash,
        size: 3,
        revision: 1,
        created_at: "2026-07-28T00:00:00Z",
        modified_at: "2026-07-28T00:00:00Z",
      },
    };

    expect(() =>
      validateUploadReceipt(receipt, 2, "report.txt", hash, 3),
    ).not.toThrow();
    receipt.status = "skipped";
    expect(() =>
      validateUploadReceipt(receipt, 2, "report.txt", hash, 3),
    ).not.toThrow();
  });
});
