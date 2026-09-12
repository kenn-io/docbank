export const TAG_HOTKEYS = ["1", "2", "3", "4", "5", "6", "7", "8", "9"] as const;

export type TagHotkey = (typeof TAG_HOTKEYS)[number];
export type TagHotkeyBindings = Partial<Record<TagHotkey, string>>;

type PreferenceStorage = Pick<Storage, "getItem" | "setItem">;

const UUID_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function isTagHotkeyVaultID(value: unknown): value is string {
  return typeof value === "string" && UUID_PATTERN.test(value);
}

function normalizeBindings(value: unknown): TagHotkeyBindings | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const record = value as Record<string, unknown>;
  const allowed = new Set<string>(TAG_HOTKEYS);
  if (Object.keys(record).some((key) => !allowed.has(key))) return undefined;
  const normalized: TagHotkeyBindings = {};
  for (const key of TAG_HOTKEYS) {
    const tagID = record[key];
    if (tagID === undefined) continue;
    if (!isTagHotkeyVaultID(tagID)) return undefined;
    normalized[key] = tagID;
  }
  return normalized;
}

export function tagHotkeyStorageKey(vaultID: string): string {
  return `docbank:tag-hotkeys:v1:${vaultID}`;
}

export function loadTagHotkeys(
  storage: Pick<PreferenceStorage, "getItem">,
  vaultID: string,
): TagHotkeyBindings {
  if (!isTagHotkeyVaultID(vaultID)) return {};
  try {
    const raw = storage.getItem(tagHotkeyStorageKey(vaultID));
    if (raw === null) return {};
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    const envelope = parsed as Record<string, unknown>;
    if (envelope.version !== 1 || Object.keys(envelope).some((key) => key !== "version" && key !== "bindings")) {
      return {};
    }
    return normalizeBindings(envelope.bindings) ?? {};
  } catch {
    return {};
  }
}

export function saveTagHotkeys(
  storage: Pick<PreferenceStorage, "setItem">,
  vaultID: string,
  bindings: TagHotkeyBindings,
): boolean {
  if (!isTagHotkeyVaultID(vaultID)) return false;
  const normalized = normalizeBindings(bindings);
  if (!normalized) return false;
  try {
    storage.setItem(
      tagHotkeyStorageKey(vaultID),
      JSON.stringify({ version: 1, bindings: normalized }),
    );
    return true;
  } catch {
    return false;
  }
}
