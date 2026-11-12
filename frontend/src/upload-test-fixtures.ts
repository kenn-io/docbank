import { hmac } from "@noble/hashes/hmac.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { utf8ToBytes } from "@noble/hashes/utils.js";

export function daemonProof(token: string, nonce: string): string {
  const bytes = hmac(
    sha256,
    new Uint8Array(32).fill(1),
    utf8ToBytes(`docbank-web-upload-v1\u0000${token}\u0000${nonce}`),
  );
  return btoa(String.fromCharCode(...bytes))
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replace(/=+$/, "");
}

export class DaemonSocket extends EventTarget {
  static OPEN = 1;
  static CLOSED = 3;
  readyState: number = DaemonSocket.OPEN;
  token = "browser-session";
  proofValid = true;
  rejectConnection = false;
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
  // Indexes the daemon asks to have hashed before it plans, once per socket.
  needHashes: number[] = [];
  // Further requests the daemon makes after those hashes arrive, one per declaration.
  followUps: number[][] = [];
  frames: (string | ArrayBuffer)[] = [];

  constructor() {
    super();
    queueMicrotask(() => this.dispatchEvent(new Event(this.rejectConnection ? "close" : "open")));
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
      files?: { expected_hash: string }[];
    };
    if (message.type === "authenticate" && this.authenticate) {
      this.emit({
        type: "authenticated",
        proof: this.proofValid ? daemonProof(this.token, message.nonce ?? "") : "bad",
      });
    } else if (message.type === "plan_photo_import") {
      if (this.needHashes.length && !message.files?.[this.needHashes[0]!]?.expected_hash) {
        this.emit({
          type: "photo_import_hashes_needed",
          request_id: message.request_id,
          indexes: this.needHashes,
        });
        return;
      }
      const followUp = this.followUps.shift();
      if (followUp) {
        this.emit({
          type: "photo_import_hashes_needed",
          request_id: message.request_id,
          indexes: followUp,
        });
        return;
      }
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
    this.readyState = DaemonSocket.CLOSED;
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
