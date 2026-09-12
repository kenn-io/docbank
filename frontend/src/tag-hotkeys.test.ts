import { afterEach, describe, expect, it, vi } from "vitest";
import {
  loadTagHotkeys,
  isTagHotkeyVaultID,
  saveTagHotkeys,
  tagHotkeyStorageKey,
} from "./tag-hotkeys.js";

const vaultA = "11111111-1111-4111-8111-111111111111";
const vaultB = "22222222-2222-4222-8222-222222222222";
const tax = "33333333-3333-4333-8333-333333333333";
const reviewed = "44444444-4444-4444-8444-444444444444";

function memoryStorage() {
  const entries = new Map<string, string>();
  return {
    entries,
    getItem: (key: string) => entries.get(key) ?? null,
    setItem: (key: string, value: string) => entries.set(key, value),
  };
}

afterEach(() => vi.restoreAllMocks());

describe("tag hotkey preferences", () => {
  it("persists only versioned digit-to-tag IDs in a vault UUID namespace", () => {
    const storage = memoryStorage();
    expect(saveTagHotkeys(storage, vaultA, { "1": tax, "9": reviewed })).toBe(
      true,
    );

    expect(tagHotkeyStorageKey(vaultA)).toBe(`docbank:tag-hotkeys:v1:${vaultA}`);
    expect(storage.getItem(tagHotkeyStorageKey(vaultA))).toBe(
      JSON.stringify({ version: 1, bindings: { "1": tax, "9": reviewed } }),
    );
    expect(loadTagHotkeys(storage, vaultA)).toEqual({
      "1": tax,
      "9": reviewed,
    });
  });

  it("isolates preferences for two vaults", () => {
    const storage = memoryStorage();
    saveTagHotkeys(storage, vaultA, { "1": tax });
    saveTagHotkeys(storage, vaultB, { "1": reviewed });

    expect(loadTagHotkeys(storage, vaultA)).toEqual({ "1": tax });
    expect(loadTagHotkeys(storage, vaultB)).toEqual({ "1": reviewed });
  });

  it.each([
    ["malformed JSON", "{"],
    ["wrong version", JSON.stringify({ version: 2, bindings: { "1": tax } })],
    ["array payload", JSON.stringify({ version: 1, bindings: [] })],
    ["bad key", JSON.stringify({ version: 1, bindings: { "0": tax } })],
    ["bad tag ID", JSON.stringify({ version: 1, bindings: { "1": "tax" } })],
  ])("rejects %s", (_name, payload) => {
    const storage = memoryStorage();
    storage.setItem(tagHotkeyStorageKey(vaultA), payload);
    expect(loadTagHotkeys(storage, vaultA)).toEqual({});
  });

  it("tolerates blocked storage reads and writes", () => {
    const blocked = {
      getItem: vi.fn(() => {
        throw new DOMException("blocked", "SecurityError");
      }),
      setItem: vi.fn(() => {
        throw new DOMException("blocked", "SecurityError");
      }),
    };

    expect(loadTagHotkeys(blocked, vaultA)).toEqual({});
    expect(saveTagHotkeys(blocked, vaultA, { "1": tax })).toBe(false);
  });

  it("rejects untrusted vault namespaces and invalid values before storage", () => {
    const storage = memoryStorage();
    expect(isTagHotkeyVaultID(vaultA)).toBe(true);
    expect(isTagHotkeyVaultID("short-lived-session")).toBe(false);
    expect(loadTagHotkeys(storage, "short-lived-session")).toEqual({});
    expect(saveTagHotkeys(storage, "short-lived-session", { "1": tax })).toBe(
      false,
    );
    expect(saveTagHotkeys(storage, vaultA, { "1": "tax" })).toBe(false);
    expect(storage.entries.size).toBe(0);
  });
});
