import { listPhotoAssets, trashPhotoAsset, type PhotoBrowseRow, type SavedQueryV1Schema, type PhotoAlbumMembersRequest } from "./generated/docbank.js";
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
  allResults = $state(false);
  query = $state.raw<SavedQueryV1Schema>(photoQuery);
  started = false;
  private expired = false;
  private replacement: "refresh" | "expiry" | undefined;
  private controller = new AbortController();
  private disposed = false;

  constructor(private session: string, private onauthfailure: (cause: unknown) => void, query: SavedQueryV1Schema = photoQuery) { this.query = query; }

  setQuery(query: SavedQueryV1Schema) {
    this.cancelPending();
    this.query = query;
    this.items = []; this.total = 0; this.cursor = undefined; this.scrollTop = 0;
    this.started = false; this.expired = false; this.replacement = undefined; this.error = "";
    this.clearSelection();
    return this.loadMore();
  }

  scope(): PhotoAlbumMembersRequest {
    return this.allResults ? { query: structuredClone(this.query) } : { asset_ids: [...this.selection.selectedIDs] };
  }

  setDensity(density: Density) {
    this.density = density;
    try { localPreferenceStorage()?.setItem(densityKey, density); } catch { /* Keep the current session's preference. */ }
  }

  async loadMore(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.disposed || this.loading || this.error || (this.started && !this.cursor)) return;
    this.loading = true;
    const controller = this.controller;
    try {
      const page = await listPhotoAssets({ query: this.query, page_size: 250, ...(this.cursor ? { cursor: this.cursor } : {}) }, { session: this.session, signal: controller.signal });
      if (controller.signal.aborted) return;
      const seen = new Set(this.items.map(item => item.asset_id));
      const restore = preserve?.();
      this.items = [...this.items, ...page.items.filter(item => {
        if (seen.has(item.asset_id)) return false;
        seen.add(item.asset_id);
        return true;
      })];
      if (this.allResults) this.selection = { selectedIDs: new Set(this.items.map(item => item.asset_id)), anchorID: undefined };
      this.total = page.total;
      this.cursor = page.next_cursor;
      this.started = true;
      await restore?.();
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
      this.expired = cause instanceof APIError && cause.code === "cursor_expired";
      this.error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  cancelPending() {
    if (this.disposed) return;
    this.controller.abort();
    this.controller = new AbortController();
    this.loading = false;
  }

  syncDensity() { this.density = loadDensity(); }

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
    const count = this.items.length;
    const tail = this.items.at(-1)?.asset_id;
    const candidate = new Map<string, PhotoBrowseRow>();
    let cursor: string | undefined;
    let total = 0;
    let reachedPrefix = !count;
    try {
      do {
        signal = AbortSignal.any([controller.signal, AbortSignal.timeout(60_000)]);
        const page = await listPhotoAssets({ query: this.query, page_size: 250, ...(cursor ? { cursor } : {}) }, { session: this.session, signal });
        if (signal.aborted) throw signal.reason;
        const reachedPreviously = reachedPrefix;
        for (const item of page.items) candidate.set(item.asset_id, item);
        reachedPrefix ||= (!!tail && candidate.has(tail)) || candidate.size >= count;
        total = page.total;
        cursor = page.next_cursor;
        if (reachedPrefix && (mode === "refresh" || reachedPreviously)) break;
      } while (cursor);
      if (signal.aborted) throw signal.reason;
      const eligible = new Set(candidate.keys());
      const checked = new Set(candidate.keys());
      const filter = this.query.filters?.asset_ids;
      while (!this.allResults && cursor) {
        const retained = new Set(this.selection.selectedIDs);
        if (this.selection.anchorID !== undefined) retained.add(this.selection.anchorID);
        const missing = [...retained].filter(id => !checked.has(id));
        if (!missing.length) break;
        for (let index = 0; index < missing.length; index += 256) {
          await Promise.all(Array.from({ length: Math.min(4, Math.ceil((missing.length - index) / 64)) }, async (_, batch) => {
            const group = missing.slice(index + batch * 64, index + (batch + 1) * 64);
            for (const id of group) checked.add(id);
            const ids = group.filter(id => !filter || filter.includes(id));
            if (!ids.length) return;
            const batchSignal = AbortSignal.any([controller.signal, AbortSignal.timeout(60_000)]);
            const query = { ...this.query, filters: { ...this.query.filters, asset_ids: ids } };
            const page = await listPhotoAssets({ query, page_size: 250 }, { session: this.session, signal: batchSignal });
            if (batchSignal.aborted) throw batchSignal.reason;
            for (const item of page.items) eligible.add(item.asset_id);
          }));
        }
      }
      if (controller.signal.aborted) return;
      const restore = preserve?.();
      this.items = [...candidate.values()];
      this.total = total;
      this.cursor = cursor;
      this.started = true;
      this.selection = this.allResults ? { selectedIDs: new Set(candidate.keys()), anchorID: undefined } : reconcileIDSelection(this.selection, new Set([...eligible, ...this.trashTargets.map(item => item.asset_id)]));
      if (!this.selection.selectedIDs.size) this.allResults = false;
      this.expired = false;
      this.replacement = undefined;
      await restore?.();
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
      this.error = signal.aborted || cause instanceof DOMException && cause.name === "TimeoutError" ? "Photo refresh timed out. Retry to keep browsing." : cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  async trashSelected(preserve?: () => (() => Promise<void>) | undefined, ontrashed?: () => void) {
    if (this.trashing || this.disposed) return false;
    this.pruneTrashTargets();
    this.trashing = true;
    try {
      const rows = new Map([...this.trashTargets, ...this.items].map(item => [item.asset_id, item]));
      // A refresh keeps verified selections that moved past the loaded pages; fetch their current rows.
      const missing = [...this.selection.selectedIDs].filter(id => !rows.has(id));
      try {
        for (let index = 0; index < missing.length; index += 64) {
          const query = { ...this.query, filters: { ...this.query.filters, asset_ids: missing.slice(index, index + 64) } };
          const page = await listPhotoAssets({ query, page_size: 250 }, { session: this.session, signal: AbortSignal.timeout(60_000) });
          for (const item of page.items) rows.set(item.asset_id, item);
        }
      } catch (cause) { if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause); }
      const resolved = [...this.selection.selectedIDs].map(id => rows.get(id));
      if (!resolved.length || resolved.some(item => !item)) {
        this.trashError = "Load and select the photos again before moving them to trash.";
        return false;
      }
      const selected = resolved.filter((item): item is PhotoBrowseRow => !!item).map(item => ({ ...item }));
      this.trashTargets = selected;
      this.trashError = "";
      let successes = 0;
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
    this.allResults = false;
    if (event.shiftKey || event.ctrlKey || event.metaKey) {
      this.selection = toggleIDSelection(this.selection, orderedIDs, id, event.shiftKey || !this.selection.selectedIDs.has(id), event.shiftKey);
    } else this.selection = { selectedIDs: new Set([id]), anchorID: id };
    this.pruneTrashTargets();
  }

  check(id: string, checked: boolean, range: boolean, orderedIDs: string[]) {
    this.allResults = false;
    this.selection = toggleIDSelection(this.selection, orderedIDs, id, checked, range);
    this.pruneTrashTargets();
  }

  private pruneTrashTargets() { this.trashTargets = this.trashTargets.filter(item => this.selection.selectedIDs.has(item.asset_id)); }

  clearSelection() { this.allResults = false; this.selection = clearSelection<string>(); this.trashTargets = []; }
  selectLoaded() { this.allResults = false; this.selection = { selectedIDs: new Set(this.items.map(item => item.asset_id)), anchorID: undefined }; this.pruneTrashTargets(); }
  selectAllResults() { this.selectLoaded(); this.allResults = true; }
  dispose() { this.disposed = true; this.controller.abort(); }
}
