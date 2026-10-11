import { afterEach, describe, expect, it, vi } from "vitest";
import { randomUUID, sha256Digest } from "./crypto.js";

afterEach(() => vi.unstubAllGlobals());

describe("browser crypto fallbacks", () => {
  it("creates a v4 UUID with getRandomValues when randomUUID is unavailable", () => {
    const getRandomValues = vi.fn((bytes: Uint8Array) => {
      bytes.fill(0);
      return bytes;
    });
    vi.stubGlobal("crypto", { getRandomValues });

    expect(randomUUID()).toBe("00000000-0000-4000-8000-000000000000");
    expect(getRandomValues).toHaveBeenCalledTimes(1);
  });

  it("computes SHA-256 without SubtleCrypto", async () => {
    vi.stubGlobal("crypto", { getRandomValues: vi.fn() });

    const digest = await sha256Digest(new TextEncoder().encode("abc"));
    const hex = Array.from(digest, byte => byte.toString(16).padStart(2, "0")).join("");

    expect(hex).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  });
});
