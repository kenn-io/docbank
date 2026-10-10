import { hidePhotoAsset, unhidePhotoAsset, listPhotoAssets, trashPhotoAsset, type PhotoBrowseRow } from "./generated/docbank.js";
import { parseQuery, canonicalQuery, type Query } from "./query.js";
import type { SnapshotPage } from "./snapshots.js";
import { localPreferenceStorage } from "./browser-storage.js";
import { APIError } from "./api-transport.js";
import { ROW_HEIGHTS, type Density } from "./photoGrid.js";
import { clearSelection, reconcileIDSelection, toggleIDSelection, type SelectionState } from "./selection.js";

export const photoQuery = parseQuery('{"syntax":"simple","sort":{"field":"capture_time","direction":"desc"}}');
const photoFacets = ["camera", "lens", "year", "location", "set"] as const;
const densityKey = "docbank.photos.density";

export function loadDensity(): Density {
  try {
    const value = localPreferenceStorage()?.getItem(densityKey);
    if (value && Object.hasOwn(ROW_HEIGHTS, value)) return value as Density;
  } catch { /* Browsing also works when local storage is disabled. */ }
  return "comfortable";
}

export class Photos {
  query = $state<Query>(parseQuery(canonicalQuery(photoQuery)));
  facets = $state<SnapshotPage["facets"]>([]);
  facetsLoading = $state(false);
  facetsError = $state("");
  facetsRetryable = $state(true);
  items = $state<PhotoBrowseRow[]>([]);
  total = $state(0);
  cursor = $state<string | undefined>();
  loading = $state(false);
  trashing = $state(false);
  hiding = $state(false);
  actionError = $state("");
  trashError = $state("");
  trashTargets = $state<PhotoBrowseRow[]>([]);
  error = $state("");
  scrollTop = $state(0);
  grouping = $state<"months" | "sessions">("months");
  density = $state<Density>(loadDensity());
  selection = $state<SelectionState<string>>(clearSelection<string>());
  started = false;
  private expired = false;
  private replacement: "refresh" | "expiry" | undefined;
  private controller = new AbortController();
  private facetsController = new AbortController();
  private needsFacets = false;
  private disposed = false;

  constructor(private session: string, private onauthfailure: (cause: unknown) => void, readonly hidden = false) {}

  setQuery(query: Query) {
    if (this.disposed || this.hiding || this.trashing) return;
    this.cancelPending();
    try { this.query = parseQuery(canonicalQuery(query)); } catch (cause) { this.error = cause instanceof Error ? cause.message : String(cause); return; }
    this.items = []; this.facets = []; this.total = 0; this.cursor = undefined;
    this.facetsError = ""; this.needsFacets = false;
    this.started = false; this.error = ""; this.expired = false; this.replacement = undefined;
    this.scrollTop = 0; this.clearSelection();
    return this.loadMore();
  }

  setDensity(density: Density) {
    this.density = density;
    try { localPreferenceStorage()?.setItem(densityKey, density); } catch { /* Keep the current session's preference. */ }
  }

  async loadMore(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.disposed || this.hiding || this.trashing || this.loading || this.error || (this.started && !this.cursor)) return;
    this.loading = true;
    const controller = this.controller;
    try {
      const first = !this.started;
      const page = await listPhotoAssets({ query: this.query, hidden: this.hidden, facets: [], page_size: 250, ...(this.cursor ? { cursor: this.cursor } : {}) }, { session: this.session, signal: controller.signal });
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
      if (first && !controller.signal.aborted) { this.needsFacets = true; void this.retryFacets(); }
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && (cause.status === 401 || this.hidden && cause.status === 403)) this.onauthfailure(cause);
      this.expired = cause instanceof APIError && cause.code === "cursor_expired";
      this.error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  async setHidden(id: string, preserve?: () => (() => Promise<void>) | undefined, onhidden?: () => void, onactionerror?: (error: string) => void) {
    if (this.hiding || this.trashing || this.disposed) return;
    const members = this.resolveTargets(this.selection.selectedIDs.has(id) ? this.selection.selectedIDs : [id]);
    if (!members) {
      this.actionError = "Load and select the photos again before changing their visibility.";
      onactionerror?.(this.actionError);
      return;
    }
    this.cancelPending();
    this.hiding = true;
    this.actionError = "";
    let failure = "";
    let failures = 0;
    await this.mutateBatch(members, async member => {
      const signal = AbortSignal.timeout(60_000);
      const receipt = await (this.hidden ? unhidePhotoAsset : hidePhotoAsset)(member.asset_id, { "If-Match": String(member.revision) }, { session: this.session, signal });
      if (signal.aborted) throw signal.reason;
      return receipt;
    }, cause => {
      failures++;
      failure = cause instanceof APIError && cause.code === "hidden_not_configured" ? "Set a passcode in the Hidden view first."
        : cause instanceof APIError && cause.code === "hidden_locked" && !this.hidden ? "This photo is already hidden."
        : cause instanceof Error ? cause.message : String(cause);
      if (cause instanceof APIError && (cause.status === 401 || this.hidden && cause.status === 403)) { this.onauthfailure(cause); return true; }
      return false;
    }, () => {
      this.actionError = failures ? `${failures} photo${failures === 1 ? "" : "s"} failed: ${failure}` : "";
      onactionerror?.(this.actionError);
      onhidden?.();
    }, "hiding", preserve);
  }

  cancelPending() {
    if (this.disposed) return;
    this.controller.abort();
    this.controller = new AbortController();
    this.facetsController.abort();
    this.facetsLoading = false;
    this.loading = false;
  }

  resume(preserve?: () => (() => Promise<void>) | undefined) {
    const counts = this.needsFacets && !this.facetsError ? this.retryFacets() : undefined;
    if (this.error) return counts;
    if (this.replacement) return this.retry(preserve);
    if (!this.started) return this.loadMore(preserve);
    return counts;
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
    this.cancelPending();
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
        const page = await listPhotoAssets({ query: this.query, hidden: this.hidden, facets: [], page_size: 250, ...(cursor ? { cursor } : {}) }, { session: this.session, signal });
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
      this.facets = []; this.facetsError = "";
      this.cursor = cursor;
      this.started = true;
      this.selection = reconcileIDSelection(this.selection, new Set([...candidate.keys(), ...this.trashTargets.map(item => item.asset_id)]));
      this.expired = false;
      this.replacement = undefined;
      await restore?.();
      if (!controller.signal.aborted) { this.needsFacets = true; void this.retryFacets(); }
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && (cause.status === 401 || this.hidden && cause.status === 403)) this.onauthfailure(cause);
      this.error = signal.aborted ? "Photo refresh timed out. Retry to keep browsing." : cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  async trashSelected(preserve?: () => (() => Promise<void>) | undefined, ontrashed?: () => void) {
    if (this.trashing || this.hiding || this.disposed) return false;
    const selected = this.resolveTargets(this.selection.selectedIDs);
    if (!selected) {
      this.trashError = "Load and select the photos again before moving them to trash.";
      return false;
    }
    this.cancelPending();
    this.trashing = true;
    this.trashError = "";
    const successes = await this.mutateBatch(selected, item => {
      const options = { session: this.session, signal: AbortSignal.timeout(60_000) };
      return trashPhotoAsset(item.asset_id, { "If-Match": String(item.revision) }, options);
    }, cause => {
      if (cause instanceof APIError && (cause.status === 401 || this.hidden && cause.status === 403)) { this.onauthfailure(cause); return true; }
      this.trashError = cause instanceof Error ? cause.message : String(cause);
      return false;
    }, () => {
      ontrashed?.();
    }, "trashing", preserve);
    return successes === selected.length;
  }

  private async mutateBatch(targets: PhotoBrowseRow[], request: (item: PhotoBrowseRow) => Promise<{ id: string; revision: number }>, onerror: (cause: unknown) => boolean, oncomplete: () => void, action: "hiding" | "trashing", preserve?: () => (() => Promise<void>) | undefined) {
    let successes = 0;
    let dispatched = false;
    try {
      for (const item of targets) {
        try {
          dispatched = true;
          const receipt = await request(item);
          if (receipt.id !== item.asset_id || receipt.revision <= item.revision) throw new Error(`Photo${action === "trashing" ? " trash" : ""} response did not confirm the selected photo. Refresh and retry.`);
          this.cancelPending();
          successes++;
          const restore = preserve?.();
          this.removeTarget(item.asset_id);
          await restore?.();
        } catch (cause) {
          if (onerror(cause)) break;
        }
      }
      if (dispatched) oncomplete();
      await this.refresh(preserve);
      return successes;
    } finally { this[action] = false; }
  }

  async retryFacets() {
    if (this.disposed || !this.started || this.facetsLoading) return;
    this.facetsController.abort();
    const controller = this.facetsController = new AbortController();
    this.needsFacets = true;
    this.facetsLoading = true; this.facetsError = ""; this.facetsRetryable = true;
    const query = parseQuery(canonicalQuery(this.query));
    query.sort = { field: "capture_time", direction: "desc" };
    try {
      const page = await listPhotoAssets({ query, hidden: this.hidden, facets: [...photoFacets], page_size: 1 }, { session: this.session, signal: controller.signal });
      if (controller.signal.aborted) return;
      this.facets = (page.facets ?? []) as SnapshotPage["facets"];
      this.needsFacets = false;
      const unavailable = this.facets.filter(facet => !facet.available);
      if (unavailable.length) {
        this.facetsRetryable = unavailable.some(facet => facet.reason !== "member_budget_exceeded" && facet.reason !== "byte_budget_exceeded");
        this.facetsError = this.facetsRetryable ? "Some photo counts couldn't be loaded." : "Photo counts exceed the query's limits. Narrow your search or filters.";
      }
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && (cause.status === 401 || this.hidden && cause.status === 403)) this.onauthfailure(cause);
      this.needsFacets = false;
      this.facetsError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.facetsLoading = false;
    }
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

  private resolveTargets(ids: Iterable<string>): PhotoBrowseRow[] | undefined {
    this.pruneTrashTargets();
    const rows = new Map([...this.trashTargets, ...this.items].map(item => [item.asset_id, item]));
    const resolved = [...ids].map(id => rows.get(id));
    if (!resolved.length || resolved.some(item => !item)) return;
    const targets = resolved.filter((item): item is PhotoBrowseRow => !!item).map(item => ({ ...item }));
    this.trashTargets = [...new Map([...this.trashTargets, ...targets].map(item => [item.asset_id, item])).values()];
    return targets;
  }

  private removeTarget(id: string) {
    this.items = this.items.filter(item => item.asset_id !== id);
    this.total = Math.max(0, this.total - 1);
    this.trashTargets = this.trashTargets.filter(item => item.asset_id !== id);
    const ids = new Set(this.selection.selectedIDs);
    ids.delete(id);
    this.selection = { selectedIDs: ids, anchorID: undefined };
  }

  private pruneTrashTargets() { this.trashTargets = this.trashTargets.filter(item => this.selection.selectedIDs.has(item.asset_id)); }

  clearSelection() { this.selection = clearSelection<string>(); this.trashTargets = []; }
  selectLoaded() { this.selection = { selectedIDs: new Set(this.items.map(item => item.asset_id)), anchorID: undefined }; this.pruneTrashTargets(); }
  dispose() { this.disposed = true; this.controller.abort(); this.facetsController.abort(); }
}
