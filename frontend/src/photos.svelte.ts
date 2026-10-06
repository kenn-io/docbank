import { listPhotoAssets, type PhotoBrowseRow, type SavedQueryV1Schema } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { ROW_HEIGHTS, type Density } from "./photoGrid.js";
import { toggleIDSelection, type SelectionState } from "./selection.js";

export const photoQuery: SavedQueryV1Schema = { v: 1, syntax: "advanced", mode: "lexical", text: "", sort: { field: "capture_time", direction: "desc" } };
const densityKey = "docbank.photos.density";

export function loadDensity(): Density {
  try {
    const value = localStorage.getItem(densityKey);
    if (value && Object.hasOwn(ROW_HEIGHTS, value)) return value as Density;
  } catch { /* Browsing also works when local storage is disabled. */ }
  return "comfortable";
}

export class Photos {
  items = $state<PhotoBrowseRow[]>([]);
  total = $state(0);
  cursor = $state<string | undefined>();
  loading = $state(false);
  error = $state("");
  grouping = $state<"months" | "sessions">("months");
  density = $state<Density>(loadDensity());
  selection = $state<SelectionState<string>>({ selectedIDs: new Set(), anchorID: undefined });
  private started = false;
  private expired = false;
  private controller = new AbortController();
  private disposed = false;

  constructor(private session: string, private onauthfailure: (cause: unknown) => void) {}

  setDensity(density: Density) {
    this.density = density;
    try { localStorage.setItem(densityKey, density); } catch { /* Keep the current session's preference. */ }
  }

  async loadMore() {
    if (this.disposed || this.loading || this.error || (this.started && !this.cursor)) return;
    this.loading = true;
    const controller = this.controller;
    try {
      const page = await listPhotoAssets({ query: photoQuery, page_size: 250, ...(this.cursor ? { cursor: this.cursor } : {}) }, { session: this.session, signal: controller.signal });
      if (controller.signal.aborted) return;
      this.items = [...this.items, ...page.items];
      this.total = page.total;
      this.cursor = page.next_cursor;
      this.started = true;
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
      this.expired = cause instanceof APIError && cause.code === "cursor_expired";
      this.error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  retry() {
    if (this.expired) {
      this.items = [];
      this.cursor = undefined;
      this.started = false;
      this.clearSelection();
    }
    this.error = "";
    this.expired = false;
    return this.loadMore();
  }

  refresh() {
    this.controller.abort();
    this.controller = new AbortController();
    this.items = [];
    this.cursor = undefined;
    this.started = false;
    this.loading = false;
    this.error = "";
    this.expired = false;
    this.clearSelection();
    return this.loadMore();
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

  clearSelection() { this.selection = { selectedIDs: new Set(), anchorID: undefined }; }
  selectLoaded() { this.selection = { selectedIDs: new Set(this.items.map(item => item.asset_id)), anchorID: undefined }; }
  dispose() { this.disposed = true; this.controller.abort(); }
}
