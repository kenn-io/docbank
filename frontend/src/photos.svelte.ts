import { patchPhotoRows, photoRows, type PhotoBrowseRow } from "./photoRows.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import { photoSearchSort } from "./photoSearch.js";
import { getSavedQuery, type SavedQuery } from "./savedQueries.js";
import {
  getPhotoSettings,
  setPhotoSettings,
  getNode,
  hidePhotoAsset,
  unhidePhotoAsset,
  preparePhotoPreview,
  listPhotoAssets,
  trashPhotoAsset,
  preflightPhotoRejects,
  movePhotoRejects,
  type PhotoRejectsPreflight,
  type PhotoAuthoredPatch,
  type PhotoAuthoredReceipt,
  type PhotoPreviewSlot,
  type PhotoMapRow,
  type SavedQueryV1Schema,
  type PhotoAlbumMembersRequest,
  type SavedQueryRunResult,
} from "./generated/docbank.js";
import { readVerifiedPreview } from "./download.js";
import { selectedSourceFromNode } from "./selectedSource.js";
import { localPreferenceStorage } from "./browser-storage.js";
import { APIError } from "./api-transport.js";
import { randomUUID } from "./crypto.js";
import { ROW_HEIGHTS, type Density } from "./photoGrid.js";
import {
  clearSelection,
  reconcileIDSelection,
  toggleIDSelection,
  type SelectionState,
} from "./selection.js";
import { fetchPhotoTimeline, type SnapshotPage } from "./snapshots.js";
import {
  parseQuery,
  canonicalQuery,
  type Query,
  type QueryFilters,
  type QuerySort,
} from "./query.js";
import { captureDateQuery, type CaptureDayFacet } from "./photoTimeline.js";
import { PhotoAuthoredEditor, type PhotoUndoState } from "./photoAuthored.svelte.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";
import type { PhotoCull } from "./photoCull.svelte.js";
import { fittedPhotoView, type PhotoView } from "./photoView.js";

export const maxComparePhotos = 8;
export const photoQuery = parseQuery(
  '{"syntax":"simple","sort":{"field":"capture_time","direction":"desc"}}',
);
// QueryV1 accepts at most 64 asset IDs per request.
export function lookupPhotoRows(
  session: string,
  ids: string[],
  signal?: AbortSignal,
  onprogress?: (loaded: number, total: number) => void,
  hidden?: boolean,
): Promise<PhotoBrowseRow[]>;
export function lookupPhotoRows<Row extends { asset_id: string }>(
  session: string,
  ids: string[],
  signal: AbortSignal | undefined,
  onprogress: ((loaded: number, total: number) => void) | undefined,
  hidden: boolean,
  read: (batch: string[], signal: AbortSignal) => Promise<{ items: Row[] }>,
): Promise<Row[]>;
export function lookupPhotoRows<Row extends { asset_id: string }>(
  session: string,
  ids: string[],
  signal: AbortSignal | undefined,
  onprogress: ((loaded: number, total: number) => void) | undefined,
  hidden: boolean,
  read: undefined,
  project: (row: PhotoBrowseRow) => Row,
): Promise<Row[]>;
export async function lookupPhotoRows(
  session: string,
  ids: string[],
  signal?: AbortSignal,
  onprogress?: (loaded: number, total: number) => void,
  hidden = false,
  read?: (batch: string[], signal: AbortSignal) => Promise<{ items: { asset_id: string }[] }>,
  project?: (row: PhotoBrowseRow) => { asset_id: string },
): Promise<{ asset_id: string }[]> {
  const controller = new AbortController();
  signal = signal ? AbortSignal.any([signal, controller.signal]) : controller.signal;
  const rows: { asset_id: string }[] = [];
  let offset = 0,
    loaded = 0;
  try {
    await Promise.all(
      Array.from({ length: Math.min(4, Math.ceil(ids.length / 64)) }, async () => {
        while (offset < ids.length) {
          signal?.throwIfAborted();
          const batch = ids.slice(offset, (offset += 64));
          let items: { asset_id: string }[];
          if (read) items = (await read(batch, signal!)).items;
          else {
            const page = await listPhotoAssets(
              {
                query: { ...photoQuery, filters: { ...photoQuery.filters, asset_ids: batch } },
                page_size: 250,
                hidden,
              },
              { session, signal },
            );
            items = project ? page.items.map(project) : page.items;
          }
          signal?.throwIfAborted();
          rows.push(...items);
          loaded += batch.length;
          onprogress?.(loaded, ids.length);
        }
      }),
    );
  } catch (cause) {
    controller.abort();
    throw cause;
  }
  const byID = new Map(rows.map((row) => [row.asset_id, row]));
  return ids.flatMap((id) => (byID.has(id) ? [byID.get(id)!] : []));
}
export const mapPhotoLimit = 20_000;
const mapPageLimit = 20;
const photoFacets = [
  "camera",
  "lens",
  "year",
  "location",
  "set",
  "rating",
  "flag",
  "label",
] as const;
// One move can audit-trash up to 1,000 files in a single writer transaction.
const photoRejectsTimeout = 300_000;
const densityKey = "docbank.photos.density";
const smartSetStorageKey = (session: string) =>
  `docbank.photos.smart-set.${bytesToHex(sha256(utf8ToBytes(session)))}`;
// Search text and these filters can change the listed photos after metadata edits.
const authoredFilters = [
  "rating_min",
  "rating_max",
  "flags",
  "labels",
  "tag_ids",
  "exclude_tag_ids",
  "no_tags",
  "modified_after",
  "modified_before",
] as const satisfies readonly (keyof QueryFilters)[];

export function loadDensity(): Density {
  try {
    const value = localPreferenceStorage()?.getItem(densityKey);
    if (value && Object.hasOwn(ROW_HEIGHTS, value)) return value as Density;
  } catch {
    /* Browsing also works when local storage is disabled. */
  }
  return "comfortable";
}

let density = $state<Density>(loadDensity());

export interface MapView {
  lat: number;
  lng: number;
  zoom: number;
}
export interface PhotoReturn {
  contextID: string;
  assetID: string;
  control: "image" | "open" | "check" | "marker";
  scrollTop: number;
}

export type PhotoRow = PhotoBrowseRow | PhotoMapRow;

type PhotoTarget = Pick<PhotoBrowseRow, "asset_id" | "revision">;
interface PhotosOptions {
  projection?: "map" | undefined;
  undoState?: PhotoUndoState | undefined;
  hiddenAccess?: HiddenAccess | undefined;
}

export class Photos {
  readonly authored: PhotoAuthoredEditor;
  cullEditor = $state.raw<PhotoAuthoredEditor>();
  cull?: PhotoCull;
  cullVaultID = "";
  culling = $state(false);
  comparing = $state(false);
  compareIDs = $state.raw<string[]>([]);
  compareView = $state<PhotoView>({ ...fittedPhotoView });
  compareCrop = $state<PhotoView>();
  editScopeRevision = $state(0);
  private changeEditScope() {
    if (!this.authored.canChangeTarget()) return false;
    this.editScopeRevision++;
    this.authored.reset();
    return true;
  }
  readonly contextID = randomUUID();
  viewerReturn = $state.raw<PhotoReturn>();
  view = $state<"grid" | "timeline">("grid");
  date = $state<string | undefined>();
  smartSet = $state.raw<SavedQuery>();
  frozen = $state.raw<SavedQueryRunResult>();
  frozenExpired = $state(false);
  private frozenIdentity = "";
  private frozenRevision = "";
  private frozenTimer: ReturnType<typeof setTimeout> | undefined;
  private smartSetRecoveryGeneration = 0;

  get savedPhotoQuery() {
    return parseQuery(
      canonicalQuery({
        ...this.query,
        filters: {
          ...this.query.filters,
          kinds: this.query.filters.kinds?.length ? this.query.filters.kinds : ["photo", "video"],
        },
        sort: ["capture_time", "import_time", "added_time", "relevance"].includes(
          this.query.sort.field,
        )
          ? { field: "name", direction: "asc" }
          : this.query.sort,
      }),
    );
  }
  private smartIdentity(value = this.savedPhotoQuery) {
    return canonicalQuery({ ...value, sort: { field: "name", direction: "asc" } });
  }
  get matchingSmartSet() {
    return this.smartSet?.kind === "query" &&
      this.smartIdentity(this.smartSet.payload) === this.smartIdentity()
      ? this.smartSet
      : undefined;
  }
  rememberSmartSet(saved: SavedQuery) {
    this.smartSetRecoveryGeneration++;
    this.smartSet = saved;
    try {
      localPreferenceStorage()?.setItem(smartSetStorageKey(this.session), saved.id);
    } catch {
      /* Keep the current view usable without storage. */
    }
  }
  async recoverSmartSet() {
    if (this.hidden || this.withoutCounts) return;
    const storageKey = smartSetStorageKey(this.session);
    let id = this.smartSet?.id;
    try {
      id ??= localPreferenceStorage()?.getItem(storageKey) ?? undefined;
    } catch {
      return;
    }
    if (!id) return;
    const recovering = !this.smartSet;
    const generation = ++this.smartSetRecoveryGeneration;
    const current = () =>
      !this.disposed &&
      generation === this.smartSetRecoveryGeneration &&
      (!this.smartSet || this.smartSet.id === id);
    try {
      const saved = await getSavedQuery(this.session, id);
      if (!current() || (this.smartSet?.id === id && saved.revision < this.smartSet.revision))
        return;
      if (
        recovering &&
        (saved.kind !== "query" || this.smartIdentity(saved.payload) !== this.smartIdentity())
      ) {
        try {
          if (localPreferenceStorage()?.getItem(storageKey) === id)
            localPreferenceStorage()?.removeItem(storageKey);
        } catch {
          /* The mismatched definition stays unavailable in this view. */
        }
        return;
      }
      this.smartSet = saved;
      this.reconcileFrozen();
    } catch (cause) {
      if (!current()) return;
      if (cause instanceof APIError && cause.status === 404) {
        this.smartSetRecoveryGeneration++;
        this.smartSet = undefined;
        this.clearFrozen();
        try {
          if (localPreferenceStorage()?.getItem(storageKey) === id)
            localPreferenceStorage()?.removeItem(storageKey);
        } catch {
          /* The missing definition stays unavailable in this view. */
        }
      } else if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
    }
  }
  openSmartSet(saved: SavedQuery) {
    if (saved.kind !== "query" || this.disposed || this.hiding || this.trashing) return false;
    const applied = this.setQuery({ ...saved.payload, sort: photoSearchSort(saved.payload) });
    if (!applied) return false;
    this.rememberSmartSet(saved);
    return true;
  }
  clearFrozen() {
    clearTimeout(this.frozenTimer);
    this.frozen = undefined;
    this.frozenExpired = false;
  }
  expireFrozen() {
    this.clearFrozen();
    this.frozenExpired = true;
  }
  reconcileFrozen() {
    if (this.frozen && Date.parse(this.frozen.run.expires_at) <= Date.now()) {
      this.expireFrozen();
      return;
    }
    if (
      (this.frozen || this.frozenExpired) &&
      (this.frozenIdentity !== this.smartIdentity() ||
        this.frozenRevision !== `${this.smartSet?.id}:${this.smartSet?.revision}`)
    )
      this.clearFrozen();
  }
  setFrozen(result: SavedQueryRunResult) {
    this.clearFrozen();
    this.frozenIdentity = this.smartIdentity();
    this.frozenRevision = `${this.smartSet?.id}:${this.smartSet?.revision}`;
    this.frozen = result;
    this.frozenTimer = setTimeout(
      () => this.expireFrozen(),
      Math.max(0, Date.parse(result.run.expires_at) - Date.now()),
    );
  }
  baseQuery = $state.raw<Query>(photoQuery);
  query = $state.raw<Query>(photoQuery);
  timeline = $state<CaptureDayFacet | undefined>();
  timelineLoading = $state(false);
  timelineError = $state("");
  private timelineController = new AbortController();
  facets = $state<SnapshotPage["facets"]>([]);
  relevanceAvailable = $state(false);
  facetsLoading = $state(false);
  facetsError = $state("");
  facetsRetryable = $state(true);
  private rows = $state.raw<PhotoRow[]>([]);
  private seenIDs: Set<string> | undefined;
  appendedFrom = $state.raw<PhotoRow[]>();
  get items() {
    return this.rows;
  }
  set items(value: PhotoRow[]) {
    this.appendedFrom = undefined;
    this.seenIDs = undefined;
    this.rows = value;
  }
  burstGapSeconds = $state(2);
  expandedStacks = $state<ReadonlySet<string>>(new Set());
  total = $state(0);
  private countStale = false;
  cursor = $state<string | undefined>();
  get mapCapped() {
    return (
      this.projection === "map" &&
      this.items.length >= mapPhotoLimit &&
      this.total > this.items.length
    );
  }
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
  get density() {
    return density;
  }
  selection = $state<SelectionState<string>>(clearSelection<string>());
  allResults = $state(false);
  started = $state(false);
  private expired = false;
  private replacement: "refresh" | "expiry" | "date" | undefined;
  private replacementDate: string | undefined;
  staged:
    | {
        candidate: Map<string, PhotoRow>;
        cursor?: string;
        total: number;
        relevanceAvailable: boolean;
        complete: boolean;
        pages: number;
      }
    | undefined;
  private controller = new AbortController();
  private facetsController = new AbortController();
  private needsFacets = false;
  private rejectsController = new AbortController();
  private disposed = false;

  // Album views and Map show no filter counts, so those stores never request them.
  private readonly withoutCounts: boolean;

  readonly projection?: "map" | undefined;
  private readonly hiddenAccess?: HiddenAccess | undefined;
  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
    readonly hidden = false,
    query: Query = photoQuery,
    { projection, undoState, hiddenAccess }: PhotosOptions = {},
  ) {
    this.projection = projection;
    this.hiddenAccess = hiddenAccess;
    this.authored = new PhotoAuthoredEditor(
      session,
      (cause) => {
        this.authFailure(cause);
      },
      undoState,
      hidden ? hiddenAccess : undefined,
    );
    this.baseQuery = query;
    this.query = query;
    this.withoutCounts = Boolean(query.filters?.set_ids?.length) || this.projection === "map";
  }

  setSort(sort: QuerySort) {
    if (sort.field === "relevance" && !this.relevanceAvailable) return;
    if (this.disposed || !this.authored.canChangeTarget()) return;
    // Timeline counts ignore sort, so only photo paging restarts.
    this.controller.abort();
    this.controller = new AbortController();
    this.loading = false;
    this.staged = undefined;
    this.baseQuery = { ...this.baseQuery, sort };
    this.query = { ...this.query, sort };
    this.items = [];
    this.total = 0;
    this.cursor = undefined;
    this.scrollTop = 0;
    this.started = false;
    this.expired = false;
    this.replacement = undefined;
    this.error = "";
    this.clearSelection();
    return this.loadMore();
  }

  async moveGroup(
    ids: string[],
    direction: -1 | 1,
  ): Promise<
    | { status: "moved"; ids: string[] }
    | { status: "boundary" }
    | { status: "failed"; error: string }
  > {
    if (this.disposed || this.loading) return { status: "boundary" };
    const missingMember =
      "A compared photo is no longer in this query. Exit compare and select photos again.";
    while (!this.disposed) {
      const order = this.items.map((item) => item.asset_id);
      const positions = ids.map((id) => order.indexOf(id));
      if (positions.every((index) => index >= 0)) {
        if (positions.some((index) => index + direction < 0)) return { status: "boundary" };
        if (positions.every((index) => index + direction < order.length))
          return { status: "moved", ids: positions.map((index) => order[index + direction]!) };
      }
      if (this.error) {
        await this.retry();
        if (this.error) return { status: "failed", error: this.error };
        continue;
      }
      if (!this.cursor)
        return positions.some((index) => index < 0)
          ? {
              status: "failed",
              error: missingMember,
            }
          : { status: "boundary" };
      const cursor = this.cursor;
      await this.loadMore();
      if (this.error) return { status: "failed", error: this.error };
      if (this.cursor === cursor && this.items.length === order.length)
        return {
          status: "failed",
          error: positions.some((index) => index < 0)
            ? missingMember
            : "Could not load more photos. Retry navigation.",
        };
    }
    return { status: "boundary" };
  }

  private authFailure(cause: unknown) {
    const expired =
      cause instanceof APIError && (cause.status === 401 || (this.hidden && cause.status === 403));
    if (expired) this.onauthfailure(cause);
    return expired;
  }

  selectedIDs = $derived.by(() =>
    !this.authored.inspectedID && this.authored.targets.length
      ? new Set(this.authored.targets.map((target) => target.asset_id))
      : this.selection.selectedIDs,
  );

  private captureSelection() {
    const selectedIDs = this.selectedIDs;
    this.resolveTargets(selectedIDs);
    this.selection = { selectedIDs: new Set(selectedIDs), anchorID: this.selection.anchorID };
  }

  scope(): PhotoAlbumMembersRequest {
    if (!this.authored.inspectedID && this.authored.targets.length)
      return { asset_ids: [...this.selectedIDs] };
    return this.allResults
      ? { query: structuredClone(this.query) }
      : { asset_ids: [...this.selection.selectedIDs] };
  }

  async lookupMany(ids: string[], signal: AbortSignal) {
    try {
      return await lookupPhotoRows(this.session, ids, signal, undefined, this.hidden);
    } catch (cause) {
      if (!signal.aborted) this.authFailure(cause);
      throw cause;
    }
  }

  async lookup(assetID: string, signal: AbortSignal): Promise<PhotoBrowseRow> {
    try {
      const [row] = await lookupPhotoRows(this.session, [assetID], signal, undefined, this.hidden);
      if (!row) throw new APIError("This photo is no longer in this view.", 404, "not_found");
      return row;
    } catch (cause) {
      if (!signal.aborted) this.authFailure(cause);
      throw cause;
    }
  }

  async detail(row: PhotoRow, signal: AbortSignal): Promise<PhotoBrowseRow> {
    return "content_version_id" in row ? row : this.lookup(row.asset_id, signal);
  }

  async reload(assetID: string, signal: AbortSignal): Promise<PhotoBrowseRow> {
    const query = this.query;
    try {
      if (query.filters.asset_ids && !query.filters.asset_ids.includes(assetID))
        throw new APIError("This photo is no longer in this view.", 404, "not_found");
      const row = (
        await this.page(
          { ...query, filters: { ...query.filters, asset_ids: [assetID] } },
          signal,
        ).catch((cause) => {
          if (!signal.aborted) this.authFailure(cause);
          throw cause;
        })
      ).items[0];
      if (!row) throw new APIError("This photo is no longer in this view.", 404, "not_found");
      signal.throwIfAborted();
      if (!this.disposed && this.query === query) {
        this.cancelPending(true);
        this.items = this.items.map((item) => (item.asset_id === assetID ? row : item));
        this.trashTargets = this.trashTargets.map((item) =>
          item.asset_id === assetID ? row : item,
        );
        void this.resume();
      }
      return await this.detail(row, signal);
    } catch (cause) {
      if (
        !signal.aborted &&
        !this.disposed &&
        this.query === query &&
        cause instanceof APIError &&
        cause.status === 404
      ) {
        this.cancelPending(true);
        if (
          this.items.some((item) => item.asset_id === assetID) ||
          this.trashTargets.some((item) => item.asset_id === assetID)
        )
          this.removeTarget(assetID);
        void this.resume();
        return this.lookup(assetID, signal);
      }
      throw cause;
    }
  }

  private async page(query: Query, signal: AbortSignal, cursor?: string) {
    const page = await listPhotoAssets(
      {
        query,
        projection: this.projection,
        stacks: !this.projection && query.sort.field === "capture_time",
        hidden: this.hidden,
        facets: [],
        page_size: this.projection ? 1000 : 250,
        ...(cursor ? { cursor } : {}),
      },
      { session: this.session, signal },
    );
    if (page.burst_gap_seconds) this.burstGapSeconds = page.burst_gap_seconds;
    return { ...page, items: this.projection ? (page.map_items ?? []) : photoRows(page) };
  }

  async preview(
    photo: PhotoBrowseRow,
    size: "fit" | "large",
    signal: AbortSignal,
  ): Promise<PhotoPreviewSlot> {
    if (photo.previews[size].state !== "missing") return photo.previews[size];
    try {
      const slot = await preparePhotoPreview(
        photo.asset_id,
        { content_version_id: photo.content_version_id, size, hidden: this.hidden },
        { session: this.session, signal },
      );
      signal.throwIfAborted();
      photo.previews[size] = slot;
      return slot;
    } catch (cause) {
      if (!signal.aborted) this.authFailure(cause);
      throw cause;
    }
  }

  async original(photo: PhotoBrowseRow, signal: AbortSignal): Promise<string> {
    const conflict = () =>
      new APIError(
        "The photo changed. Reload this photo before viewing its original.",
        409,
        "photo_display_changed",
      );
    const check = async () => {
      signal.throwIfAborted();
      const current = await this.lookup(photo.asset_id, signal).catch((cause) => {
        if (cause instanceof APIError && cause.status === 404) throw conflict();
        throw cause;
      });
      if (
        current.display_file_id !== photo.display_file_id ||
        current.node_id !== photo.node_id ||
        current.content_version_id !== photo.content_version_id
      )
        throw conflict();
    };
    await check();
    let objectURL: string;
    try {
      const node = await getNode(photo.node_id, { session: this.session, signal });
      if (node.current_version_id !== photo.content_version_id) throw conflict();
      const result = await readVerifiedPreview(
        this.session,
        selectedSourceFromNode(node, photo.name),
        node.revision,
        signal,
        () => {},
      );
      if (result.kind !== "image") throw new Error("This original cannot be viewed as an image.");
      objectURL = result.url;
    } catch (cause) {
      if (!signal.aborted) this.authFailure(cause);
      throw cause;
    }
    try {
      await check();
      signal.throwIfAborted();
      return objectURL;
    } catch (cause) {
      URL.revokeObjectURL(objectURL);
      throw cause;
    }
  }

  // setQuery replaces the search, filter, or album scope. It clears the selected
  // date and the timeline counts, which describe the previous scope.
  setQuery(query: Query) {
    if (this.disposed || this.hiding || this.trashing || !this.authored.canChangeTarget()) return;
    this.cancelPending();
    let next: Query;
    try {
      next = parseQuery(canonicalQuery(query));
    } catch (cause) {
      this.error = cause instanceof Error ? cause.message : String(cause);
      return;
    }
    this.baseQuery = next;
    this.query = next;
    this.date = undefined;
    this.reconcileFrozen();
    this.timeline = undefined;
    this.timelineError = "";
    this.items = [];
    this.facets = [];
    this.total = 0;
    this.cursor = undefined;
    this.relevanceAvailable = false;
    this.facetsError = "";
    this.needsFacets = false;
    this.started = false;
    this.error = "";
    this.expired = false;
    this.replacement = undefined;
    this.scrollTop = 0;
    this.clearSelection();
    if (this.view === "timeline") void this.loadTimeline();
    return this.loadMore();
  }

  setDensity(value: Density) {
    density = value;
    try {
      localPreferenceStorage()?.setItem(densityKey, value);
    } catch {
      /* Keep the current session's preference. */
    }
  }

  async loadMore(preserve?: () => (() => Promise<void>) | undefined): Promise<void> {
    if (this.disposed || this.hiding || this.loading || this.trashing || this.error) return;
    if (this.projection && this.replacement) {
      await this.retry(preserve);
      return;
    }
    if (this.projection === "map" && this.items.length >= mapPhotoLimit) return;
    if (this.started && !this.cursor) return;
    this.loading = true;
    const controller = this.controller;
    let signal = controller.signal;
    try {
      const first = !this.started;
      let pages = 0;
      do {
        signal = this.projection
          ? AbortSignal.any([controller.signal, AbortSignal.timeout(60_000)])
          : controller.signal;
        const page = await this.page(this.query, signal, this.cursor);
        if (signal.aborted) throw signal.reason;
        if (!this.started) this.relevanceAvailable = page.relevance_available;
        pages++;
        const seen = (this.seenIDs ??= new Set(this.items.map((item) => item.asset_id)));
        const added: PhotoRow[] = [];
        let capped = false;
        for (const item of page.items) {
          if (seen.has(item.asset_id)) continue;
          if (this.projection === "map" && this.items.length + added.length >= mapPhotoLimit) {
            capped = true;
            break;
          }
          seen.add(item.asset_id);
          added.push(item);
        }
        const restore = preserve?.();
        const previous = this.items;
        this.rows = [...previous, ...added];
        this.appendedFrom = previous;
        if (this.allResults && !this.authored.targets.length)
          this.selection = {
            selectedIDs: new Set(this.items.map((item) => item.asset_id)),
            anchorID: undefined,
          };
        if (!this.started || !this.projection) this.total = page.total;
        this.cursor = capped ? this.cursor : page.next_cursor;
        this.started = true;
        await restore?.();
      } while (
        this.projection &&
        this.cursor &&
        pages < mapPageLimit &&
        this.items.length < mapPhotoLimit &&
        !controller.signal.aborted
      );
      if (first && !controller.signal.aborted) {
        this.needsFacets = true;
        void this.retryFacets();
      }
    } catch (cause) {
      if (controller.signal.aborted) return;
      this.authFailure(cause);
      this.expired =
        cause instanceof APIError &&
        ["cursor_expired", "invalid_photo_cursor"].includes(cause.code);
      this.error =
        this.projection &&
        (signal.aborted || (cause instanceof Error && cause.name === "TimeoutError"))
          ? "Photo refresh timed out. Retry to keep browsing."
          : cause instanceof Error
            ? cause.message
            : String(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }

  async setHidden(
    id: string,
    preserve?: () => (() => Promise<void>) | undefined,
    onhidden?: () => void | Promise<void>,
    onactionerror?: (error: string) => void,
    oninvalidate?: () => void | Promise<void>,
  ) {
    if (this.hiding || this.trashing || this.disposed || !this.authored.canChangeTarget()) return;
    if (this.allResults && !this.authored.targets.length && this.selectedIDs.has(id)) {
      this.actionError = "Hide changes loaded photos only. Choose Select loaded photos first.";
      onactionerror?.(this.actionError);
      return;
    }
    const members = this.resolveTargets(this.selectedIDs.has(id) ? this.selectedIDs : [id]);
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
    await this.mutateBatch(
      members,
      async (member) => {
        const signal = AbortSignal.timeout(60_000);
        const receipt = await (this.hidden ? unhidePhotoAsset : hidePhotoAsset)(
          member.asset_id,
          { "If-Match": String(member.revision) },
          { session: this.session, signal },
        );
        if (signal.aborted) throw signal.reason;
        return receipt;
      },
      (cause) => {
        failures++;
        failure =
          cause instanceof APIError && cause.code === "hidden_not_configured"
            ? "Set a passcode in the Hidden view first."
            : cause instanceof APIError && cause.code === "hidden_locked" && !this.hidden
              ? "This photo is already hidden."
              : cause instanceof Error
                ? cause.message
                : String(cause);
        if (this.authFailure(cause)) return true;
        return false;
      },
      async (changed) => {
        this.actionError = failures
          ? `${failures} photo${failures === 1 ? "" : "s"} failed: ${failure}`
          : "";
        onactionerror?.(this.actionError);
        if (changed) await onhidden?.();
      },
      "hiding",
      preserve,
      oninvalidate,
    );
  }

  cancelPending(invalidateReplacement = false) {
    if (this.disposed) return;
    if (invalidateReplacement && this.replacement && !this.error)
      this.error = "Photo list changed. Retry to finish loading.";
    this.controller.abort();
    this.controller = new AbortController();
    this.facetsController.abort();
    this.facetsLoading = false;
    this.authored.validating = false;
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
    if (this.view === "timeline" && !this.timeline && !this.timelineLoading && !this.timelineError)
      void this.loadTimeline();
    if (this.error) return;
    if (this.countStale && !this.replacement) void this.recount(preserve);
    if (this.replacement) return this.retry(preserve);
    if (!this.started || (this.projection && this.cursor)) return this.loadMore(preserve);
  }

  async retry(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.replacement || this.expired)
      return this.replace(
        this.replacement ?? "expiry",
        preserve,
        this.replacementDate,
        this.projection === "map",
      );
    this.error = "";
    this.expired = false;
    return this.loadMore(preserve);
  }

  metadataChanged(receipt: PhotoAuthoredReceipt): Map<
    string,
    PhotoAuthoredReceipt["after"][number]["values"]["rotation"]
  > & {
    stacksUnresolved?: boolean;
  } {
    const before = new Map(receipt.before.map((file) => [file.file_id, file]));
    const rotations = new Map(
      receipt.after
        .filter((file) => before.get(file.file_id)?.values.rotation !== file.values.rotation)
        .map((file) => [file.file_id, file.values.rotation]),
    );
    let stacksUnresolved = false;
    if (this.projection) {
      if (rotations.size)
        this.items = this.items.map((row) =>
          "display_file_id" in row && rotations.has(row.display_file_id)
            ? { ...row, rotation: rotations.get(row.display_file_id)! }
            : row,
        );
    } else {
      // Editors hold every file of the photos they edited, so they know each asset's picks.
      const targets = [this.authored, this.cullEditor].flatMap((editor) =>
        editor ? [...editor.targets, ...editor.stackTargets] : [],
      );
      const rows = this.items as PhotoBrowseRow[];
      const known = new Set(targets.map((target) => target.file_id));
      stacksUnresolved =
        rows.some((row) => row.stack) &&
        receipt.after.some(
          (file) =>
            !known.has(file.file_id) && before.get(file.file_id)?.values.flag !== file.values.flag,
        );
      this.items = patchPhotoRows(rows, receipt, (assetID) => {
        const files = targets.filter((target) => target.asset_id === assetID);
        return files.length ? files.some((target) => target.values.flag === "pick") : undefined;
      });
    }
    if (
      !this.dependsOnAuthored &&
      receipt.after.some((file) => {
        const previous = before.get(file.file_id)?.values;
        return (
          previous &&
          (previous.rating !== file.values.rating ||
            previous.flag !== file.values.flag ||
            previous.label !== file.values.label)
        );
      })
    )
      void this.reloadCounts();
    return Object.assign(rotations, { stacksUnresolved });
  }

  /** Whether a metadata edit can change which photos this view lists or how it orders them. */
  get dependsOnAuthored() {
    const filters = this.query.filters ?? {};
    return (
      Boolean(this.query.text.trim()) ||
      authoredFilters.some((key) => {
        const value = filters[key];
        return Array.isArray(value)
          ? value.length > 0
          : value !== undefined && value !== false && value !== "";
      })
    );
  }

  /** Whether this view must reload after an edit with these display-file rotations, because compact Map rows cannot be matched to rotated files. */
  reloadsAfter(rotations: ReadonlyMap<string, number> & { stacksUnresolved?: boolean }) {
    return (
      rotations.stacksUnresolved ||
      this.dependsOnAuthored ||
      (this.projection === "map" && rotations.size > 0)
    );
  }

  refresh(preserve?: () => (() => Promise<void>) | undefined) {
    this.timelineController.abort();
    this.timeline = undefined;
    this.timelineError = "";
    this.timelineLoading = false;
    if (this.view === "timeline") void this.loadTimeline();
    return this.replacement === "date"
      ? this.replace("date", undefined, this.replacementDate)
      : this.replace("refresh", preserve);
  }

  setView(view: "grid" | "timeline") {
    this.view = view;
    if (view === "timeline" && !this.timeline && !this.timelineLoading) void this.loadTimeline();
  }

  async loadTimeline() {
    if (this.disposed || this.hidden) return;
    this.timelineController.abort();
    const controller = (this.timelineController = new AbortController());
    this.timelineLoading = true;
    this.timelineError = "";
    try {
      const page = await fetchPhotoTimeline(this.session, this.baseQuery, controller.signal);
      if (!controller.signal.aborted) this.timeline = page.facets[0];
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (this.authFailure(cause)) return;
      if (
        cause instanceof APIError &&
        [
          "snapshot_too_large",
          "snapshot_capacity",
          "snapshot_busy",
          "snapshot_unavailable",
        ].includes(cause.code)
      ) {
        this.timeline = {
          dimension: "capture_day",
          available: false,
          reason: cause.code,
          values: [],
        };
      } else this.timelineError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.timelineLoading = false;
    }
  }

  private async eligibleSelection(ids: Iterable<string>, query: Query, signal: AbortSignal) {
    query = this.projection ? query : photoQuery;
    if (this.projection && this.authored.targets.length)
      query = {
        ...photoQuery,
        filters: query.filters?.gps_bounds ? { gps_bounds: query.filters.gps_bounds } : {},
      };
    const eligible = new Set(ids);
    const absent = [
      ...new Set([
        ...this.trashTargets
          .filter((row) => this.selection.selectedIDs.has(row.asset_id))
          .map((row) => row.asset_id),
        ...this.authored.targets.map((target) => target.asset_id),
      ]),
    ].filter((id) => !eligible.has(id));
    const rows = await lookupPhotoRows<PhotoRow>(
      this.session,
      absent,
      signal,
      undefined,
      this.hidden,
      (asset_ids, signal) =>
        this.page(
          { ...query, filters: { ...query.filters, asset_ids } },
          AbortSignal.any([signal, AbortSignal.timeout(60_000)]),
        ),
    );
    for (const row of rows) eligible.add(row.asset_id);
    return eligible;
  }

  async revalidateAuthored(valid: () => boolean): Promise<boolean> {
    const controller = this.controller;
    this.authored.validating = true;
    try {
      const eligible = await this.eligibleSelection([], this.query, controller.signal);
      if (controller.signal.aborted || this.disposed || !valid()) return false;
      if (this.authored.targets.length) {
        this.captureSelection();
        this.selection = {
          ...this.selection,
          selectedIDs: new Set([...this.selection.selectedIDs].filter((id) => eligible.has(id))),
        };
      }
      this.authored.revalidate(eligible);
      return true;
    } catch (cause) {
      if (!controller.signal.aborted) this.authFailure(cause);
      if (controller.signal.aborted) return false;
      throw cause;
    }
  }

  selectDate(date?: string) {
    if (!this.authored.canChangeTarget()) return;
    return this.replace("date", undefined, date);
  }

  async previewRejects(selected = false) {
    if (
      this.disposed ||
      this.trashing ||
      this.hiding ||
      this.rejectsLoading ||
      !this.authored.canChangeTarget()
    )
      return;
    this.rejects = undefined;
    this.rejectsError = "";
    this.rejectsSelected = selected;
    const query = $state.snapshot(selected ? photoQuery : this.query);
    if (selected) {
      const ids = [...this.selectedIDs].sort();
      if (!ids.length || ids.length > this.rejectsSelectionLimit) {
        this.rejectsError = `Select between 1 and ${this.rejectsSelectionLimit} photos to preview selected rejects.`;
        return;
      }
      query.filters = { asset_ids: ids };
    }
    this.rejectsLoading = true;
    this.rejectsController.abort();
    const controller = (this.rejectsController = new AbortController());
    try {
      const result = await preflightPhotoRejects(
        { query, hidden: this.hidden },
        {
          session: this.session,
          signal: AbortSignal.any([controller.signal, AbortSignal.timeout(photoRejectsTimeout)]),
        },
      );
      if (!controller.signal.aborted) {
        this.rejects = result;
        this.rejectsSelectionLimit = result.selection_limit;
      }
    } catch (cause) {
      if (!controller.signal.aborted) this.rejectsFailure(cause);
    } finally {
      if (this.rejectsController === controller) this.rejectsLoading = false;
    }
  }

  async trashRejects(preserve?: () => (() => Promise<void>) | undefined, ontrashed?: () => void) {
    if (
      this.disposed ||
      this.trashing ||
      this.hiding ||
      !this.rejects?.targets.length ||
      !this.authored.canChangeTarget()
    )
      return false;
    const targets = this.rejects.targets;
    this.cancelPending();
    this.trashing = true;
    this.rejectsError = "";
    return this.write(async () => {
      try {
        const result = await movePhotoRejects(
          { hidden: this.hidden, targets },
          { session: this.session, signal: AbortSignal.timeout(photoRejectsTimeout) },
        );
        if (
          result.moved.length !== targets.length ||
          result.moved.some((id, i) => id !== targets[i].asset_id)
        )
          throw new Error("Invalid rejects move response");
        await this.removeConfirmed(result.moved, preserve);
        this.rejects = undefined;
        ontrashed?.();
        await this.refresh(preserve);
        return true;
      } catch (cause) {
        this.rejects = undefined;
        this.rejectsFailure(cause);
        if (
          !(cause instanceof APIError) ||
          (cause.status >= 500 && cause.code !== "maintenance_busy")
        ) {
          this.rejectsError = "The move may have completed. Refresh Photos before trying again.";
          ontrashed?.();
        }
        await this.refresh(preserve);
        return false;
      } finally {
        this.trashing = false;
      }
    });
  }

  private rejectsFailure(cause: unknown) {
    if (this.disposed) return;
    this.rejectsError = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.code === "audit_mutation_unsupported")
      this.rejectsError += ` The batch stayed in Docbank. Close this dialog, select up to ${this.rejectsSelectionLimit} other photos, and choose Selected photos.`;
    this.authFailure(cause);
  }

  private async replace(
    mode: "refresh" | "expiry" | "date",
    preserve?: () => (() => Promise<void>) | undefined,
    date?: string,
    resume = false,
  ) {
    if (this.disposed) return false;
    this.authored.conceal();
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
    const staged = (this.staged =
      resume && this.staged
        ? this.staged
        : {
            candidate: new Map<string, PhotoRow>(),
            cursor: undefined,
            total: 0,
            relevanceAvailable: false,
            complete: false,
            pages: 0,
          });
    const candidate = staged.candidate;
    let cursor = staged.cursor;
    let total = staged.total;
    let reachedPrefix = !count;
    try {
      while (!staged.complete) {
        signal = AbortSignal.any([controller.signal, AbortSignal.timeout(60_000)]);
        const page = await this.page(query, signal, cursor);
        if (signal.aborted) throw signal.reason;
        if (!cursor) staged.relevanceAvailable = page.relevance_available;
        const reachedPreviously = reachedPrefix;
        staged.pages++;
        let capped = false;
        for (const item of page.items) {
          if (
            this.projection === "map" &&
            !candidate.has(item.asset_id) &&
            candidate.size >= mapPhotoLimit
          ) {
            capped = true;
            break;
          }
          candidate.set(item.asset_id, item);
        }
        reachedPrefix ||= (!!tail && candidate.has(tail)) || candidate.size >= count;
        total = page.total;
        cursor = capped ? cursor : page.next_cursor;
        staged.cursor = cursor;
        staged.total = total;
        staged.complete =
          !cursor ||
          (!!this.projection &&
            (candidate.size >= mapPhotoLimit || staged.pages >= mapPageLimit)) ||
          (!this.projection &&
            (mode === "date" || (reachedPrefix && (mode === "refresh" || reachedPreviously))));
      }
      const eligible =
        mode === "date"
          ? new Set(candidate.keys())
          : await this.eligibleSelection(candidate.keys(), query, controller.signal);
      if (controller.signal.aborted) return false;
      const restore = mode === "date" ? undefined : preserve?.();
      const captured = !this.authored.inspectedID && !!this.authored.targets.length;
      if (captured) this.captureSelection();
      this.authored.revalidate(eligible);
      this.editScopeRevision++;
      this.items = [...candidate.values()];
      this.total = total;
      this.countStale = false;
      this.relevanceAvailable = staged.relevanceAvailable;
      this.facets = [];
      this.facetsError = "";
      this.cursor = cursor;
      this.started = true;
      if (mode === "date") {
        this.clearFrozen();
        this.query = query;
        this.date = date;
        this.scrollTop = 0;
        this.clearSelection();
      } else {
        if (!this.authored.targets.length)
          this.selection =
            this.allResults && !captured
              ? { selectedIDs: new Set(candidate.keys()), anchorID: undefined }
              : reconcileIDSelection(this.selection, eligible);
        if (!this.authored.targets.length && !this.selection.selectedIDs.size)
          this.allResults = false;
        this.trashTargets = this.trashTargets.filter(
          (item) => eligible.has(item.asset_id) && this.selectedIDs.has(item.asset_id),
        );
      }
      this.expired = false;
      this.replacement = undefined;
      this.staged = undefined;
      await restore?.();
      if (!controller.signal.aborted) {
        this.needsFacets = true;
        void this.retryFacets();
      }
      return !controller.signal.aborted;
    } catch (cause) {
      if (controller.signal.aborted) return false;
      this.authFailure(cause);
      if (
        cause instanceof APIError &&
        ["cursor_expired", "invalid_photo_cursor"].includes(cause.code)
      )
        this.staged = undefined;
      this.error =
        signal.aborted || (cause instanceof Error && cause.name === "TimeoutError")
          ? "Photo refresh timed out. Retry to keep browsing."
          : cause instanceof Error
            ? cause.message
            : String(cause);
      return false;
    } finally {
      if (this.controller === controller) {
        this.loading = false;
        this.authored.validating = false;
      }
    }
  }

  async trashSelected(preserve?: () => (() => Promise<void>) | undefined, ontrashed?: () => void) {
    if (this.trashing || this.hiding || this.disposed || !this.authored.canChangeTarget())
      return false;
    const selected = this.resolveTargets(this.scope().asset_ids ?? this.selectedIDs);
    if (!selected) {
      this.trashError = "Load and select the photos again before moving them to trash.";
      return false;
    }
    this.cancelPending();
    this.trashing = true;
    this.trashError = "";
    const successes = await this.mutateBatch(
      selected,
      (item) => {
        const options = { session: this.session, signal: AbortSignal.timeout(60_000) };
        return trashPhotoAsset(item.asset_id, { "If-Match": String(item.revision) }, options);
      },
      (cause) => {
        if (this.authFailure(cause)) return true;
        this.trashError = cause instanceof Error ? cause.message : String(cause);
        return false;
      },
      (changed) => {
        if (changed) ontrashed?.();
      },
      "trashing",
      preserve,
    );
    return successes === selected.length;
  }

  private async mutateBatch(
    targets: PhotoTarget[],
    request: (item: PhotoTarget) => Promise<{ id: string; revision: number }>,
    onerror: (cause: unknown) => boolean,
    oncomplete: (changed: boolean) => void | Promise<void>,
    action: "hiding" | "trashing",
    preserve?: () => (() => Promise<void>) | undefined,
    onconfirmed?: () => void | Promise<void>,
  ) {
    const batch = async () => {
      let successes = 0;
      let uncertain = false;
      try {
        for (const item of targets) {
          try {
            const receipt = await request(item);
            if (receipt.id !== item.asset_id || receipt.revision <= item.revision)
              throw new Error(
                `Photo${action === "trashing" ? " trash" : ""} response did not confirm the selected photo. Refresh and retry.`,
              );
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
        else if (this.countStale) await this.recount(preserve);
        return successes;
      } finally {
        this[action] = false;
        if (this.projection) void this.resume(preserve);
      }
    };
    return this.write(batch, this.hidden || action === "hiding");
  }

  private write<T>(request: () => Promise<T>, needsHidden = this.hidden): Promise<T> {
    return this.hiddenAccess && needsHidden ? this.hiddenAccess.run(request) : request();
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
    const controller = (this.facetsController = new AbortController());
    this.needsFacets = true;
    this.facetsLoading = true;
    this.facetsError = "";
    this.facetsRetryable = true;
    try {
      // Counts describe the whole scope, like timeline counts, so a selected date does not narrow them.
      const query = parseQuery(canonicalQuery(this.baseQuery));
      query.sort = { field: "capture_time", direction: "desc" };
      const page = await listPhotoAssets(
        { query, hidden: this.hidden, facets: [...photoFacets], page_size: 1 },
        { session: this.session, signal: controller.signal },
      );
      if (controller.signal.aborted) return;
      this.facets = (page.facets ?? []) as SnapshotPage["facets"];
      this.needsFacets = false;
      const unavailable = this.facets.filter((facet) => !facet.available);
      if (unavailable.length) {
        this.facetsRetryable = unavailable.some(
          (facet) =>
            facet.reason !== "member_budget_exceeded" && facet.reason !== "byte_budget_exceeded",
        );
        this.facetsError = this.facetsRetryable
          ? "Some photo counts couldn't be loaded."
          : "Photo counts exceed the query's limits. Narrow your search or filters.";
      }
    } catch (cause) {
      if (controller.signal.aborted) return;
      this.authFailure(cause);
      this.needsFacets = false;
      this.facetsError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!controller.signal.aborted) this.facetsLoading = false;
    }
  }

  stackMembers(id: string) {
    const row = this.items.find((item) => item.asset_id === id);
    return row && "stack" in row && row.stack && !this.expandedStacks.has(row.stack.asset_ids[0]!)
      ? row.stack.asset_ids
      : [id];
  }
  toggleStack(id: string) {
    const row = this.items.find((item) => item.asset_id === id);
    if (!row || !("stack" in row) || !row.stack) return;
    const key = row.stack.asset_ids[0]!;
    const expanded = new Set(this.expandedStacks);
    if (expanded.has(key)) expanded.delete(key);
    else expanded.add(key);
    this.expandedStacks = expanded;
  }
  private selectStackMembers(id: string, checked: boolean, range: boolean, orderedIDs: string[]) {
    const selected = new Set(this.selection.selectedIDs);
    const anchor = this.selection.anchorID;
    const start = anchor ? orderedIDs.indexOf(anchor) : -1;
    const end = orderedIDs.indexOf(id);
    const affected = new Set(
      range && start >= 0 && end >= 0
        ? orderedIDs.slice(Math.min(start, end), Math.max(start, end) + 1)
        : [id],
    );
    const handled = new Set<string>();
    for (const row of this.items) {
      if (
        !affected.has(row.asset_id) ||
        !("stack" in row) ||
        !row.stack ||
        this.expandedStacks.has(row.stack.asset_ids[0]!)
      )
        continue;
      const key = row.stack.asset_ids[0]!;
      if (handled.has(key)) continue;
      handled.add(key);
      for (const member of row.stack.asset_ids) {
        if (checked) selected.add(member);
        else selected.delete(member);
      }
    }
    this.selection = { ...this.selection, selectedIDs: selected };
  }
  async checkBurstGap(preserve?: () => (() => Promise<void>) | undefined) {
    if (this.projection || this.query.sort.field !== "capture_time") return;
    try {
      const settings = await getPhotoSettings({ session: this.session });
      if (settings.burst_gap_seconds !== this.burstGapSeconds) await this.refresh(preserve);
    } catch (cause) {
      this.actionError = cause instanceof Error ? cause.message : String(cause);
    }
  }
  async setBurstGap(gap: number, preserve?: () => (() => Promise<void>) | undefined) {
    if (gap < 1 || gap > 30 || !Number.isInteger(gap)) {
      this.actionError = "burst gap must be 1 to 30 seconds";
      return;
    }
    if (!this.authored.canChangeTarget()) return;
    this.actionError = "";
    try {
      const settings = await getPhotoSettings({ session: this.session });
      const next = await setPhotoSettings(
        { burst_gap_seconds: gap },
        { "If-Match": `"${settings.revision}"` },
        { session: this.session },
      );
      this.burstGapSeconds = next.burst_gap_seconds;
      this.expandedStacks = new Set();
      await this.refresh(preserve);
    } catch (cause) {
      this.authFailure(cause);
      this.actionError = cause instanceof Error ? cause.message : String(cause);
    }
  }
  select(id: string, event: MouseEvent, orderedIDs: string[]) {
    if (event.button !== 0) return;
    if (!this.authored.canChangeTarget()) return;
    this.captureSelection();
    this.changeEditScope();
    this.allResults = false;
    if (event.shiftKey || event.ctrlKey || event.metaKey) {
      this.selection = toggleIDSelection(
        this.selection,
        orderedIDs,
        id,
        event.shiftKey || !this.selection.selectedIDs.has(id),
        event.shiftKey,
      );
    } else this.selection = { selectedIDs: new Set(this.stackMembers(id)), anchorID: id };
    this.selectStackMembers(id, this.selection.selectedIDs.has(id), event.shiftKey, orderedIDs);
    this.pruneTrashTargets();
  }

  check(id: string, checked: boolean, range: boolean, orderedIDs: string[]) {
    if (!this.authored.canChangeTarget()) return;
    this.captureSelection();
    this.changeEditScope();
    this.allResults = false;
    this.selection = toggleIDSelection(this.selection, orderedIDs, id, checked, range);
    this.selectStackMembers(id, checked, range, orderedIDs);
    this.pruneTrashTargets();
  }

  private resolveTargets(ids: Iterable<string>): PhotoTarget[] | undefined {
    this.pruneTrashTargets();
    const rows = new Map(
      [...this.trashTargets, ...this.items].map((item) => [item.asset_id, item]),
    );
    if (!this.authored.inspectedID)
      for (const target of this.authored.targets)
        rows.set(target.asset_id, { asset_id: target.asset_id, revision: target.asset_revision });
    const resolved = [...ids].map((id) => rows.get(id));
    if (!resolved.length || resolved.some((item) => !item)) return;
    const targets = resolved
      .filter((item): item is PhotoTarget => !!item)
      .map((item) => ({ ...item }));
    this.trashTargets = [
      ...new Map([...this.trashTargets, ...targets].map((item) => [item.asset_id, item])).values(),
    ];
    return targets;
  }

  private async removeConfirmed(ids: string[], preserve?: () => (() => Promise<void>) | undefined) {
    const restore = preserve?.();
    for (const id of ids) this.removeTarget(id);
    await restore?.();
  }

  private removeTarget(id: string) {
    const ids = new Set(this.selectedIDs);
    const listed = this.items.some((item) => item.asset_id === id);
    this.authored.targets = this.authored.targets.filter((target) => target.asset_id !== id);
    this.items = this.items.filter((item) => item.asset_id !== id);
    // Map checks prepared targets only against its bounds, so an unlisted one may be beyond the loaded rows or outside the result.
    if (listed || !this.projection) this.total = Math.max(0, this.total - 1);
    else this.countStale = true;
    this.trashTargets = this.trashTargets.filter((item) => item.asset_id !== id);
    ids.delete(id);
    if (!ids.size && !this.authored.targets.length) this.allResults = false;
    this.selection = { selectedIDs: ids, anchorID: undefined };
  }

  private async recount(preserve?: () => (() => Promise<void>) | undefined) {
    const controller = this.controller;
    try {
      const page = await listPhotoAssets(
        {
          query: this.query,
          projection: this.projection,
          hidden: this.hidden,
          facets: [],
          page_size: 1,
        },
        {
          session: this.session,
          signal: AbortSignal.any([controller.signal, AbortSignal.timeout(60_000)]),
        },
      );
      if (!controller.signal.aborted) {
        this.total = page.total;
        this.countStale = false;
      }
    } catch (cause) {
      if (controller.signal.aborted || this.authFailure(cause)) return;
      await this.refresh(preserve);
    }
  }

  private pruneTrashTargets() {
    this.trashTargets = this.trashTargets.filter((item) => this.selectedIDs.has(item.asset_id));
  }

  clearSelection() {
    if (!this.changeEditScope()) return;
    this.allResults = false;
    this.selection = clearSelection<string>();
    this.trashTargets = [];
  }
  selectLoaded(ids = this.items.map((item) => item.asset_id)) {
    if (!this.changeEditScope()) return;
    this.allResults = false;
    this.selection = { selectedIDs: new Set(ids), anchorID: undefined };
    this.pruneTrashTargets();
  }
  selectAllResults() {
    if (!this.authored.canChangeTarget()) return;
    this.selectLoaded();
    this.allResults = true;
  }
  dispose() {
    this.clearFrozen();
    this.authored.dispose();
    this.cullEditor?.dispose();
    this.cull?.cancel();
    this.viewerReturn = undefined;
    this.disposed = true;
    this.staged = undefined;
    this.controller.abort();
    this.timelineController.abort();
    this.facetsController.abort();
    this.rejectsController.abort();
  }
}
