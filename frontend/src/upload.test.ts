import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { hmac } from "@noble/hashes/hmac.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { utf8ToBytes } from "@noble/hashes/utils.js";
import type { UploadReceipt } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import {
  hashFile,
  photoImportDeclaration,
  UploadChannelError,
  validateUploadReceipt,
  VerifiedUploadChannel,
} from "./upload.js";

import { photoSelectionFiles } from "./photo-test-fixtures.js";

const testToken = "browser-session";
const testUploadSecret = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE";

function oversizedPhotoFiles(): File[] {
  const folder = ["旅".repeat(70), "旅".repeat(70), "旅".repeat(70)].join("/");
  return Array.from({ length: 11000 }, (_, index) => {
    const file = new File([], `photo-${index}.JPG`, { lastModified: 0 });
    Object.defineProperty(file, "webkitRelativePath", { value: `${folder}/${file.name}` });
    return file;
  });
}

function daemonProof(nonce: string): string {
  const proofBytes = hmac(
    sha256,
    new Uint8Array(32).fill(1),
    utf8ToBytes(`docbank-web-upload-v1\u0000${testToken}\u0000${nonce}`),
  );
  let proof = "";
  for (const byte of proofBytes) proof += String.fromCharCode(byte);
  return btoa(proof).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
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
  onbinary: (() => void) | undefined;
  packageContainerID = "";
  requestID = "";
  photoMembers = 0;
  frames: (string | ArrayBuffer)[] = [];

  constructor() {
    super();
    queueMicrotask(() => this.dispatchEvent(new Event("open")));
  }

  send(data: string | ArrayBuffer): void {
    this.frames.push(data);
    if (typeof data !== "string") {
      this.onbinary?.();
      return;
    }
    const message = JSON.parse(data) as {
      type: string;
      nonce?: string;
      request_id?: string;
      container_id?: string;
      hashes?: unknown[];
    };
    if (message.type === "authenticate" && this.authenticate) {
      this.emit({ type: "authenticated", proof: daemonProof(message.nonce ?? "") });
    } else if (message.type === "plan_photo_import") {
      this.emit({
        type: "photo_import_plan",
        request_id: message.request_id,
        import_id: "import-1",
        groups: [[0, 1], [2]],
        unsupported: [3],
        skip_reasons: { 3: "Unsupported type" },
      });
    } else if (message.type === "begin_photo_group") {
      this.requestID = message.request_id ?? "";
      this.photoMembers = message.hashes?.length ?? 0;
      this.emit({ type: "ready", request_id: this.requestID });
    } else if (message.type === "begin") {
      this.packageContainerID = message.container_id ?? "";
      this.requestID = message.request_id ?? "";
      this.onbegin?.();
      if (this.ready) {
        this.emit({ type: "ready", request_id: message.request_id });
      }
    } else if (message.type === "end") {
      this.onend?.();
      if (this.photoMembers) {
        this.photoMembers--;
        this.emit(
          this.photoMembers
            ? { type: "ready", request_id: this.requestID }
            : {
                type: "photo_group_receipt",
                request_id: this.requestID,
                photo_group_receipt: { status: "added", asset_id: "photo-1" },
              },
        );
      } else if (this.packageContainerID) {
        this.emit({
          type: "package_container_receipt",
          request_id: this.requestID,
          container_id: this.packageContainerID,
        });
      }
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

  emit(message: object): void {
    queueMicrotask(() =>
      this.dispatchEvent(
        new MessageEvent("message", {
          data: JSON.stringify(message),
        }),
      ),
    );
  }
}

afterEach(() => {
  vi.useRealTimers();
});

describe("verified browser upload", () => {
  it("streams package bytes through the daemon-owned socket", async () => {
    const socket = new DaemonSocket();
    const channel = new VerifiedUploadChannel(
      { token: testToken, uploadSecret: testUploadSecret },
      () => socket as unknown as WebSocket,
    );
    await channel.connect();

    await channel.uploadPackageContainer(
      "package-1",
      new File(["zip"], "production.zip"),
      "4a70fe9aa6436e9b764a7da70d4040be1580bf6796f6e02196f3a1ca1e732c59",
      new AbortController().signal,
      () => {},
    );

    expect(socket.packageContainerID).toBe("package-1");
  });

  it("hashes file bytes incrementally and reports terminal progress", async () => {
    const file = new File(["abc"], "report.txt", { type: "text/plain" });
    const progress: number[] = [];

    const digest = await hashFile(file, new AbortController().signal, (current) =>
      progress.push(current.processed),
    );

    expect(digest).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
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

    expect(() => validateUploadReceipt(receipt, 2, "report.txt", hash, 3)).toThrow(
      /did not match the declared file authority/,
    );
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

    expect(() => validateUploadReceipt(receipt, 2, "report.txt", hash, 3)).not.toThrow();
    receipt.status = "skipped";
    receipt.node.name = "renamed-quarterly-report.txt";
    expect(() => validateUploadReceipt(receipt, 2, "report.txt", hash, 3)).not.toThrow();
    receipt.status = "added";
    expect(() => validateUploadReceipt(receipt, 2, "report.txt", hash, 3)).toThrow(
      /did not match the declared file authority/,
    );
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

it("plans on the server and streams each photo member between ready and end", async () => {
  const socket = new DaemonSocket();
  const channel = new VerifiedUploadChannel(
    { token: testToken, uploadSecret: testUploadSecret },
    () => socket as unknown as WebSocket,
  );
  await channel.connect();
  const files = photoSelectionFiles();
  for (const file of files)
    Object.defineProperty(file, "webkitRelativePath", { value: `Trip/${file.name}` });
  const hashes = new Map([
    [files[0], "d7439bee24773bcbfa2d0a97947ee36227b10d1022b1a55847e928965bb6bfde"],
    [files[1], "41e5787e9f28562d07b891b1816b492309d646c0f2829743fa4963a9f9cc1d61"],
    [files[2], "0cab1c9617404faf2b24e221e189ca5945813e14d3f766345b09ca13bbe28ffc"],
  ]);
  expect(
    await channel.planPhotoImport(files, "/photos", "20000000-0000-4000-8000-000000000001", hashes),
  ).toEqual({
    import_id: "import-1",
    groups: [[0, 1], [2]],
    unsupported: [3],
    skip_reasons: { 3: "Unsupported type" },
  });
  const progress = vi.fn();
  expect(
    await channel.uploadPhotoGroup(
      "import-1",
      0,
      files.slice(0, 2),
      new AbortController().signal,
      progress,
      files.slice(0, 2).map((file) => hashes.get(file)!),
    ),
  ).toEqual({ status: "added", asset_id: "photo-1" });
  const frames = socket.frames.map((frame) =>
    typeof frame === "string" ? JSON.parse(frame) : "bytes",
  );
  expect(frames.map((frame) => (frame === "bytes" ? frame : frame.type))).toEqual([
    "authenticate",
    "plan_photo_import",
    "begin_photo_group",
    "bytes",
    "end",
    "bytes",
    "end",
  ]);
  expect(frames[1].files[0]).toEqual({
    relative_path: "Trip/capture.ARW",
    expected_hash: hashes.get(files[0]),
    size: 3,
    modified_time: 1700000000000,
  });
  expect(frames[2].import_id).toBe("import-1");
  expect(frames[1].destination).toBe("/photos");
  expect(frames[1].source_id).toBe("20000000-0000-4000-8000-000000000001");
  expect(frames[2].group).toBe(0);
  expect(frames[2].hashes).toEqual([
    "d7439bee24773bcbfa2d0a97947ee36227b10d1022b1a55847e928965bb6bfde",
    "41e5787e9f28562d07b891b1816b492309d646c0f2829743fa4963a9f9cc1d61",
  ]);
  expect(progress.mock.calls.every(([phase]) => phase === "uploading")).toBe(true);
  expect(progress).toHaveBeenLastCalledWith("uploading", {
    processed: 7,
    total: 7,
    submitted: true,
  });
  channel.close();
});

it("rejects a photo plan that doesn't account for every selected file", async () => {
  const socket = new DaemonSocket();
  const channel = new VerifiedUploadChannel(
    { token: testToken, uploadSecret: testUploadSecret },
    () => socket as unknown as WebSocket,
  );
  await channel.connect();
  const file = new File(["raw"], "capture.ARW");
  await expect(
    channel.planPhotoImport(
      [file],
      "/",
      "20000000-0000-4000-8000-000000000001",
      new Map([[file, await hashFile(file, new AbortController().signal, () => {})]]),
    ),
  ).rejects.toThrow(/invalid response/);
  expect(socket.closed).toBe(true);
});

it("rejects an oversized folder plan before sending and keeps the channel usable", async () => {
  const socket = new DaemonSocket();
  const channel = new VerifiedUploadChannel(
    { token: testToken, uploadSecret: testUploadSecret },
    () => socket as unknown as WebSocket,
  );
  await channel.connect();
  const files = oversizedPhotoFiles();
  const hashes = new Map(
    files.map((file) => [file, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"]),
  );
  await expect(
    channel.planPhotoImport(files, "/", "20000000-0000-4000-8000-000000000001", hashes),
  ).rejects.toThrow(/fewer files.*8 MiB declaration/);
  expect(socket.frames).toHaveLength(1);
  expect(socket.closed).toBe(false);
  const next = photoSelectionFiles();
  const nextHashes = new Map(
    await Promise.all(
      next.map(
        async (file) =>
          [file, await hashFile(file, new AbortController().signal, () => {})] as const,
      ),
    ),
  );
  await channel.planPhotoImport(next, "/", "20000000-0000-4000-8000-000000000002", nextHashes);
  expect(socket.frames).toHaveLength(2);
  channel.close();
});

it("reports terminal submission when an empty photo loses its receipt", async () => {
  const socket = new DaemonSocket();
  const channel = new VerifiedUploadChannel(
    { token: testToken, uploadSecret: testUploadSecret },
    () => socket as unknown as WebSocket,
  );
  await channel.connect();
  socket.onend = () => queueMicrotask(() => socket.close());
  const progress = vi.fn();
  const empty = new File([], "empty.JPG");
  await expect(
    channel.uploadPhotoGroup("import-1", 0, [empty], new AbortController().signal, progress, [
      await hashFile(empty, new AbortController().signal, () => {}),
    ]),
  ).rejects.toThrow(/channel ended/);
  expect(progress).toHaveBeenLastCalledWith("uploading", {
    processed: 0,
    total: 0,
    submitted: true,
  });
  expect(socket.frames.filter((frame) => typeof frame !== "string")).toHaveLength(0);
});

async function photoChannel(): Promise<{ socket: DaemonSocket; channel: VerifiedUploadChannel }> {
  const socket = new DaemonSocket();
  const channel = new VerifiedUploadChannel(
    { token: testToken, uploadSecret: testUploadSecret },
    () => socket as unknown as WebSocket,
  );
  await channel.connect();
  return { socket, channel };
}

it("reports a server error that arrives while photo bytes are still sending", async () => {
  const { socket, channel } = await photoChannel();
  socket.onbinary = () => {
    socket.onbinary = undefined;
    socket.emit({
      type: "error",
      request_id: socket.requestID,
      status: 507,
      code: "storage_full",
      detail: "Vault storage is full",
    });
    queueMicrotask(() => socket.close());
  };
  const large = new File([new Uint8Array(3 * 1024 * 1024)], "large.JPG");
  const digest = await hashFile(large, new AbortController().signal, () => {});
  const upload = channel.uploadPhotoGroup(
    "import-1",
    0,
    [large],
    new AbortController().signal,
    vi.fn(),
    [digest],
  );
  await expect(upload).rejects.toBeInstanceOf(APIError);
  await expect(upload).rejects.toMatchObject({
    message: "Vault storage is full",
    status: 507,
    code: "storage_full",
  });
  expect(socket.frames.filter((frame) => typeof frame !== "string")).toHaveLength(1);
  expect(socket.closed).toBe(true);
});

it("retires the channel when a photo file can't be read during upload", async () => {
  const { socket, channel } = await photoChannel();
  const unreadable = new File(["jpg"], "capture.JPG");
  let reads = 0;
  Object.defineProperty(unreadable, "slice", {
    value: (...args: Parameters<Blob["slice"]>) =>
      reads++ === 0
        ? Blob.prototype.slice.apply(unreadable, args)
        : {
            arrayBuffer: () =>
              Promise.reject(new DOMException("File changed on disk", "NotReadableError")),
          },
  });
  const digest = await hashFile(unreadable, new AbortController().signal, () => {});
  const upload = channel.uploadPhotoGroup(
    "import-1",
    0,
    [unreadable],
    new AbortController().signal,
    vi.fn(),
    [digest],
  );
  await expect(upload).rejects.toBeInstanceOf(UploadChannelError);
  await expect(upload).rejects.toThrow("File changed on disk");
  expect(socket.closed).toBe(true);
  const next = new File(["jpg"], "next.JPG");
  const progress = vi.fn();
  await expect(
    channel.uploadPhotoGroup("import-1", 1, [next], new AbortController().signal, progress, [
      await hashFile(next, new AbortController().signal, () => {}),
    ]),
  ).rejects.toThrow(/channel ended/);
  expect(progress).not.toHaveBeenCalled();
});

it("refuses a second photo group while one is active", async () => {
  const { channel } = await photoChannel();
  const firstFile = new File(["jpg"], "first.JPG");
  const secondFile = new File(["jpg"], "second.JPG");
  const first = channel.uploadPhotoGroup(
    "import-1",
    0,
    [firstFile],
    new AbortController().signal,
    vi.fn(),
    [await hashFile(firstFile, new AbortController().signal, () => {})],
  );
  await expect(
    channel.uploadPhotoGroup("import-1", 1, [secondFile], new AbortController().signal, vi.fn(), [
      await hashFile(secondFile, new AbortController().signal, () => {}),
    ]),
  ).rejects.toBeInstanceOf(UploadChannelError);
  await expect(first).resolves.toEqual({ status: "added", asset_id: "photo-1" });
  channel.close();
});

it("counts UTF-8 declaration bytes for folder files at the 8 MiB boundary", () => {
  const limit = 8 * 1024 * 1024;
  const folder = ["旅".repeat(70), "旅".repeat(70), "旅".repeat(70)].join("/");
  const files = Array.from({ length: 10500 }, (_, index) => {
    const file = new File([], `image-${String(index).padStart(5, "0")}.JPG`, { lastModified: 0 });
    Object.defineProperty(file, "webkitRelativePath", {
      value: `${folder}/${file.name}`,
      configurable: true,
    });
    return file;
  });
  const initialSize = utf8ToBytes(JSON.stringify(photoImportDeclaration(files, "/"))).byteLength;
  const remaining = limit - initialSize;
  expect(remaining).toBeGreaterThanOrEqual(0);
  const paddingPerFile = Math.floor(remaining / files.length);
  const paddedFiles = remaining % files.length;
  for (const [index, file] of files.entries()) {
    const padding = "x".repeat(paddingPerFile + (index < paddedFiles ? 1 : 0));
    Object.defineProperty(file, "webkitRelativePath", {
      value: `${folder}/${file.name.replace(".JPG", `${padding}.JPG`)}`,
    });
  }
  expect(utf8ToBytes(JSON.stringify(photoImportDeclaration(files, "/"))).byteLength).toBe(limit);
  const actual = photoImportDeclaration(
    files,
    "/",
    undefined,
    undefined,
    new Map(files.map((file) => [file, "f".repeat(64)])),
  );
  expect(utf8ToBytes(JSON.stringify(actual)).byteLength).toBe(limit);
  const last = files.at(-1)!;
  Object.defineProperty(last, "webkitRelativePath", {
    value: last.webkitRelativePath.replace(".JPG", "x.JPG"),
  });
  expect(() => photoImportDeclaration(files, "/")).toThrow(/Choose fewer files/);

  const mixedFiles = Array.from({ length: 32768 }, (_, index) => {
    const file = new File([], index === 0 ? "image.JPG" : `file-${index}.txt`, {
      lastModified: 0,
    });
    Object.defineProperty(file, "webkitRelativePath", {
      value: `${"x".repeat(130)}/${file.name}`,
    });
    return file;
  });
  const mixedActual = photoImportDeclaration(
    mixedFiles,
    "/",
    undefined,
    undefined,
    new Map([[mixedFiles[0], "f".repeat(64)]]),
  );
  expect(utf8ToBytes(JSON.stringify(mixedActual)).byteLength).toBeLessThan(limit);
  const mixedPreflight = photoImportDeclaration(mixedFiles, "/");
  expect(utf8ToBytes(JSON.stringify(mixedPreflight)).byteLength).toBe(
    utf8ToBytes(JSON.stringify(mixedActual)).byteLength,
  );
});
