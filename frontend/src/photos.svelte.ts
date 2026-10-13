import { getNode, hidePhotoAsset, unhidePhotoAsset, preparePhotoPreview, listPhotoAssets, trashPhotoAsset, preflightPhotoRejects, movePhotoRejects, type PhotoRejectsPreflight, type PhotoPreviewSlot, type PhotoBrowseRow, type PhotoMapRow, type SavedQueryV1Schema, type PhotoAlbumMembersRequest } from "./generated/docbank.js";
import { readVerifiedPreview } from "./download.js";
import { selectedSourceFromNode } from "./selectedSource.js";
import { localPreferenceStorage } from "./browser-storage.js";
import { APIError } from "./api-transport.js";
import { randomUUID } from "./crypto.js";
import { ROW_HEIGHTS, type Density } from "./photoGrid.js";
import { clearSelection, reconcileIDSelection, toggleIDSelection, type SelectionState } from "./selection.js";
import { fetchPhotoTimeline, type SnapshotPage } from "./snapshots.js";
import { parseQuery, canonicalQuery, type Query, type QuerySort } from "./query.js";
import { captureDateQuery, type CaptureDayFacet } from "./photoTimeline.js";

export const photoQuery = parseQuery('{"syntax":"simple","sort":{"field":"capture_time","direction":"desc"}}');
export const mapPhotoLimit = 20_000;
const photoFacets = ["camera", "lens", "year", "location", "set"] as const;
// One move can audit-trash up to 1,000 files in a single writer transaction.
const photoRejectsTimeout = 300_000;
const densityKey = "docbank.photos.density";

export function loadDensity(): Density {
  try {
    const value = localPreferenceStorage()?.getItem(densityKey);
    if (value && Object.hasOwn(ROW_HEIGHTS, value)) return value as Density;
  } catch { /* Browsing also works when local storage is disabled. */ }
  return "comfortable";
}

let density = $state<Density>(loadDensity());

export interface MapView { lat: number; lng: number; zoom: number }
export interface PhotoReturn { contextID: string; assetID: string; control: "image" | "open" | "check" | "marker"; scrollTop: number }

export type PhotoRow = PhotoBrowseRow | PhotoMapRow;

type PhotoTarget = Pick<PhotoBrowseRow, "asset_id" | "revision">;

export class Photos {
  readonly contextID = randomUUID();
  viewerReturn = $state.raw<PhotoReturn>();
  view = $state<"grid" | "timeline">("grid");
  date = $state<string | undefined>();
  baseQuery = $state.raw<Query>(photoQuery);
  query = $state.raw<Query>(photoQuery);
  timeline = $state<CaptureDayFacet | undefined>();
  timelineLoading = $state(false);
  timelineError = $state("");
  private timelineController = new AbortController();
  facets = $state<SnapshotPage["facets"]>([]);
  facetsLoading = $state(false);
  facetsError = $state("");
  facetsRetryable = $state(true);
  private rows = $state.raw<PhotoRow[]>([]);
  private seenIDs: Set<string> | undefined;
  appendedFrom = $state.raw<PhotoRow[]>();
  get items() { return this.rows; }
  set items(value: PhotoRow[]) { this.appendedFrom = undefined; this.seenIDs = undefined; this.rows = value; }
  total = $state(0);
  cursor = $state<string | undefined>();
  get mapCapped() { return this.projection === "map" && this.items.length >= mapPhotoLimit && this.total > this.items.length; }
  loading = $state(false);
  trashing = $state(false);
  rejects = $state<PhotoRejectsPreflight>();
  rejectsLoading = $state(false);
  rejectsSelectionLimit = $state(0);
  rejectsError = $state("");
  rejectsSelected = $state(false);
  hiding = $state(false);
  actionError = $state("");
  trashError = $state("");
  trashTargets = $state<PhotoTarget[]>([]);
  error = $state("");
  scrollTop = $state(0);
  // Map remounts after every access check, so its position lives with the list, like scrollTop.
  mapView: MapView | undefined;
  grouping = $state<"months" | "sessions">("months");
  get density() { return density; }
  selection = $state<SelectionState<string>>(clearSelection<string>());
  allResults = $state(false);
  started = $state(false);
  private expired = false;
  private replacement: "refresh" | "expiry" | "date" | undefined;
  private replacementDate: string | undefined;
  staged: { candidate: Map<string, PhotoRow>; cursor?: string; total: number; complete: boolean } | undefined;
  private controller = new AbortController();
  private facetsController = new AbortController();
  private needsFacets = false;
  private rejectsController = new AbortController();
  private disposed = false;

  // Album views and Map show no filter counts, so those stores never request them.
  private readonly withoutCounts: boolean;

  constructor(private session: string, private onauthfailure: (cause: unknown) => void, readonly hidden = false, query: Query = photoQuery, readonly projection?: "map") { this.baseQuery = query; this.query = query; this.withoutCounts = Boolean(query.filters?.set_ids?.length) || projection === "map"; }

  setSort(sort: QuerySort) {
    if (this.disposed) return;
    // Timeline counts ignore sort, so only photo paging restarts.
    this.controller.abort();
    this.controller = new AbortController();
    this.loading = false;
    this.staged = undefined;
    this.baseQuery = { ...this.baseQuery, sort };
    this.query = { ...this.query, sort };
    this.items = []; this.total = 0; this.cursor = undefined; this.scrollTop = 0;
    this.started = false; this.expired = false; this.replacement = undefined; this.error = "";
    this.clearSelection();
    return this.loadMore();
  }

  scope(): PhotoAlbumMembersRequest {
    return this.allResults ? { query: structuredClone(this.query) } : { asset_ids: [...this.selection.selectedIDs] };
  }

  async lookup(assetID: string, signal: AbortSignal): Promise<PhotoBrowseRow> {
    const query = photoQuery;
    try {
      const page = await listPhotoAssets({ query: { ...query, filters: { ...query.filters, asset_ids: [assetID] } }, page_size: 1, hidden: this.hidden }, { session: this.session, signal });
      if (!page.items[0]) throw new APIError("This photo is no longer in this view.", 404, "not_found");
      signal.throwIfAborted();
      return page.items[0];
    } catch (cause) {
      if (!signal.aborted && cause instanceof APIError && (cause.status === 401 || (this.hidden && cause.status === 403))) this.onauthfailure(cause);
      throw cause;
    }
  }

  async detail(row: PhotoRow, signal: AbortSignal): Promise<PhotoBrowseRow> {
    return "content_version_id" in row ? row : this.lookup(row.asset_id, signal);
  }

  async reload(assetID: string, signal: AbortSignal): Promise<PhotoBrowseRow> {
    const query = this.query;
    try {
      if (query.filters.asset_ids && !query.filters.asset_ids.includes(assetID)) throw new APIError("This photo is no longer in this view.", 404, "not_found");
      const row = (await this.page({ ...query, filters: { ...query.filters, asset_ids: [assetID] } }, signal).catch(cause => {
        if (!signal.aborted) this.authFailure(cause);
        throw cause;
      })).items[0];
      if (!row) throw new APIError("This photo is no longer in this view.", 404, "not_found");
      signal.throwIfAborted();
      if (!this.disposed && this.query === query) {
        this.cancelPending(true);
        this.items = this.items.map(item => item.asset_id === assetID ? row : item);
        this.trashTargets = this.trashTargets.map(item => item.asset_id === assetID ? row : item);
        void this.resume();
      }
      return await this.detail(row, signal);
    } catch (cause) {
      if (!signal.aborted && !this.disposed && this.query === query && cause instanceof APIError && cause.status === 404) {
        this.cancelPending(true);
        if (this.items.some(item => item.asset_id === assetID) || this.trashTargets.some(item => item.asset_id === assetID)) this.removeTarget(assetID);
        void this.resume();
        return this.lookup(assetID, signal);
      }
      throw cause;
    }
  }

  private async page(query: Query, signal: AbortSignal, cursor?: string) {
    const page = await listPhotoAssets({ query, projection: this.projection, hidden: this.hidden, facets: [], page_size: this.projection ? 1000 : 250, ...(cursor ? { cursor } : {}) }, { session: this.session, signal });
    return { ...page, items: this.projection ? page.map_items ?? [] : page.items };
  }

  async preview(photo: PhotoBrowseRow, size: "fit" | "large", signal: AbortSignal): Promise<PhotoPreviewSlot> {
    if (photo.previews[size].state !== "missing") return photo.previews[size];
    try {
      const slot = await preparePhotoPreview(photo.asset_id, { content_version_id: photo.content_version_id, size, hidden: this.hidden }, { session: this.session, signal });
      signal.throwIfAborted();
      photo.previews[size] = slot;
      return slot;
    } catch (cause) {
      if (!signal.aborted && cause instanceof APIError && (cause.status === 401 || (this.hidden && cause.status === 403))) this.onauthfailure(cause);
      throw cause;
    }
  }

  async original(photo: PhotoBrowseRow, signal: AbortSignal): Promise<string> {
    const conflict = () => new APIError("The photo changed. Reload this photo before viewing its original.", 409, "photo_display_changed");
    const check = async () => {
      signal.throwIfAborted();
      const current = await this.lookup(photo.asset_id, signal).catch(cause => { if (cause instanceof APIError && cause.status === 404) throw conflict(); throw cause; });
      if (current.display_file_id !== photo.display_file_id || current.node_id !== photo.node_id || current.content_version_id !== photo.content_version_id) throw conflict();
    };
    await check();
    let objectURL: string;
    try {
      const node = await getNode(photo.node_id, { session: this.session, signal });
      if (node.current_version_id !== photo.content_version_id) throw conflict();
      const result = await readVerifiedPreview(this.session, selectedSourceFromNode(node, photo.name), node.revision, signal, () => {});
      if (result.kind !== "image") throw new Error("This original cannot be viewed as an image.");
      objectURL = result.url;
    } catch (cause) {
      if (!signal.aborted && cause instanceof APIError && (cause.status === 401 || (this.hidden && cause.status === 403))) this.onauthfailure(cause);
      throw cause;
    }
    try {
      await check();
      signal.throwIfAborted();
      return objectURL;
    } catch (cause) { URL.revokeObjectURL(objectURL); throw cause; }
  }

  // setQuery replaces the search, filter, or album scope. It clears the selected
  // date and the timeline counts, which describe the previous scope.
  setQuery(query: Query) {
    if (this.disposed || this.hiding || this.trashing) return;
    this.cancelPending();
    let next: Query;
    try { next = parseQuery(canonicalQuery(query)); } catch (cause) { this.error = cause instanceof Error ? cause.message : String(cause); return; }
    this.baseQuery = next; this.query = next; this.date = undefined;
    this.timeline = undefined; this.timelineError = "";
    this.items = []; this.facets = []; this.total = 0; this.cursor = undefined;
    this.facetsError = ""; this.needsFacets = false;
    this.started = false; this.error = ""; this.expired = false; this.replacement = undefined;
    this.scrollTop = 0; this.clearSelection();
    if (this.view === "timeline") void this.loadTimeline();
    return this.loadMore();
  }

  setDensity(value: Density) {
    density = value;
    try { localPreferenceStorage()?.setItem(densityKey, value); } catch { /* Keep the current session's preference. */ }
  }

  async loadMore(preserve?: () => (() => Promise<void>) | undefined): Promise<void> {
    if (this.disposed || this.hiding || this.loading || this.trashing || this.error) return;
    if (this.projection && this.replacement) return this.retry(preserve);
    if (this.projection === "map" && this.items.length >= mapPhotoLimit) return;
    if (this.started && !this.cursor) return;
    this.loading = true;
    const controller = this.controller;
    let signal = controller.signal;
    try {
      const first = !this.started;
      do {
        signal = this.projection ? AbortSignal.any([controller.signal, AbortSignal.timeout(60_000)]) : controller.signal;
        const page = await this.page(this.query, signal, this.cursor);
        if (signal.aborted) throw signal.reason;
        const seen = this.seenIDs ??= new Set(this.items.map(item => item.asset_id));
        const added: PhotoRow[] = [];
        let capped = false;
        for (const item of page.items) {
          if (seen.has(item.asset_id)) continue;
          if (this.projection === "map" && this.items.length + added.length >= mapPhotoLimit) { capped = true; break; }
          seen.add(item.asset_id);
          added.push(item);
        }
        const restore = preserve?.();
        const previous = this.items;
        this.rows = [...previous, ...added];
        this.appendedFrom = previous;
        if (this.allResults) this.selection = { selectedIDs: new Set(this.items.map(item => item.asset_id)), anchorID: undefined };
        if (!this.started || !this.projection) this.total = page.total;
        this.cursor = capped ? this.cursor : page.next_cursor;
        this.started = true;
        await restore?.();
      } while (this.projection && this.cursor && this.items.length < mapPhotoLimit && !controller.signal.aborted);
      if (first && !controller.signal.aborted) { this.needsFacets = true; void this.retryFacets(); }
    } catch (cause) {
      if (controller.signal.aborted) return;
      this.authFailure(cause);
      this.expired = cause instanceof APIError && (cause.code === "cursor_expired" || !!this.projection && cause.code === "invalid_photo_cursor");
      this.error = this.projection && (signal.aborted || cause instanceof Error && cause.name === "TimeoutError") ? "Photo refresh timed out. Retry to keep browsing." : cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  async setHidden(id: string, preserve?: () => (() => Promise<void>) | undefined, onhidden?: () => void | Promise<void>, onactionerror?: (error: string) => void, oninvalidate?: () => void | Promise<void>) {
    if (this.hiding || this.trashing || this.disposed) return;
    if (this.allResults && this.selection.selectedIDs.has(id)) {
      this.actionError = "Hide changes loaded photos only. Choose Select loaded photos first.";
      onactionerror?.(this.actionError);
      return;
    }
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
      if (this.authFailure(cause)) return true;
      return false;
    }, async changed => {
      this.actionError = failures ? `${failures} photo${failures === 1 ? "" : "s"} failed: ${failure}` : "";
      onactionerror?.(this.actionError);
      if (changed) await onhidden?.();
    }, "hiding", preserve, oninvalidate);
  }

  cancelPending(invalidateReplacement = false) {
    if (this.disposed) return;
    if (invalidateReplacement && this.replacement && !this.error) this.error = "Photo list changed. Retry to finish loading.";
    this.controller.abort();
    this.controller = new AbortController();
    this.facetsController.abort();
    this.facetsLoading = false;
    this.staged = undefined;
    this.loading = false;
    this.timelineController.abort();
    this.timelineLoading = false;
    this.cancelRejects();
  }

  cancelRejects() {
    this.rejectsController.abort();
    this.rejectsLoading = false;
  }

  resume(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.needsFacets && !this.facetsError) void this.retryFacets();
    if (this.view === "timeline" && !this.timeline && !this.timelineLoading && !this.timelineError) void this.loadTimeline();
    if (this.error) return;
    if (this.replacement) return this.retry(preserve);
    if (!this.started || this.projection && this.cursor) return this.loadMore(preserve);
  }

  async retry(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.replacement || this.expired) { await this.replace(this.replacement ?? "expiry", preserve, this.replacementDate, this.projection === "map"); return; }
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
    return this.replacement === "date" ? this.replace("date", undefined, this.replacementDate) : this.replace("refresh", preserve);
  }

  setView(view: "grid" | "timeline") {
    this.view = view;
    if (view === "timeline" && !this.timeline && !this.timelineLoading) void this.loadTimeline();
  }

  async loadTimeline() {
    if (this.disposed || this.hidden) return;
    this.timelineController.abort();
    const controller = this.timelineController = new AbortController();
    this.timelineLoading = true;
    this.timelineError = "";
    try {
      const page = await fetchPhotoTimeline(this.session, this.baseQuery, controller.signal);
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

  private async eligibleSelection(ids: Iterable<string>, query: Query, signal: AbortSignal) {
    query = this.projection ? query : photoQuery;
    const eligible = new Set(ids);
    const absent = this.trashTargets.filter(row => this.selection.selectedIDs.has(row.asset_id) && !eligible.has(row.asset_id));
    for (let offset = 0; offset < absent.length; offset += 64) {
      const asset_ids = absent.slice(offset, offset + 64).map(row => row.asset_id);
      const page = await this.page({ ...query, filters: { ...query.filters, asset_ids } }, AbortSignal.any([signal, AbortSignal.timeout(60_000)]));
      signal.throwIfAborted();
      for (const row of page.items) eligible.add(row.asset_id);
    }
    return eligible;
  }

  selectDate(date?: string) {
    return this.replace("date", undefined, date);
  }

  async previewRejects(selected = false) {
    if (this.disposed || this.trashing || this.hiding || this.rejectsLoading) return;
    this.rejects = undefined;
    this.rejectsError = "";
    this.rejectsSelected = selected;
    const query = $state.snapshot(this.query);
    if (selected) {
      const ids = [...this.selection.selectedIDs].sort();
      if (!ids.length || ids.length > this.rejectsSelectionLimit) {
        this.rejectsError = `Select between 1 and ${this.rejectsSelectionLimit} photos to preview selected rejects.`;
        return;
      }
      query.filters = { ...query.filters, asset_ids: ids };
    }
    this.rejectsLoading = true;
    this.rejectsController.abort();
    const controller = this.rejectsController = new AbortController();
    try {
      const result = await preflightPhotoRejects({ query, hidden: this.hidden }, { session: this.session, signal: AbortSignal.any([controller.signal, AbortSignal.timeout(photoRejectsTimeout)]) });
      if (!controller.signal.aborted) { this.rejects = result; this.rejectsSelectionLimit = result.selection_limit; }
    } catch (cause) { if (!controller.signal.aborted) this.rejectsFailure(cause); }
    finally { if (this.rejectsController === controller) this.rejectsLoading = false; }
  }

  async trashRejects(preserve?: () => (() => Promise<void>) | undefined, ontrashed?: () => void) {
    if (this.disposed || this.trashing || this.hiding || !this.rejects?.targets.length) return false;
    const targets = this.rejects.targets;
    this.cancelPending();
    this.trashing = true;
    this.rejectsError = "";
    try {
      const result = await movePhotoRejects({ hidden: this.hidden, targets }, { session: this.session, signal: AbortSignal.timeout(photoRejectsTimeout) });
      if (result.moved.length !== targets.length || result.moved.some((id, i) => id !== targets[i].asset_id)) throw new Error("Invalid rejects move response");
      await this.removeConfirmed(result.moved, preserve);
      this.rejects = undefined;
      ontrashed?.();
      await this.refresh(preserve);
      return true;
    } catch (cause) {
      this.rejects = undefined;
      this.rejectsFailure(cause);
      if (!(cause instanceof APIError) || cause.status >= 500 && cause.code !== "maintenance_busy") {
        this.rejectsError = "The move may have completed. Refresh Photos before trying again.";
        ontrashed?.();
      }
      await this.refresh(preserve);
      return false;
    } finally { this.trashing = false; }
  }

  private rejectsFailure(cause: unknown) {
    if (this.disposed) return;
    this.rejectsError = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.code === "audit_mutation_unsupported") this.rejectsError += ` The batch stayed in Docbank. Close this dialog, select up to ${this.rejectsSelectionLimit} other photos, and choose Selected photos.`;
    this.authFailure(cause);
  }

  private async replace(mode: "refresh" | "expiry" | "date", preserve?: () => (() => Promise<void>) | undefined, date?: string, resume = false) {
    if (this.disposed) return false;
    this.controller.abort();
    this.controller = new AbortController();
    this.facetsController.abort();
    this.facetsLoading = false;
    const controller = this.controller;
    let signal = controller.signal;
    this.replacement = mode;
    this.replacementDate = date;
    const query = mode === "date" ? captureDateQuery(this.baseQuery, date) : this.query;
    this.loading = true;
    this.error = "";
    const count = this.items.length;
    const tail = this.items.at(-1)?.asset_id;
    const staged = this.staged = resume && this.staged ? this.staged : { candidate: new Map<string, PhotoRow>(), cursor: undefined, total: 0, complete: false };
    const candidate = staged.candidate;
    let cursor = staged.cursor;
    let total = staged.total;
    let reachedPrefix = !count;
    try {
      while (!staged.complete) {
        signal = AbortSignal.any([controller.signal, AbortSignal.timeout(60_000)]);
        const page = await this.page(query, signal, cursor);
        if (signal.aborted) throw signal.reason;
        const reachedPreviously = reachedPrefix;
        for (const item of page.items) candidate.set(item.asset_id, item);
        reachedPrefix ||= (!!tail && candidate.has(tail)) || candidate.size >= count;
        total = page.total;
        cursor = page.next_cursor;
        staged.cursor = cursor;
        staged.total = total;
        staged.complete = !cursor || !!this.projection && candidate.size >= mapPhotoLimit || !this.projection && (mode === "date" || reachedPrefix && (mode === "refresh" || reachedPreviously));
      }
      const eligible = mode === "date" ? new Set(candidate.keys()) : await this.eligibleSelection(candidate.keys(), query, controller.signal);
      if (controller.signal.aborted) return false;
      const restore = mode === "date" ? undefined : preserve?.();
      this.items = [...candidate.values()];
      this.total = total;
      this.facets = []; this.facetsError = "";
      this.cursor = cursor;
      this.started = true;
      if (mode === "date") {
        this.query = query;
        this.date = date;
        this.scrollTop = 0;
        this.clearSelection();
      } else {
        this.selection = this.allResults ? { selectedIDs: new Set(candidate.keys()), anchorID: undefined } : reconcileIDSelection(this.selection, eligible);
        if (!this.selection.selectedIDs.size) this.allResults = false;
        this.trashTargets = this.trashTargets.filter(item => eligible.has(item.asset_id) && this.selection.selectedIDs.has(item.asset_id));
      }
      this.expired = false;
      this.replacement = undefined;
      this.staged = undefined;
      await restore?.();
      if (!controller.signal.aborted) { this.needsFacets = true; void this.retryFacets(); }
      return !controller.signal.aborted;
    } catch (cause) {
      if (controller.signal.aborted) return false;
      this.authFailure(cause);
      if (cause instanceof APIError && ["cursor_expired", "invalid_photo_cursor"].includes(cause.code)) this.staged = undefined;
      this.error = signal.aborted || cause instanceof Error && cause.name === "TimeoutError" ? "Photo refresh timed out. Retry to keep browsing." : cause instanceof Error ? cause.message : String(cause);
      return false;
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
      if (this.authFailure(cause)) return true;
      this.trashError = cause instanceof Error ? cause.message : String(cause);
      return false;
    }, changed => {
      if (changed) ontrashed?.();
    }, "trashing", preserve);
    return successes === selected.length;
  }

  private async mutateBatch(targets: PhotoTarget[], request: (item: PhotoTarget) => Promise<{ id: string; revision: number }>, onerror: (cause: unknown) => boolean, oncomplete: (changed: boolean) => void | Promise<void>, action: "hiding" | "trashing", preserve?: () => (() => Promise<void>) | undefined, onconfirmed?: () => void | Promise<void>) {
    let successes = 0;
    let uncertain = false;
    try {
      for (const item of targets) {
        try {
          const receipt = await request(item);
          if (receipt.id !== item.asset_id || receipt.revision <= item.revision) throw new Error(`Photo${action === "trashing" ? " trash" : ""} response did not confirm the selected photo. Refresh and retry.`);
          this.cancelPending();
          successes++;
          await this.removeConfirmed([item.asset_id], preserve);
          await onconfirmed?.();
        } catch (cause) {
          if (!(cause instanceof APIError) || cause.status >= 500) uncertain = true;
          if (onerror(cause)) break;
        }
      }
      await oncomplete(successes > 0 || uncertain);
      if (!this.projection || successes !== targets.length) await this.refresh(preserve);
      return successes;
    } finally { this[action] = false; if (this.projection) void this.resume(preserve); }
  }

  // reloadCounts replaces counts that a change outside this listing made stale,
  // such as an album membership change, without reloading the photos.
  reloadCounts() {
    if (this.disposed || this.withoutCounts) return;
    this.facetsController.abort();
    this.facetsLoading = false;
    this.needsFacets = true;
    return this.retryFacets();
  }

  async retryFacets() {
    if (this.disposed || !this.started || this.facetsLoading || this.withoutCounts) return;
    this.facetsController.abort();
    const controller = this.facetsController = new AbortController();
    this.needsFacets = true;
    this.facetsLoading = true; this.facetsError = ""; this.facetsRetryable = true;
    try {
      // Counts describe the whole scope, like timeline counts, so a selected date does not narrow them.
      const query = parseQuery(canonicalQuery(this.baseQuery));
      query.sort = { field: "capture_time", direction: "desc" };
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

  private resolveTargets(ids: Iterable<string>): PhotoTarget[] | undefined {
    this.pruneTrashTargets();
    const rows = new Map([...this.trashTargets, ...this.items].map(item => [item.asset_id, item]));
    const resolved = [...ids].map(id => rows.get(id));
    if (!resolved.length || resolved.some(item => !item)) return;
    const targets = resolved.filter((item): item is PhotoTarget => !!item).map(item => ({ ...item }));
    this.trashTargets = [...new Map([...this.trashTargets, ...targets].map(item => [item.asset_id, item])).values()];
    return targets;
  }

  private authFailure(cause: unknown) {
    if (!(cause instanceof APIError) || cause.status !== 401 && !(this.hidden && cause.status === 403)) return false;
    this.onauthfailure(cause);
    return true;
  }

  private async removeConfirmed(ids: string[], preserve?: () => (() => Promise<void>) | undefined) {
    const restore = preserve?.();
    for (const id of ids) this.removeTarget(id);
    await restore?.();
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

  clearSelection() { this.allResults = false; this.selection = clearSelection<string>(); this.trashTargets = []; }
  selectLoaded(ids = this.items.map(item => item.asset_id)) { this.allResults = false; this.selection = { selectedIDs: new Set(ids), anchorID: undefined }; this.pruneTrashTargets(); }
  selectAllResults() { this.selectLoaded(); this.allResults = true; }
  dispose() { this.viewerReturn = undefined; this.disposed = true; this.staged = undefined; this.controller.abort(); this.timelineController.abort(); this.facetsController.abort(); this.rejectsController.abort(); }
}
