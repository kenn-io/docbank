import { hmac } from "@noble/hashes/hmac.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import { APIError, type BrowserSession, type UploadReceipt } from "./api.js";

const hashChunkBytes = 1024 * 1024;
const uploadSocketPath = "/api/daemon/web-upload";
const uploadProofDomain = "docbank-web-upload-v1\u0000";
const uploadHandshakeTimeoutMs = 10_000;
const uploadResponseTimeoutMs = 60_000;

export interface TransferProgress {
  processed: number;
  total: number;
}

function uploadNameParts(name: string): { base: string; extension: string } {
  const dot = name.lastIndexOf(".");
  if (dot <= 0) return { base: name, extension: "" };
  return { base: name.slice(0, dot), extension: name.slice(dot) };
}

function permittedUploadName(requested: string, received: string): boolean {
  if (received === requested) return true;
  const { base, extension } = uploadNameParts(requested);
  const prefix = `${base} (`;
  const suffix = `)${extension}`;
  if (!received.startsWith(prefix) || !received.endsWith(suffix)) return false;
  const ordinal = received.slice(prefix.length, received.length - suffix.length);
  if (!/^[+-]?[0-9]+$/.test(ordinal)) return false;
  try {
    const value = BigInt(ordinal);
    return value >= BigInt(2) && value <= BigInt("9223372036854775807");
  } catch {
    return false;
  }
}

function abortError(): DOMException {
  return new DOMException("The operation was aborted.", "AbortError");
}

function throwIfAborted(signal: AbortSignal): void {
  if (signal.aborted) throw abortError();
}

export async function hashFile(
  file: File,
  signal: AbortSignal,
  onprogress: (progress: TransferProgress) => void,
): Promise<string> {
  const hasher = sha256.create();
  onprogress({ processed: 0, total: file.size });
  for (let offset = 0; offset < file.size; offset += hashChunkBytes) {
    throwIfAborted(signal);
    const end = Math.min(file.size, offset + hashChunkBytes);
    hasher.update(new Uint8Array(await file.slice(offset, end).arrayBuffer()));
    throwIfAborted(signal);
    onprogress({ processed: end, total: file.size });
  }
  throwIfAborted(signal);
  return bytesToHex(hasher.digest());
}

export function validateUploadReceipt(
  receipt: UploadReceipt,
  parentID: number,
  name: string,
  expectedHash: string,
  expectedSize: number,
): void {
  if (
    (receipt.status !== "added" && receipt.status !== "skipped") ||
    receipt.computed_hash !== expectedHash ||
    receipt.computed_size !== expectedSize ||
    receipt.node.parent_id !== parentID ||
    // New uploads may be collision-suffixed. An idempotent provenance match
    // can identify the same bytes after the existing node was renamed.
    (receipt.status === "added" && !permittedUploadName(name, receipt.node.name)) ||
    receipt.node.blob_hash !== expectedHash ||
    receipt.node.size !== expectedSize
  ) {
    throw new Error(
      `The upload receipt for ${JSON.stringify(name)} did not match the declared file authority.`,
    );
  }
}

interface UploadMessage {
  type: string;
  nonce?: string;
  proof?: string;
  request_id?: string;
  parent_id?: number;
  name?: string;
  mime_type?: string;
  expected_hash?: string;
  expected_size?: number;
  receipt?: UploadReceipt;
  status?: number;
  code?: string;
  detail?: string;
}

export interface UploadTransport {
  uploadFile(
    parentID: number,
    file: File,
    expectedHash: string,
    signal: AbortSignal,
    onprogress: (progress: TransferProgress) => void,
  ): Promise<UploadReceipt>;
}

export class UploadChannelError extends Error {
  override readonly name = "UploadChannelError";
}

type SocketFactory = (url: string) => WebSocket;

function base64URLBytes(value: string): Uint8Array {
  const padded = value.replaceAll("-", "+").replaceAll("_", "/").padEnd(
    Math.ceil(value.length / 4) * 4,
    "=",
  );
  return Uint8Array.from(atob(padded), (character) => character.charCodeAt(0));
}

function base64URL(bytes: Uint8Array): string {
  let raw = "";
  for (const byte of bytes) raw += String.fromCharCode(byte);
  return btoa(raw).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

function expectedProof(
  secret: string,
  token: string,
  nonce: string,
): string {
  const input = utf8ToBytes(`${uploadProofDomain}${token}\u0000${nonce}`);
  return base64URL(hmac(sha256, base64URLBytes(secret), input));
}

export class VerifiedUploadChannel implements UploadTransport {
  private socket: WebSocket | null = null;
  private waiter:
    | { resolve: (message: UploadMessage) => void; reject: (cause: Error) => void }
    | null = null;
  private unusable = false;
  private busy = false;

  constructor(
    private readonly session: BrowserSession,
    private readonly socketFactory: SocketFactory = (url) => new WebSocket(url),
    private readonly onfailure: () => void = () => {},
  ) {}

  async connect(): Promise<void> {
    if (this.socket || this.unusable) {
      throw new Error("The verified upload channel is unavailable. Run `docbank web` again.");
    }
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    const socket = this.socketFactory(`${scheme}//${location.host}${uploadSocketPath}`);
    this.socket = socket;
    socket.binaryType = "arraybuffer";
    socket.addEventListener("message", (event) => this.receive(event));
    socket.addEventListener("close", () => this.fail());
    socket.addEventListener("error", () => this.fail());
    await this.waitForOpen(socket);
    const nonceBytes = crypto.getRandomValues(new Uint8Array(32));
    const nonce = base64URL(nonceBytes);
    socket.send(JSON.stringify({
      type: "authenticate",
      token: this.session.token,
      nonce,
    }));
    const authenticated = await this.nextMessage(
      undefined,
      uploadHandshakeTimeoutMs,
    );
    const proof = expectedProof(
      this.session.uploadSecret,
      this.session.token,
      nonce,
    );
    if (authenticated.type !== "authenticated" || authenticated.proof !== proof) {
      this.fail();
      throw new Error(
        "The upload endpoint could not prove it is the current Docbank daemon. No file bytes were sent.",
      );
    }
  }

  close(): void {
    this.retire(false, 1000, "browser closed");
  }

  async uploadFile(
    parentID: number,
    file: File,
    expectedHash: string,
    signal: AbortSignal,
    onprogress: (progress: TransferProgress) => void,
  ): Promise<UploadReceipt> {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN || this.unusable) {
      throw this.channelError();
    }
    if (this.busy) throw new Error("Another browser upload is already active.");
    throwIfAborted(signal);
    this.busy = true;
    const requestID = crypto.randomUUID();
    const name = file.name.normalize("NFC");
    let readyForBytes = false;
    try {
      this.send({
        type: "begin",
        request_id: requestID,
        parent_id: parentID,
        name,
        mime_type: file.type || "application/octet-stream",
        expected_hash: expectedHash,
        expected_size: file.size,
      });
      const ready = await this.nextMessage(signal, uploadResponseTimeoutMs);
      this.requireMessage(ready, requestID);
      this.throwProblem(ready);
      if (ready.type !== "ready") throw this.protocolError();
      readyForBytes = true;
      onprogress({ processed: 0, total: file.size });
      for (let offset = 0; offset < file.size; offset += hashChunkBytes) {
        if (signal.aborted) {
          await this.cancelUpload(requestID);
        }
        await this.waitForWritable(signal);
        const end = Math.min(file.size, offset + hashChunkBytes);
        this.sendBinary(await file.slice(offset, end).arrayBuffer());
        onprogress({ processed: end, total: file.size });
      }
      if (signal.aborted) await this.cancelUpload(requestID);
      this.send({ type: "end", request_id: requestID });
      const terminal = await this.nextMessage(signal, uploadResponseTimeoutMs);
      this.requireMessage(terminal, requestID);
      this.throwProblem(terminal);
      if (terminal.type !== "receipt" || !terminal.receipt) {
        throw this.protocolError();
      }
      try {
        validateUploadReceipt(terminal.receipt, parentID, name, expectedHash, file.size);
      } catch (cause) {
        this.fail();
        throw new UploadChannelError(
          cause instanceof Error ? cause.message : String(cause),
        );
      }
      return terminal.receipt;
    } catch (cause) {
      if (readyForBytes && !(cause instanceof APIError) && !this.unusable) {
        this.fail();
      }
      throw cause;
    } finally {
      this.busy = false;
    }
  }

  private send(message: UploadMessage): void {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN || this.unusable) {
      throw this.channelError();
    }
    this.socket.send(JSON.stringify(message));
  }

  private sendBinary(bytes: ArrayBuffer): void {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN || this.unusable) {
      throw this.channelError();
    }
    this.socket.send(bytes);
  }

  private nextMessage(
    signal?: AbortSignal,
    timeoutMs = uploadResponseTimeoutMs,
  ): Promise<UploadMessage> {
    if (this.waiter || this.unusable) return Promise.reject(this.channelError());
    if (signal?.aborted) {
      this.fail();
      return Promise.reject(abortError());
    }
    return new Promise((resolve, reject) => {
      let timer = 0;
      const cleanup = (): void => {
        if (timer) window.clearTimeout(timer);
        signal?.removeEventListener("abort", aborted);
      };
      const settle = (
        finish: (value: UploadMessage | PromiseLike<UploadMessage>) => void,
        message: UploadMessage,
      ): void => {
        if (this.waiter !== waiter) return;
        this.waiter = null;
        cleanup();
        finish(message);
      };
      const failWait = (cause: Error): void => {
        if (this.waiter !== waiter) return;
        this.waiter = null;
        cleanup();
        reject(cause);
      };
      const aborted = (): void => {
        failWait(abortError());
        this.fail();
      };
      const waiter = {
        resolve: (message: UploadMessage) => settle(resolve, message),
        reject: failWait,
      };
      this.waiter = waiter;
      signal?.addEventListener("abort", aborted, { once: true });
      timer = window.setTimeout(() => {
        failWait(this.channelError());
        this.fail();
      }, timeoutMs);
    });
  }

  private async cancelUpload(requestID: string): Promise<never> {
    this.send({ type: "cancel", request_id: requestID });
    try {
      const canceled = await this.nextMessage(
        undefined,
        uploadHandshakeTimeoutMs,
      );
      this.requireMessage(canceled, requestID, "error");
    } catch {
      this.fail();
    }
    throw abortError();
  }

  private async waitForWritable(signal: AbortSignal): Promise<void> {
    const deadline = Date.now() + uploadResponseTimeoutMs;
    while (
      this.socket &&
      this.socket.readyState === WebSocket.OPEN &&
      this.socket.bufferedAmount > 2 * hashChunkBytes
    ) {
      if (signal.aborted) {
        this.fail();
        throw abortError();
      }
      if (Date.now() >= deadline) {
        this.fail();
        throw this.channelError();
      }
      await new Promise((resolve) => window.setTimeout(resolve, 10));
    }
    if (signal.aborted) {
      this.fail();
      throw abortError();
    }
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN || this.unusable) {
      throw this.channelError();
    }
  }

  private waitForOpen(socket: WebSocket): Promise<void> {
    return new Promise((resolve, reject) => {
      const cleanup = (): void => {
        window.clearTimeout(timer);
        socket.removeEventListener("open", opened);
        socket.removeEventListener("error", failed);
        socket.removeEventListener("close", failed);
      };
      const opened = (): void => {
        cleanup();
        resolve();
      };
      const failed = (): void => {
        cleanup();
        this.fail();
        reject(this.channelError());
      };
      const timer = window.setTimeout(failed, uploadHandshakeTimeoutMs);
      socket.addEventListener("open", opened, { once: true });
      socket.addEventListener("error", failed, { once: true });
      socket.addEventListener("close", failed, { once: true });
    });
  }

  private receive(event: MessageEvent): void {
    if (!this.waiter || typeof event.data !== "string") {
      this.fail();
      return;
    }
    let message: UploadMessage;
    try {
      message = JSON.parse(event.data) as UploadMessage;
    } catch {
      this.fail();
      return;
    }
    const waiter = this.waiter;
    waiter.resolve(message);
  }

  private requireMessage(
    message: UploadMessage,
    requestID: string,
    type?: string,
  ): void {
    if (message.request_id !== requestID || (type && message.type !== type)) {
      throw this.protocolError();
    }
  }

  private throwProblem(message: UploadMessage): void {
    if (message.type === "error") {
      throw new APIError(
        message.detail || "Browser upload failed.",
        message.status ?? 500,
        message.code ?? "",
      );
    }
  }

  private protocolError(): Error {
    this.fail();
    return new UploadChannelError(
      "The verified upload channel returned an invalid response.",
    );
  }

  private channelError(): Error {
    return new UploadChannelError(
      "The verified upload channel ended. Run `docbank web` again before selecting files.",
    );
  }

  private fail(): void {
    this.retire(true);
  }

  private retire(notify: boolean, code?: number, reason?: string): void {
    if (this.unusable) return;
    this.unusable = true;
    if (code !== undefined) this.socket?.close(code, reason);
    else this.socket?.close();
    this.socket = null;
    const waiter = this.waiter;
    waiter?.reject(this.channelError());
    if (notify) this.onfailure();
  }
}
