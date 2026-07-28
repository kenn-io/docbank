import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { APIError, type UploadReceipt } from "./api.js";

const hashChunkBytes = 1024 * 1024;

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

function decodeUploadError(xhr: XMLHttpRequest): APIError {
  let problem: { detail?: string; title?: string; code?: string } = {};
  try {
    problem = JSON.parse(xhr.responseText) as typeof problem;
  } catch {
    // The status remains useful when a proxy or transport did not return a
    // Docbank problem document.
  }
  return new APIError(
    problem.detail || problem.title || `HTTP ${xhr.status}`,
    xhr.status,
    problem.code ?? "",
  );
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
    !permittedUploadName(name, receipt.node.name) ||
    receipt.node.blob_hash !== expectedHash ||
    receipt.node.size !== expectedSize
  ) {
    throw new Error(
      `The upload receipt for ${JSON.stringify(name)} did not match the declared file authority.`,
    );
  }
}

export function uploadFile(
  session: string,
  parentID: number,
  file: File,
  expectedHash: string,
  signal: AbortSignal,
  onprogress: (progress: TransferProgress) => void,
): Promise<UploadReceipt> {
  const name = file.name.normalize("NFC");
  const form = new FormData();
  form.append("file", file, name);
  const query = new URLSearchParams({
    parent_id: String(parentID),
    name,
  });

  return new Promise((resolve, reject) => {
    throwIfAborted(signal);
    const xhr = new XMLHttpRequest();
    const abort = () => xhr.abort();
    const finish = (action: () => void) => {
      signal.removeEventListener("abort", abort);
      action();
    };

    xhr.open("POST", `/api/v1/uploads?${query.toString()}`);
    xhr.withCredentials = true;
    xhr.setRequestHeader("Accept", "application/json");
    xhr.setRequestHeader("X-Docbank-Web-Session", session);
    xhr.setRequestHeader("X-Docbank-Blob-Hash", expectedHash);
    xhr.setRequestHeader("X-Docbank-Blob-Size", String(file.size));
    xhr.upload.addEventListener("progress", (event) => {
      const processed =
        event.lengthComputable && event.total > 0
          ? Math.round((file.size * event.loaded) / event.total)
          : Math.min(file.size, event.loaded);
      onprogress({ processed: Math.min(file.size, processed), total: file.size });
    });
    xhr.addEventListener("abort", () => finish(() => reject(abortError())));
    xhr.addEventListener("error", () =>
      finish(() => reject(new Error(`Uploading ${JSON.stringify(name)} failed.`))),
    );
    xhr.addEventListener("load", () => {
      if (xhr.status < 200 || xhr.status > 299) {
        finish(() => reject(decodeUploadError(xhr)));
        return;
      }
      let receipt: UploadReceipt;
      try {
        receipt = JSON.parse(xhr.responseText) as UploadReceipt;
      } catch (cause) {
        finish(() =>
          reject(new Error(`Decoding the upload receipt failed: ${String(cause)}`)),
        );
        return;
      }
      try {
        validateUploadReceipt(receipt, parentID, name, expectedHash, file.size);
      } catch (cause) {
        finish(() => reject(cause));
        return;
      }
      onprogress({ processed: file.size, total: file.size });
      finish(() => resolve(receipt));
    });
    signal.addEventListener("abort", abort, { once: true });
    onprogress({ processed: 0, total: file.size });
    xhr.send(form);
  });
}
