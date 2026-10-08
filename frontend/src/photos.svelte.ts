import { hidePhotoAsset, unhidePhotoAsset, listPhotoAssets, trashPhotoAsset, type PhotoBrowseRow, type SavedQueryV1Schema } from "./generated/docbank.js";
import { localPreferenceStorage } from "./browser-storage.js";
import { APIError } from "./api-transport.js";
import { ROW_HEIGHTS, type Density } from "./photoGrid.js";
import { clearSelection, reconcileIDSelection, toggleIDSelection, type SelectionState } from "./selection.js";

export const photoQuery: SavedQueryV1Schema = { v: 1, syntax: "advanced", mode: "lexical", text: "", sort: { field: "capture_time", direction: "desc" } };
const densityKey = "docbank.photos.density";

export function loadDensity(): Density {
  try {
    const value = localPreferenceStorage()?.getItem(densityKey);
    if (value && Object.hasOwn(ROW_HEIGHTS, value)) return value as Density;
  } catch { /* Browsing also works when local storage is disabled. */ }
  return "comfortable";
}

export class Photos {
  items = $state<PhotoBrowseRow[]>([]);
  total = $state(0);
  cursor = $state<string | undefined>();
  loading = $state(false);
  trashing = $state(false);
  trashError = $state("");
  trashTargets = $state<PhotoBrowseRow[]>([]);
  error = $state("");
  scrollTop = $state(0);
  grouping = $state<"months" | "sessions">("months");
  density = $state<Density>(loadDensity());
  selection = $state<SelectionState<string>>(clearSelection<string>());
  started = false;
  private expired = false;
  private privacyCount = 0;
  private replacement: "refresh" | "expiry" | undefined;
  private controller = new AbortController();
  private disposed = false;

  constructor(private session: string, private onauthfailure: (cause: unknown) => void, readonly hidden = false) {}

  setDensity(density: Density) {
    this.density = density;
    try { localPreferenceStorage()?.setItem(densityKey, density); } catch { /* Keep the current session's preference. */ }
  }

  async loadMore(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.disposed || this.loading || this.error || (this.started && !this.cursor)) return;
    this.loading = true;
    const controller = this.controller;
    try {
      const page = await listPhotoAssets({ query: photoQuery, hidden: this.hidden, page_size: 250, ...(this.cursor ? { cursor: this.cursor } : {}) }, { session: this.session, signal: controller.signal });
      if (controller.signal.aborted) return;
      const seen = new Set(this.items.map(item => item.asset_id));
      const restore = preserve?.();
      this.items = [...this.items, ...page.items.filter(item => {
        if (seen.has(item.asset_id)) return false;
        seen.add(item.asset_id);
        return true;
      })];
      this.total = page.total;
      this.cursor = page.next_cursor;
      this.started = true;
      await restore?.();
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && (cause.status === 401 || this.hidden && cause.status === 403)) this.onauthfailure(cause);
      this.expired = cause instanceof APIError && cause.code === "cursor_expired";
      this.error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  clearForPrivacy(preserve = false) {
    const selection = this.selection;
    if (preserve) this.privacyCount = Math.max(this.privacyCount, this.items.length);
    this.cancelPending();
    this.items = [];
    this.total = 0;
    this.cursor = undefined;
    this.started = false;
    if (preserve) { this.selection = selection; this.replacement = "refresh"; } else this.clearSelection();
    this.error = "";
  }

  async setHidden(id: string) {
    const members = this.selection.selectedIDs.has(id) ? this.items.filter(item => this.selection.selectedIDs.has(item.asset_id)) : this.items.filter(item => item.asset_id === id);
    let failure = "";
    for (const member of members) {
      try {
        await (this.hidden ? unhidePhotoAsset : hidePhotoAsset)(member.asset_id, { "If-Match": JSON.stringify(String(member.revision)) }, { session: this.session });
        this.items = this.items.filter(item => item.asset_id !== member.asset_id);
        this.selection = reconcileIDSelection(this.selection, new Set(this.items.map(item => item.asset_id)));
      } catch (cause) { failure = cause instanceof Error ? cause.message : String(cause); }
    }
    notifyPhotoPrivacy(failure);
    this.error = failure;
  }

  cancelPending() {
    if (this.disposed) return;
    this.controller.abort();
    this.controller = new AbortController();
    this.loading = false;
  }

  resume(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.error) return;
    if (this.replacement) return this.retry(preserve);
    if (!this.started) return this.loadMore(preserve);
  }

  retry(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.replacement || this.expired) return this.replace(this.replacement ?? "expiry", preserve);
    this.error = "";
    this.expired = false;
    return this.loadMore(preserve);
  }

  refresh(preserve?: () => (() => Promise<void>) | undefined) {
    return this.replace("refresh", preserve);
  }

  private async replace(mode: "refresh" | "expiry", preserve?: () => (() => Promise<void>) | undefined) {
    if (this.disposed) return;
    this.controller.abort();
    this.controller = new AbortController();
    const controller = this.controller;
    let signal = controller.signal;
    this.replacement = mode;
    this.loading = true;
    this.error = "";
    const count = this.privacyCount || this.items.length;
    const tail = this.items.at(-1)?.asset_id;
    const candidate = new Map<string, PhotoBrowseRow>();
    let cursor: string | undefined;
    let total = 0;
    let reachedPrefix = !count;
    try {
      do {
        signal = AbortSignal.any([controller.signal, AbortSignal.timeout(60_000)]);
        const page = await listPhotoAssets({ query: photoQuery, hidden: this.hidden, page_size: 250, ...(cursor ? { cursor } : {}) }, { session: this.session, signal });
        if (signal.aborted) throw signal.reason;
        const reachedPreviously = reachedPrefix;
        for (const item of page.items) candidate.set(item.asset_id, item);
        reachedPrefix ||= (!!tail && candidate.has(tail)) || candidate.size >= count;
        total = page.total;
        cursor = page.next_cursor;
        if (reachedPrefix && (mode === "refresh" || reachedPreviously)) break;
      } while (cursor);
      if (signal.aborted) throw signal.reason;
      const restore = preserve?.();
      this.items = [...candidate.values()];
      this.total = total;
      this.cursor = cursor;
      this.started = true;
      this.privacyCount = 0;
      this.selection = reconcileIDSelection(this.selection, new Set([...candidate.keys(), ...this.trashTargets.map(item => item.asset_id)]));
      this.expired = false;
      this.replacement = undefined;
      await restore?.();
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && (cause.status === 401 || this.hidden && cause.status === 403)) this.onauthfailure(cause);
      this.error = signal.aborted ? "Photo refresh timed out. Retry to keep browsing." : cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  async trashSelected(preserve?: () => (() => Promise<void>) | undefined, ontrashed?: () => void) {
    if (this.trashing || this.disposed) return false;
    this.pruneTrashTargets();
    const rows = new Map([...this.trashTargets, ...this.items].map(item => [item.asset_id, item]));
    const resolved = [...this.selection.selectedIDs].map(id => rows.get(id));
    if (!resolved.length || resolved.some(item => !item)) {
      this.trashError = "Load and select the photos again before moving them to trash.";
      return false;
    }
    const selected = resolved.filter((item): item is PhotoBrowseRow => !!item).map(item => ({ ...item }));
    this.trashTargets = selected;
    this.trashing = true;
    this.trashError = "";
    let successes = 0;
    try {
      for (const item of selected) {
        try {
          const options = { session: this.session, signal: AbortSignal.timeout(60_000) };
          const revision = item.revision;
          const receipt = await trashPhotoAsset(item.asset_id, { "If-Match": String(revision) }, options);
          if (receipt.id !== item.asset_id || receipt.revision <= revision) throw new Error("Photo trash response did not confirm the selected photo. Refresh and retry.");
          successes++;
          const restore = preserve?.();
          this.items = this.items.filter(row => row.asset_id !== item.asset_id);
          this.total = Math.max(0, this.total - 1);
          this.trashTargets = this.trashTargets.filter(target => target.asset_id !== item.asset_id);
          const ids = new Set(this.selection.selectedIDs);
          ids.delete(item.asset_id);
          this.selection = { selectedIDs: ids, anchorID: undefined };
          await restore?.();
        } catch (cause) {
          if (cause instanceof APIError && cause.status === 401) { this.onauthfailure(cause); break; }
          this.trashError = cause instanceof Error ? cause.message : String(cause);
        }
      }
      if (successes) ontrashed?.();
      await this.refresh(preserve);
      return successes === selected.length;
    } finally { this.trashing = false; }
  }

  select(id: string, event: MouseEvent, orderedIDs: string[]) {
    if (event.button !== 0) return;
    if (event.shiftKey || event.ctrlKey || event.metaKey) {
      this.selection = toggleIDSelection(this.selection, orderedIDs, id, event.shiftKey || !this.selection.selectedIDs.has(id), event.shiftKey);
    } else this.selection = { selectedIDs: new Set([id]), anchorID: id };
    this.pruneTrashTargets();
  }

  check(id: string, checked: boolean, range: boolean, orderedIDs: string[]) {
    this.selection = toggleIDSelection(this.selection, orderedIDs, id, checked, range);
    this.pruneTrashTargets();
  }

  private pruneTrashTargets() { this.trashTargets = this.trashTargets.filter(item => this.selection.selectedIDs.has(item.asset_id)); }

  clearSelection() { this.selection = clearSelection<string>(); this.trashTargets = []; }
  selectLoaded() { this.selection = { selectedIDs: new Set(this.items.map(item => item.asset_id)), anchorID: undefined }; this.pruneTrashTargets(); }
  dispose() { this.disposed = true; this.controller.abort(); }
}

export const photoPrivacyEvent = "docbank-photo-privacy";
export function notifyPhotoPrivacy(error = "") {
  window.dispatchEvent(new CustomEvent(photoPrivacyEvent, { detail: error }));
  if (typeof BroadcastChannel !== "undefined") {
    const channel = new BroadcastChannel(photoPrivacyEvent);
    channel.postMessage("changed");
    channel.close();
  }
}
