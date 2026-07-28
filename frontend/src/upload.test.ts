import { afterEach, describe, expect, it, vi } from "vitest";
import { hmac } from "@noble/hashes/hmac.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { utf8ToBytes } from "@noble/hashes/utils.js";
import type { UploadReceipt } from "./api.js";
import {
  hashFile,
  validateUploadReceipt,
  VerifiedUploadChannel,
} from "./upload.js";

const testToken = "browser-session";
const testUploadSecret = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE";

function daemonProof(nonce: string): string {
  const proofBytes = hmac(
    sha256,
    new Uint8Array(32).fill(1),
    utf8ToBytes(
      `docbank-web-upload-v1\u0000${testToken}\u0000${nonce}`,
    ),
  );
  let proof = "";
  for (const byte of proofBytes) proof += String.fromCharCode(byte);
  return btoa(proof)
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replace(/=+$/, "");
}

class DaemonSocket extends EventTarget {
  readyState: number = WebSocket.OPEN;
  bufferedAmount = 0;
  binaryType = "blob";
  closed = false;
  authenticate = true;
  ready = true;
  cancelAcknowledgment = true;
  onbegin: (() => void) | undefined;
  onend: (() => void) | undefined;
  oncancel: (() => void) | undefined;

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
    if (message.type === "authenticate" && this.authenticate) {
      this.emit({ type: "authenticated", proof: daemonProof(message.nonce ?? "") });
    } else if (message.type === "begin") {
      this.onbegin?.();
      if (this.ready) {
        this.emit({ type: "ready", request_id: message.request_id });
      }
    } else if (message.type === "end") {
      this.onend?.();
    } else if (message.type === "cancel") {
      this.oncancel?.();
      if (this.cancelAcknowledgment) {
        this.emit({
          type: "error",
          request_id: message.request_id,
          status: 499,
          code: "canceled",
        });
      }
    }
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.readyState = WebSocket.CLOSED;
    this.dispatchEvent(new Event("close"));
  }

  private emit(message: object): void {
    queueMicrotask(() =>
      this.dispatchEvent(new MessageEvent("message", {
        data: JSON.stringify(message),
      })),
    );
  }
}

afterEach(() => {
  vi.useRealTimers();
});

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
    let endUpload: (() => void) | undefined;
    const ended = new Promise<void>((resolve) => {
      endUpload = resolve;
    });
    const socket = new DaemonSocket();
    socket.onend = () => endUpload?.();
    let failed = 0;
    const channel = new VerifiedUploadChannel(
      { token: testToken, uploadSecret: testUploadSecret },
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

  it("times out a daemon that never proves the upload channel", async () => {
    vi.useFakeTimers();
    const socket = new DaemonSocket();
    socket.authenticate = false;
    const channel = new VerifiedUploadChannel(
      { token: testToken, uploadSecret: testUploadSecret },
      () => socket as unknown as WebSocket,
    );

    const connecting = channel.connect();
    const rejected = expect(connecting).rejects.toThrow(/channel ended/);
    await vi.advanceTimersByTimeAsync(10_000);

    await rejected;
    expect(socket.closed).toBe(true);
  });

  it("aborts while waiting for readiness and backpressure", async () => {
    const readySocket = new DaemonSocket();
    readySocket.ready = false;
    const readyChannel = new VerifiedUploadChannel(
      { token: testToken, uploadSecret: testUploadSecret },
      () => readySocket as unknown as WebSocket,
    );
    await readyChannel.connect();
    let sawBegin: (() => void) | undefined;
    const began = new Promise<void>((resolve) => {
      sawBegin = resolve;
    });
    readySocket.onbegin = () => sawBegin?.();
    const readyController = new AbortController();
    const waitingReady = readyChannel.uploadFile(
      2,
      new File(["abc"], "report.txt"),
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
      readyController.signal,
      () => {},
    );
    await began;
    readyController.abort();
    await expect(waitingReady).rejects.toMatchObject({ name: "AbortError" });
    expect(readySocket.closed).toBe(true);

    const pressureSocket = new DaemonSocket();
    pressureSocket.bufferedAmount = 3 * 1024 * 1024;
    const pressureChannel = new VerifiedUploadChannel(
      { token: testToken, uploadSecret: testUploadSecret },
      () => pressureSocket as unknown as WebSocket,
    );
    await pressureChannel.connect();
    const pressureController = new AbortController();
    let becameReady: (() => void) | undefined;
    const readyProgress = new Promise<void>((resolve) => {
      becameReady = resolve;
    });
    const waitingPressure = pressureChannel.uploadFile(
      2,
      new File(["abc"], "report.txt"),
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
      pressureController.signal,
      (progress) => {
        if (progress.processed === 0) becameReady?.();
      },
    );
    await readyProgress;
    pressureController.abort();
    await expect(waitingPressure).rejects.toMatchObject({ name: "AbortError" });
    expect(pressureSocket.closed).toBe(true);
  });

  it("bounds cancellation acknowledgment and rejects a waiter on close", async () => {
    vi.useFakeTimers();
    const cancelSocket = new DaemonSocket();
    cancelSocket.cancelAcknowledgment = false;
    const cancelChannel = new VerifiedUploadChannel(
      { token: testToken, uploadSecret: testUploadSecret },
      () => cancelSocket as unknown as WebSocket,
    );
    await cancelChannel.connect();
    const cancelController = new AbortController();
    let sawCancel: (() => void) | undefined;
    const canceled = new Promise<void>((resolve) => {
      sawCancel = resolve;
    });
    cancelSocket.oncancel = () => sawCancel?.();
    const waitingCancel = cancelChannel.uploadFile(
      2,
      new File(["abc"], "report.txt"),
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
      cancelController.signal,
      (progress) => {
        if (progress.processed === 0) cancelController.abort();
      },
    );
    const cancelRejected = expect(waitingCancel).rejects.toMatchObject({
      name: "AbortError",
    });
    await canceled;
    await vi.advanceTimersByTimeAsync(10_000);
    await cancelRejected;
    expect(cancelSocket.closed).toBe(true);

    vi.useRealTimers();
    const closeSocket = new DaemonSocket();
    closeSocket.ready = false;
    const closeChannel = new VerifiedUploadChannel(
      { token: testToken, uploadSecret: testUploadSecret },
      () => closeSocket as unknown as WebSocket,
    );
    await closeChannel.connect();
    let closeBegin: (() => void) | undefined;
    const closeBegan = new Promise<void>((resolve) => {
      closeBegin = resolve;
    });
    closeSocket.onbegin = () => closeBegin?.();
    const waitingClose = closeChannel.uploadFile(
      2,
      new File(["abc"], "report.txt"),
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
      new AbortController().signal,
      () => {},
    );
    await closeBegan;
    closeChannel.close();
    await expect(waitingClose).rejects.toThrow(/channel ended/);
  });
});
