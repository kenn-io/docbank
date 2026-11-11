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
