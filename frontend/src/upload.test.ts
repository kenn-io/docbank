import { describe, expect, it } from "vitest";
import type { UploadReceipt } from "./api.js";
import {
  hashFile,
  validateUploadReceipt,
  VerifiedUploadChannel,
} from "./upload.js";

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

  it("sends no file bytes when an endpoint cannot prove daemon ownership", async () => {
    class ImpostorSocket extends EventTarget {
      readonly sent: unknown[] = [];
      readyState: number = WebSocket.OPEN;
      binaryType = "blob";

      constructor() {
        super();
        queueMicrotask(() => this.dispatchEvent(new Event("open")));
      }

      send(data: unknown): void {
        this.sent.push(data);
        queueMicrotask(() =>
          this.dispatchEvent(
            new MessageEvent("message", {
              data: JSON.stringify({ type: "authenticated", proof: "forged" }),
            }),
          ),
        );
      }

      close(): void {
        this.readyState = WebSocket.CLOSED;
        this.dispatchEvent(new Event("close"));
      }
    }

    const socket = new ImpostorSocket();
    const channel = new VerifiedUploadChannel(
      {
        token: "browser-session",
        uploadSecret: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
      },
      () => socket as unknown as WebSocket,
    );

    await expect(channel.connect()).rejects.toThrow(/No file bytes were sent/);
    expect(socket.sent).toHaveLength(1);
    expect(typeof socket.sent[0]).toBe("string");
    expect(String(socket.sent[0])).not.toContain("private document");
  });
});
