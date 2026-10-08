import { listPhotoAssets, type PhotoBrowseRow } from "./generated/docbank.js";
import { localPreferenceStorage } from "./browser-storage.js";
import { APIError } from "./api-transport.js";
import { ROW_HEIGHTS, type Density } from "./photoGrid.js";
import { clearSelection, reconcileIDSelection, toggleIDSelection, type SelectionState } from "./selection.js";
import { createFacetCounts } from "./snapshots.js";
import type { Query } from "./query.js";
import { captureDateQuery, type CaptureDayFacet } from "./photoTimeline.js";

export const photoQuery: Query = { v: 1, syntax: "advanced", mode: "lexical", text: "", filters: {}, sort: { field: "capture_time", direction: "desc" } };
const densityKey = "docbank.photos.density";

export function loadDensity(): Density {
  try {
    const value = localPreferenceStorage()?.getItem(densityKey);
    if (value && Object.hasOwn(ROW_HEIGHTS, value)) return value as Density;
  } catch { /* Browsing also works when local storage is disabled. */ }
  return "comfortable";
}

export class Photos {
  view = $state<"grid" | "timeline">("grid");
  date = $state<string | undefined>();
  query = $state<Query>(photoQuery);
  timeline = $state<CaptureDayFacet | undefined>();
  timelineLoading = $state(false);
  timelineError = $state("");
  private timelineController = new AbortController();
  items = $state<PhotoBrowseRow[]>([]);
  total = $state(0);
  cursor = $state<string | undefined>();
  loading = $state(false);
  error = $state("");
  scrollTop = $state(0);
  grouping = $state<"months" | "sessions">("months");
  density = $state<Density>(loadDensity());
  selection = $state<SelectionState<string>>(clearSelection<string>());
  started = false;
  private expired = false;
  private replacement: "refresh" | "expiry" | undefined;
  private controller = new AbortController();
  private disposed = false;

  constructor(private session: string, private onauthfailure: (cause: unknown) => void) {}

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
    this.timelineController.abort();
    this.timelineLoading = false;
  }

  resume(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.view === "timeline" && !this.timeline && !this.timelineLoading && !this.timelineError) void this.loadTimeline();
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
    this.timelineController.abort();
    this.timeline = undefined;
    this.timelineError = "";
    this.timelineLoading = false;
    if (this.view === "timeline") void this.loadTimeline();
    return this.replace("refresh", preserve);
  }

  setView(view: "grid" | "timeline") {
    this.view = view;
    if (view === "timeline" && !this.timeline && !this.timelineLoading) void this.loadTimeline();
  }

  async loadTimeline() {
    if (this.disposed) return;
    this.timelineController.abort();
    const controller = this.timelineController = new AbortController();
    this.timelineLoading = true;
    this.timelineError = "";
    try {
      const page = await createFacetCounts(this.session, photoQuery, controller.signal);
      if (!controller.signal.aborted) this.timeline = page.facets[0];
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) { this.onauthfailure(cause); return; }
      if (cause instanceof APIError && ["snapshot_too_large", "snapshot_capacity", "snapshot_busy", "snapshot_unavailable"].includes(cause.code)) {
        this.timeline = { dimension: "capture_day", available: false, reason: cause.code, values: [] };
      } else this.timelineError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.timelineLoading = false;
    }
  }

  selectDate(date?: string) {
    const query = captureDateQuery(photoQuery, date);
    this.controller.abort();
    this.controller = new AbortController();
    this.query = query;
    this.date = date;
    this.items = [];
    this.total = 0;
    this.cursor = undefined;
    this.started = false;
    this.loading = false;
    this.error = "";
    this.expired = false;
    this.replacement = undefined;
    this.scrollTop = 0;
    this.clearSelection();
    return this.loadMore();
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
      const restore = preserve?.();
      this.items = [...candidate.values()];
      this.total = total;
      this.cursor = cursor;
      this.started = true;
      this.selection = reconcileIDSelection(this.selection, new Set(candidate.keys()));
      this.expired = false;
      this.replacement = undefined;
      await restore?.();
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
      this.error = signal.aborted ? "Photo refresh timed out. Retry to keep browsing." : cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  select(id: string, event: MouseEvent, orderedIDs: string[]) {
    if (event.button !== 0) return;
    if (event.shiftKey || event.ctrlKey || event.metaKey) {
      this.selection = toggleIDSelection(this.selection, orderedIDs, id, event.shiftKey || !this.selection.selectedIDs.has(id), event.shiftKey);
    } else this.selection = { selectedIDs: new Set([id]), anchorID: id };
  }

  check(id: string, checked: boolean, range: boolean, orderedIDs: string[]) {
    this.selection = toggleIDSelection(this.selection, orderedIDs, id, checked, range);
  }

  clearSelection() { this.selection = clearSelection<string>(); }
  selectLoaded() { this.selection = { selectedIDs: new Set(this.items.map(item => item.asset_id)), anchorID: undefined }; }
  dispose() { this.disposed = true; this.controller.abort(); this.timelineController.abort(); }
}
