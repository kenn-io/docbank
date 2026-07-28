import { describe, expect, it } from "vitest";
import { hmac } from "@noble/hashes/hmac.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { utf8ToBytes } from "@noble/hashes/utils.js";
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

  it("accepts collision suffixes for additions and provenance matches for skips", () => {
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
    receipt.node.name = "renamed-quarterly-report.txt";
    expect(() =>
      validateUploadReceipt(receipt, 2, "report.txt", hash, 3),
    ).not.toThrow();
    receipt.status = "added";
    expect(() =>
      validateUploadReceipt(receipt, 2, "report.txt", hash, 3),
    ).toThrow(/did not match the declared file authority/);
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

  it("retires the channel when cancellation races the terminal receipt", async () => {
    const token = "browser-session";
    const uploadSecret = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE";
    let endUpload: (() => void) | undefined;
    const ended = new Promise<void>((resolve) => {
      endUpload = resolve;
    });
    class DaemonSocket extends EventTarget {
      readyState: number = WebSocket.OPEN;
      bufferedAmount = 0;
      binaryType = "blob";
      closed = false;

      constructor() {
        super();
        queueMicrotask(() => this.dispatchEvent(new Event("open")));
      }

      send(data: string | ArrayBuffer): void {
        if (typeof data !== "string") return;
        const message = JSON.parse(data) as {
          type: string;
          nonce?: string;
          request_id?: string;
        };
        if (message.type === "authenticate") {
          const padded = uploadSecret.replaceAll("-", "+").replaceAll("_", "/").padEnd(
            Math.ceil(uploadSecret.length / 4) * 4,
            "=",
          );
          const secret = Uint8Array.from(
            atob(padded),
            (character) => character.charCodeAt(0),
          );
          const proofBytes = hmac(
            sha256,
            secret,
            utf8ToBytes(
              `docbank-web-upload-v1\u0000${token}\u0000${message.nonce}`,
            ),
          );
          let proof = "";
          for (const byte of proofBytes) proof += String.fromCharCode(byte);
          proof = btoa(proof)
            .replaceAll("+", "-")
            .replaceAll("/", "_")
            .replace(/=+$/, "");
          queueMicrotask(() =>
            this.dispatchEvent(new MessageEvent("message", {
              data: JSON.stringify({ type: "authenticated", proof }),
            })),
          );
        } else if (message.type === "begin") {
          queueMicrotask(() =>
            this.dispatchEvent(new MessageEvent("message", {
              data: JSON.stringify({
                type: "ready",
                request_id: message.request_id,
              }),
            })),
          );
        } else if (message.type === "end") {
          endUpload?.();
        }
      }

      close(): void {
        if (this.closed) return;
        this.closed = true;
        this.readyState = WebSocket.CLOSED;
        this.dispatchEvent(new Event("close"));
      }
    }

    const socket = new DaemonSocket();
    let failed = 0;
    const channel = new VerifiedUploadChannel(
      { token, uploadSecret },
      () => socket as unknown as WebSocket,
      () => failed++,
    );
    await channel.connect();
    const controller = new AbortController();
    const upload = channel.uploadFile(
      2,
      new File(["abc"], "report.txt", { type: "text/plain" }),
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
      controller.signal,
      () => {},
    );
    await ended;
    controller.abort();

    await expect(upload).rejects.toMatchObject({ name: "AbortError" });
    expect(socket.closed).toBe(true);
    expect(failed).toBe(1);
  });
});
