import { sha256 } from "@noble/hashes/sha2.js";

export function randomUUID(): string {
  const browserCrypto = globalThis.crypto;
  if (typeof browserCrypto.randomUUID === "function") return browserCrypto.randomUUID();

  const bytes = browserCrypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6]! & 0x0f) | 0x40;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export async function sha256Digest(data: Uint8Array): Promise<Uint8Array> {
  const subtle = globalThis.crypto.subtle;
  if (subtle) return new Uint8Array(await subtle.digest("SHA-256", new Uint8Array(data)));
  return sha256(data);
}
