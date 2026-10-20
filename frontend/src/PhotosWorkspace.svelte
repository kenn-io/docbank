<script module lang="ts">
  // Map keeps Library's text and filters, so search and filter counts live in Library and Hidden.
  export const searchablePhotoView = (albumID: string, mapMode: boolean) => !albumID && !mapMode;
</script>

<script lang="ts">
  import { reportAction } from "./action-events.js";
  import { type PhotoBrowseRow } from "./photoRows.js";
  import { fittedPhotoView } from "./photoView.js";
  import { onDestroy, onMount, tick, untrack } from "svelte";
  import { MediaQuery } from "svelte/reactivity";
  import {
    Button,
    EmptyState,
    IconButton,
    Menu,
    MenuTrigger,
    MenuContent,
    MenuItem,
    MenuRadioGroup,
    MenuRadioItem,
    MenuSeparator,
    MEDIA,
    Modal,
    TextInput,
    SelectDropdown,
    Chip,
    Spinner,
    Notice,
    type ShortcutManager,
  } from "@kenn-io/kit-ui";
  import StarIcon from "@lucide/svelte/icons/star";
  import ImageIcon from "@lucide/svelte/icons/image";
  import { activeFilterCount, isFilterSet } from "./photoSearch.js";
  import {
    mapPhotoLimit,
    maxComparePhotos,
    type Photos,
    type PhotoReturn,
  } from "./photos.svelte.js";
  import { groupPhotos, monthLabel, ROW_HEIGHTS, type Density } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import { isAppShortcutSuppressed } from "./shortcuts.js";
  import PhotoSmartSet from "./PhotoSmartSet.svelte";
  import PhotoGrid from "./PhotoGrid.svelte";
  import PhotoInspector from "./PhotoInspector.svelte";
  import type {
    PhotoAuthoredPatch,
    PhotoAuthoredReceipt,
    PhotoKeywordEdit,
  } from "./generated/docbank.js";
  import PhotoLoupe from "./PhotoLoupe.svelte";
  import PhotoTimeline from "./PhotoTimeline.svelte";
  import SlidersHorizontalIcon from "@lucide/svelte/icons/sliders-horizontal";
  import EllipsisIcon from "@lucide/svelte/icons/ellipsis";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import type { PhotoMapRow } from "./generated/docbank.js";
  import FacetSidebar from "./FacetSidebar.svelte";
  import type { ExportInput } from "./exportState.js";
  import { maxPhotoExportMembers } from "./exports.js";
  import SelectionDock from "./SelectionDock.svelte";
  import PhotoAlbumPicker from "./PhotoAlbumPicker.svelte";
  import PhotoAlbumPending from "./PhotoAlbumPending.svelte";
  import { type PhotoAlbums, type PhotoAlbumItem } from "./photoAlbums.svelte.js";
  import { PhotoCull as CullRun } from "./photoCull.svelte.js";
  import { photoCullStorage } from "./photoCullStorage.js";
  import PhotoCull from "./PhotoCull.svelte";
  import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";
  import PhotoCompare from "./PhotoCompare.svelte";
  import PhotoRejectsModal from "./PhotoRejectsModal.svelte";

  import type PhotoMap from "./PhotoMap.svelte";
  import type { MapTiles } from "./photoMap.js";
  let {
    photos,
    vaultID = "",
    cache,
    shortcuts,
    tags,
    tagsError = "",
    tagsLoading = false,
    tagsTotal = 0,
    onloadtags,
    onloadmoretags,
    onmetadatachanged,
    onhelp,
    title = "Library",
    mapMode = false,
    tiles = { tile_url: "", host: "", attribution: "", max_zoom: 19 },
    albums,
    albumID = "",
    onnavigate = () => {},
    ontrashed,
    onhidden,
    onactionerror,
    oninvalidate,
    photoID = "",
    onphotochange = () => true,
    onexport,
    onadd,
    uploadChannelError = "",
  }: {
    photos: Photos;
    vaultID?: string;
    cache: PhotoPreviewCache;
    shortcuts?: ShortcutManager;
    tags?: import("./generated/docbank.js").Tag[];
    tagsError?: string;
    tagsLoading?: boolean;
    tagsTotal?: number;
    onloadtags?: () => Promise<void>;
    onloadmoretags?: () => Promise<void>;
    onmetadatachanged?: (
      receipt: PhotoAuthoredReceipt,
      tags: import("./generated/docbank.js").Tag[],
    ) => void;
    onhelp?: () => void;
    title?: string;
    mapMode?: boolean;
    tiles?: MapTiles;
    albums?: PhotoAlbums;
    albumID?: string;
    onnavigate?: (path: string) => void;
    ontrashed?: (source: Photos) => void;
    onhidden?: () => void | Promise<void>;
    onactionerror?: (error: string) => void;
    oninvalidate?: () => void | Promise<void>;
    photoID?: string;
    onphotochange?: (id: string, mode: "open" | "step" | "close") => boolean;
    onexport?: (input: ExportInput) => void;
    onadd?: () => void;
    uploadChannelError?: string;
  } = $props();
  let cull = $state.raw<CullRun>();
  const culling = $derived(photos.culling);
  const comparing = $derived(photos.comparing);
  let enteringReview = false;
  let cullConfirmation = $state<PhotoCullConfirmation>();
  let returnFocus: HTMLElement | undefined;
  $effect(() => {
    const owner = photos;
    const id = vaultID;
    const review = untrack(() => {
      if (!id) return;
      if (owner.cullVaultID !== id) {
        owner.cull?.cancel();
        owner.cullEditor?.dispose();
        owner.cullEditor = undefined;
        owner.culling = false;
        owner.cull = new CullRun(owner.session, photoCullStorage(id), owner.onauthfailure);
        owner.cullVaultID = id;
      }
      if (!owner.culling) void owner.cull!.probe();
      return owner.cull;
    });
    cull = review;
    return () => review?.cancel();
  });
  async function startCull(resume = false) {
    const review = cull;
    if (!review || enteringReview || culling || comparing || !editor.canChangeTarget()) return;
    enteringReview = true;
    returnFocus = document.activeElement as HTMLElement;
    try {
      const captured = resume
        ? await review.restore()
        : await review.start(photos.query, (message) =>
            cullConfirmation!.confirm(message, "Replace"),
          );
      if (captured && review === cull && !culling && !comparing && editor.canChangeTarget())
        photos.culling = true;
    } finally {
      enteringReview = false;
    }
  }
  function compare() {
    if (enteringReview || culling || comparing || !editor.canChangeTarget()) return;
    returnFocus = document.activeElement as HTMLElement;
    const ids = [...photos.selectedIDs];
    if (ids.length >= 2 && ids.length <= maxComparePhotos) {
      photos.compareIDs = ids;
      photos.comparing = true;
    }
  }
  async function closeReview() {
    photos.culling = photos.comparing = false;
    photos.compareIDs = [];
    photos.compareView = { ...fittedPhotoView };
    photos.compareCrop = undefined;
    photos.cullEditor?.dispose();
    photos.cullEditor = undefined;
    await cull?.flush();
    await refresh();
    await editor.reload();
    await tick();
    if (returnFocus?.isConnected) returnFocus.focus({ preventScroll: true });
  }
  const album = $derived(albums?.items.find((item) => item.id === albumID));
  const editor = $derived(photos.authored);
  let editing = $state(false);
  let loupeMetadata = $state(false);
  const selectionCount = $derived(photos.selectedIDs.size);
  const actionDisabled = $derived(photos.loading || photos.hiding || photos.trashing);
  const narrow = new MediaQuery(MEDIA.compact);
  const cullDisabled = $derived(
    actionDisabled || !cull || cull.capturing || !vaultID || !photos.total,
  );
  const editCount = $derived(
    photoID ? 1 : editor.count || (photos.allResults ? photos.total : photos.selectedIDs.size),
  );
  $effect(() => {
    const id = photoID,
      active = id ? loupeMetadata : editing,
      revision = photos.editScopeRevision;
    untrack(() => {
      void revision;
      if (editor.inspectedID !== id) {
        editor.reset();
        editor.inspectedID = id;
      }
      if (!id) loupeMetadata = false;
      if (
        active &&
        !editor.targets.length &&
        !editor.loading &&
        !editor.concealed &&
        !editor.conflict
      ) {
        const scope = id ? { asset_ids: [id] } : photos.scope();
        if (scope.query || scope.asset_ids?.length)
          void editor.prepare({ ...scope, hidden: photos.hidden });
      }
    });
  });
  $effect(() => {
    editor.loadTags = onloadtags;
    editor.loadMoreTags = onloadmoretags;
    const catalog = tags;
    if (catalog) untrack(() => editor.catalog(catalog));
  });
  $effect(() => {
    editor.tagsError = tagsError;
    editor.tagsLoading = tagsLoading;
    editor.tagsTotal = tagsTotal;
  });
  async function metadataRefresh(
    receipt: PhotoAuthoredReceipt,
    tags: import("./generated/docbank.js").Tag[],
    refreshList = true,
  ) {
    onmetadatachanged?.(receipt, tags);
    const refreshing = photos.loading;
    if (refreshing) photos.cancelPending();
    const rotations = photos.metadataChanged(receipt);
    const displayFileID = editor.asset?.display_file_id ?? direct?.display_file_id;
    if (direct && rotations.has(direct.display_file_id))
      direct = { ...direct, rotation: rotations.get(direct.display_file_id)! };
    // A lookup that has not bound its display file yet may predate any rotation in this receipt.
    if (
      photoID &&
      detailLoading &&
      (displayFileID ? rotations.has(displayFileID) : rotations.size > 0)
    ) {
      detailController?.abort();
      reload = { id: photoID };
    }
    if (refreshList && (refreshing || (photos.started && photos.reloadsAfter(rotations))))
      await refresh();
  }
  async function suggestionRefresh(
    receipt?: PhotoAuthoredReceipt,
    tags: import("./generated/docbank.js").Tag[] = [],
  ) {
    if (receipt) await metadataRefresh(receipt, tags, false);
    await onloadtags?.();
    await refresh();
  }
  function unavailable() {
    editor.unavailable(
      editor.conflict
        ? "Reload metadata before editing."
        : photos.loading
          ? "Photos are loading. Try again when they finish."
          : photos.hiding || photos.trashing
            ? "A photo change is in progress. Try again when it finishes."
            : undefined,
    );
  }
  function edit(patch: PhotoAuthoredPatch, keywords?: PhotoKeywordEdit[], rotationDelta?: 90) {
    if (!editCount) return;
    if (actionDisabled || editor.conflict || editor.busy || editor.loading || editor.concealed) {
      unavailable();
      return;
    }
    if (photoID && !current) {
      editor.unavailable(
        detailError
          ? "Reload the photo before editing."
          : "Photo is loading. Try again when it finishes.",
      );
      return;
    }
    const scope = photoID ? { asset_ids: [photoID] } : photos.scope();
    const id = photoID;
    void editor
      .edit(scope, patch, photos.hidden, metadataRefresh, {
        keywords,
        rotationDelta,
        viewed: current && {
          content_version_id: current.content_version_id,
        },
      })
      .then(() => {
        if (!id || id !== photoID || !editor.conflict) return;
        reload = { id };
        if (!editor.dirty) void editor.reload(true);
      });
  }
  function undoShortcut() {
    if (!editor.receiptID) return;
    if (actionDisabled || editor.conflict || editor.busy || editor.loading || editor.concealed) {
      unavailable();
      return;
    }
    void editor.undo(metadataRefresh);
  }
  onMount(() => {
    if (!shortcuts || (mapMode && !tiles.tile_url)) return;
    const scope = `photos-${photos.contextID}`;
    const pop = shortcuts.pushScope(scope);
    const bindings: [string, string, () => void][] = [
      ...[0, 1, 2, 3, 4, 5].map(
        (value) =>
          [
            String(value),
            value ? `Rate ${value} star${value === 1 ? "" : "s"}` : "Clear rating",
            () => edit({ rating: value }),
          ] as [string, string, () => void],
      ),
      ...(
        [
          ["p", "pick", "Pick photo"],
          ["x", "reject", "Reject photo"],
          ["u", "", "Clear flag"],
        ] as const
      ).map(
        ([key, flag, description]) =>
          [key, description, () => edit({ flag })] as [string, string, () => void],
      ),
      ...(["red", "yellow", "green", "blue"] as const).map(
        (label, index) =>
          [String(index + 6), `Set ${label} label`, () => edit({ label })] as [
            string,
            string,
            () => void,
          ],
      ),
      ["v", "Set purple label", () => edit({ label: "purple" })],
      ["r", "Rotate selected photos 90° clockwise", () => edit({}, undefined, 90)],
      ["mod+z", "Undo last photo edit", undoShortcut],
      ...(onhelp
        ? [["shift+/", "Show keyboard shortcuts", onhelp] as [string, string, () => void]]
        : []),
    ];
    const unregister = bindings.map(([key, description, handler]) =>
      shortcuts.register(
        key,
        () => {
          if (!culling && !comparing) handler();
        },
        { scope, description, allowInInput: true },
      ),
    );
    return () => {
      unregister.forEach((remove) => remove());
      pop();
    };
  });
  let alive = true;
  onDestroy(() => (alive = false));
  let picker = $state<{ focus: () => void; addToTarget: () => Promise<boolean> }>();
  let renaming = $state(false);
  let renameAlbum = $state<PhotoAlbumItem>();
  let renameError = $state("");
  let name = $state("");
  let modal = $state<"delete" | "duplicate" | undefined>();
  let modalError = $state("");
  let modalAlbum = $state<PhotoAlbumItem>();
  let duplicateName = $state("");
  let renameInput = $state<HTMLInputElement>();
  const albumSortOptions = [
    { value: "added_time", label: "Added" },
    { value: "capture_time", label: "Captured" },
    { value: "import_time", label: "Imported" },
  ];
  let map = $state<PhotoMap>();
  let visible = $state.raw<PhotoMapRow[]>([]);
  let mobileTab = $state("map");
  const shown = $derived(mapMode ? visible : photos.items);
  const shownIDs = $derived(new Set(shown.map((item) => item.asset_id)));
  let trashOpen = $state(false);
  let direct = $state<PhotoBrowseRow>();
  let detailError = $state("");
  let detailLoading = $state(false);
  let reload = $state.raw<{ id: string }>();
  let consumedReload = $state.raw<typeof reload>();
  let reloadedRow: Photos["items"][number] | undefined;
  let reloadRevision = $state(0);
  let detailController: AbortController | undefined;
  // An explicit open counts once the viewer shows that photo. Stepping to another
  // photo or closing the viewer first stays silent.
  let openIntent = $state("");
  let visitController: AbortController | undefined;
  const fromGrid = $derived(photos.viewerReturn?.contextID === photos.contextID);
  const listed = $derived(photos.items.some((item) => item.asset_id === photoID));
  const membershipNote = $derived(
    photoID && fromGrid && !listed && !photos.loading
      ? "This photo is no longer in the refreshed result."
      : "",
  );
  let localReturn: PhotoReturn | undefined;
  let openingElement: HTMLElement | undefined;
  let openingID = "";
  let restorePosition: (() => Promise<void>) | undefined;
  $effect(() => {
    if (!photoID) return;
    const controller = new AbortController();
    visitController = controller;
    return () => controller.abort();
  });
  $effect(() => {
    const id = photoID;
    if (!id) {
      direct = undefined;
      detailError = "";
      if (fromGrid) void restoreGrid();
      return;
    }
    const controller = new AbortController();
    detailController = controller;
    const explicit =
      reload?.id === id &&
      reload !== (photos.projection ? consumedReload : untrack(() => consumedReload));
    // Map list refreshes revalidate compact rows; explicit reloads update the list themselves.
    if (photos.projection && !explicit && fromGrid) void listedRow;
    if (
      photos.projection &&
      !explicit &&
      reload?.id === id &&
      reload === consumedReload &&
      untrack(() => direct?.asset_id === id && (!fromGrid || listedRow === reloadedRow))
    ) {
      detailLoading = false;
      return () => controller.abort();
    }
    if (!explicit) {
      if (!photos.projection || untrack(() => direct?.asset_id) !== id) direct = undefined;
      detailError = "";
    }
    if (!explicit && fromGrid && listedDetail) {
      detailLoading = false;
      return () => controller.abort();
    }
    detailLoading = true;
    const request = reload;
    const version = untrack(() => current?.content_version_id);
    const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(30_000)]);
    void (photos.projection && explicit ? photos.reload(id, signal) : photos.lookup(id, signal))
      .then((photo) => {
        if (!signal.aborted) {
          if (!photos.projection)
            photos.items = photos.items.map((item) =>
              item.asset_id === id
                ? { ...photo, ...("stack" in item ? { stack: item.stack } : {}) }
                : item,
            );
          direct = photo;
          detailError = "";
          if (explicit) {
            if (!photos.projection || photo.content_version_id === version) reloadRevision++;
            reloadedRow = untrack(() => listedRow);
            consumedReload = request;
          }
        }
      })
      .catch((cause) => {
        if (!controller.signal.aborted)
          detailError = cause instanceof Error ? cause.message : String(cause);
      })
      .finally(() => {
        if (!controller.signal.aborted) detailLoading = false;
      });
    return () => {
      controller.abort();
      if (explicit) consumedReload = request;
    };
  });
  async function restoreGrid() {
    const record = photos.viewerReturn!;
    await tick();
    if (photoID || photos.viewerReturn !== record) return;
    if (record === localReturn) await restorePosition?.();
    await tick();
    if (record.control === "marker") {
      map?.focus(record.assetID);
      photos.viewerReturn = undefined;
      clearOpening();
      return;
    }
    const control =
      record.control === "check"
        ? "input[type=checkbox]"
        : record.control === "open"
          ? ".open-photo"
          : ".photo-image";
    const target =
      record === localReturn && openingElement?.isConnected
        ? openingElement
        : document.querySelector<HTMLElement>(`[data-asset="${record.assetID}"] ${control}`);
    target?.focus({ preventScroll: true });
    if (!photoID && photos.viewerReturn === record) photos.viewerReturn = undefined;
    clearOpening();
  }
  function clearOpening() {
    openingElement = undefined;
    openingID = "";
    restorePosition = undefined;
    localReturn = undefined;
  }
  export function captureReturn(element: HTMLElement = document.activeElement as HTMLElement) {
    openingElement = element;
    openingID =
      element.dataset.mapAsset ?? element.closest<HTMLElement>("[data-asset]")?.dataset.asset ?? "";
    restorePosition = preserve();
    photos.viewerReturn = {
      contextID: photos.contextID,
      assetID: openingID,
      control: element.dataset.mapAsset
        ? "marker"
        : element.matches(".open-photo")
          ? "open"
          : element.matches("input[type=checkbox]")
            ? "check"
            : "image",
      scrollTop: photos.scrollTop,
    };
    localReturn = photos.viewerReturn;
  }
  function open(id: string, element: HTMLElement) {
    if (!editor.canChangeTarget()) return;
    captureReturn(element);
    if (onphotochange(id, "open")) openIntent = id;
  }
  async function move(direction: -1 | 1) {
    if (!editor.canChangeTarget()) return;
    const controller = visitController;
    if (!fromGrid || !current || !controller) return;
    const id = current.asset_id;
    const index = ordered.findIndex((item) => item.asset_id === id);
    if (index < 0) return;
    if (
      photos.cursor &&
      (!mapMode || photos.items.length < mapPhotoLimit) &&
      direction === 1 &&
      index === ordered.length - 1
    ) {
      await photos.loadMore(preserve);
    }
    if (controller.signal.aborted || photoID !== id) return;
    const currentIndex = ordered.findIndex((item) => item.asset_id === id);
    if (currentIndex < 0) return;
    const next = ordered[currentIndex + direction];
    if (next) onphotochange(next.asset_id, "step");
  }
  async function retryNavigation() {
    await photos.retry(preserve);
  }
  function select(id: string, event: MouseEvent, touch: boolean) {
    if (touch) {
      if (photos.selectedIDs.size) photos.check(id, !photos.selectedIDs.has(id), false, orderedIDs);
      else open(id, event.currentTarget as HTMLElement);
    } else photos.select(id, event, orderedIDs);
  }
  let grid = $state<{ preservePosition: () => () => Promise<void> }>();
  let rejectsOpen = $state(false);
  const preserve = () => grid?.preservePosition();
  export function refresh() {
    return photos.refresh(preserve);
  }
  async function setHidden(id: string) {
    if (photos.hiding || photos.trashing) return;
    onactionerror?.("");
    await photos.setHidden(id, preserve, onhidden, onactionerror, oninvalidate);
  }
  const collapsed = $derived(
    shown.filter(
      (row) =>
        !("stack" in row) ||
        !row.stack ||
        photos.expandedStacks.has(row.stack.asset_ids[0]!) ||
        row.asset_id === row.stack.representative_id,
    ),
  );
  const selectedStack = $derived.by(() => {
    if (photos.allResults) return;
    const selected = shown.filter((row) => photos.selectedIDs.has(row.asset_id));
    const row = selected[0];
    if (
      selected.length !== photos.selectedIDs.size ||
      !row ||
      !("stack" in row) ||
      !row.stack ||
      row.stack.asset_ids.length < 2
    )
      return;
    const ids = new Set(row.stack.asset_ids);
    return selected.every((item) => ids.has(item.asset_id)) ? row.stack : undefined;
  });
  // Hold every member's revisions once a stack is selected, so its decision checks what was shown.
  $effect(() => {
    const stack = selectedStack;
    if (
      stack &&
      editor.needsStack(stack.asset_ids, photos.hidden) &&
      !editor.busy &&
      !editor.loading &&
      !editor.conflict
    )
      untrack(() => void editor.prepareStack(stack.asset_ids, photos.hidden));
  });
  async function reloadStack() {
    await refresh();
    if (photos.error) return;
    await editor.reload();
    await tick();
    if (selectedStack) await editor.prepareStack(selectedStack.asset_ids, photos.hidden);
  }

  async function decideStack(decision: "all" | "selected" | "representative") {
    if (!selectedStack || editor.busy || editor.loading || editor.stackLoading) return;
    const members = new Set(selectedStack.asset_ids);
    await editor.decideStack(
      {
        hidden: photos.hidden,
        asset_ids: selectedStack.asset_ids,
        burst_gap_seconds: photos.burstGapSeconds,
        decision,
        current_id: [...photos.selectedIDs][0]!,
        selected_ids: [...photos.selectedIDs],
      },
      metadataRefresh,
      {
        observed: shown.filter(
          (row): row is PhotoBrowseRow => "display_file_id" in row && members.has(row.asset_id),
        ),
      },
    );
  }
  const groups = $derived(
    groupPhotos(
      collapsed,
      photos.query.sort?.field === "relevance"
        ? "relevance"
        : albumID && photos.query.sort?.field !== "capture_time"
          ? "flat"
          : photos.grouping,
    ),
  );
  const searchable = $derived(searchablePhotoView(albumID, mapMode));
  const activeFilters = $derived(activeFilterCount(photos.baseQuery.filters));
  const filtered = $derived(
    searchable &&
      (Boolean(photos.baseQuery.text.trim()) ||
        Object.values(photos.baseQuery.filters ?? {}).some(isFilterSet)),
  );
  const ordered = $derived(photos.items);
  const outsideSelection = $derived(
    mapMode ? [...photos.selectedIDs].filter((id) => !shownIDs.has(id)).length : 0,
  );
  const listedRow = $derived(photos.items.find((item) => item.asset_id === photoID));
  const listedDetail = $derived(!!listedRow && "content_version_id" in listedRow);
  const current = $derived(
    photoID
      ? fromGrid && listedRow && "content_version_id" in listedRow
        ? listedRow
        : direct
      : undefined,
  );
  $effect(() => {
    if (!openIntent) return;
    if (photoID && photoID !== openIntent) openIntent = "";
    else if (current?.asset_id === openIntent && !detailLoading && !detailError) {
      openIntent = "";
      reportAction(photos.session, "document_opened");
    }
  });
  const exportSelection = $derived(photos.scope());
  const exportSelectionCount = $derived(exportSelection.asset_ids?.length ?? photos.total);
  function exportPhotos(selected = false): void {
    const ids = selected ? (exportSelection.asset_ids ?? []) : [];
    onexport?.({
      label: selected ? "Selected photos" : title,
      photos: { query: photos.query, asset_ids: ids, hidden: photos.hidden },
      total: selected ? exportSelectionCount : photos.total,
    });
  }
  const orderedIDs = $derived(
    mapMode
      ? shown.map((item) => item.asset_id)
      : groups.flatMap((group) => group.items.map((item) => item.asset_id)),
  );
  const dateLabel = $derived(photos.date?.length === 7 ? monthLabel(photos.date) : photos.date);
  const count = $derived(mapMode ? shown.length : photos.total);
  const photoCount = $derived(
    !photos.started
      ? photos.loading
        ? "Loading…"
        : ""
      : `${count.toLocaleString()} photo${count === 1 ? "" : "s"}`,
  );
  const filtersID = $props.id();
  const photoDate = $derived(
    !mapMode && photos.started && photos.date
      ? ` ${photos.date.length === 10 ? "on" : "in"} ${dateLabel}`
      : "",
  );
  const densityOptions = [
    { value: "compact", label: "Compact" },
    { value: "comfortable", label: "Comfortable" },
    { value: "large", label: "Large" },
  ];
  const sortOptions = $derived([
    { value: "capture_time", label: "Capture date" },
    ...(photos.relevanceAvailable || photos.query.sort.field === "relevance"
      ? [{ value: "relevance", label: "Relevance" }]
      : []),
  ]);
  const groupingOptions = [
    { value: "months", label: "Months" },
    { value: "sessions", label: "Capture sessions" },
  ];

  onMount(() => {
    if (!mapMode || tiles.tile_url) {
      void photos.resume(preserve);
    }
    return () => {
      photos.cancelPending();
      openingElement = undefined;
      restorePosition = undefined;
      localReturn = undefined;
    };
  });
  function relayout(change: () => void) {
    const restore = preserve();
    change();
    void restore?.();
  }
  function escape(event: KeyboardEvent) {
    if (mapMode && !tiles.tile_url) return;
    if (
      !photoID &&
      event.key === "Escape" &&
      !isAppShortcutSuppressed(event, false, document, ".photo-cell")
    )
      photos.clearSelection();
    if (
      albums &&
      !photoID &&
      event.key.toLowerCase() === "b" &&
      !event.repeat &&
      !event.ctrlKey &&
      !event.metaKey &&
      !event.altKey &&
      !event.shiftKey &&
      !isAppShortcutSuppressed(event, !photos.selectedIDs.size, document, ".photo-cell")
    ) {
      event.preventDefault();
      if (albums.rejectBusy()) return;
      if (albums.targetID) void picker?.addToTarget();
      else picker?.focus();
    }
  }
  async function refreshView() {
    await Promise.all([refresh(), albums?.load()]);
    // An open inspector can be showing a description that finished since it loaded.
    await photos.authored.reload();
  }
  async function remove() {
    if (!albums || !album) return;
    await albums.members(album, photos.scope(), true);
  }
  async function rename() {
    if (!albums || !renameAlbum || albums.busy || !name.trim()) return;
    const inspected = renameAlbum;
    renameError = "";
    const result = await albums.update(inspected, { name: name.trim() });
    if (!alive || !renaming || renameAlbum !== inspected) return;
    if (result) renaming = false;
    else {
      renameError = albums.error;
      albums.error = "";
      if (!albums.loadError)
        renameAlbum = albums.items.find((item) => item.id === inspected.id) ?? inspected;
    }
  }
  function beginRename() {
    renameError = "";
    renameAlbum = album;
    name = album?.name ?? "";
    renaming = true;
  }
  function openModal(kind: "delete" | "duplicate") {
    modalError = "";
    modalAlbum = album;
    modal = kind;
    duplicateName = `${album?.name ?? "Album"} copy`;
  }
  async function confirm() {
    if (
      !albums ||
      !modal ||
      !modalAlbum ||
      albums.busy ||
      (modal === "duplicate" && albums.unconfirmed)
    )
      return;
    modalError = "";
    const inspected = modalAlbum;
    const result =
      modal === "delete"
        ? await albums.delete(inspected)
        : await albums.duplicate(inspected, duplicateName);
    if (!alive || !modal) return;
    if (!result) {
      modalError = albums.error;
      albums.error = "";
      if (!albums.loadError)
        modalAlbum = albums.items.find((item) => item.id === inspected.id) ?? inspected;
    }
    if (result) {
      const deleting = modal === "delete";
      modal = undefined;
      onnavigate(deleting ? "/photos/albums" : `/photos/albums/${result.id}`);
    }
  }
  $effect(() => {
    if (renaming && renameInput) {
      renameInput.focus();
      renameInput.select();
    }
  });
</script>

{#snippet sourceSummary(source: PhotoAlbumItem)}
  <span
    >{source.name}{#if !albums?.loadError && source.included_count !== undefined}{" · "}{source.included_count.toLocaleString()}
      {source.included_count === 1 ? "photo" : "photos"}{/if}</span
  >
{/snippet}

<svelte:window
  onkeydown={escape}
  onkeydowncapture={(event) => {
    if ((culling || comparing) && (event.ctrlKey || event.metaKey || event.altKey))
      event.stopPropagation();
  }}
/>
<PhotoCullConfirmation bind:this={cullConfirmation} />
{#if culling && cull}<PhotoCull
    {cull}
    {photos}
    {cache}
    {onmetadatachanged}
    onclose={() => void closeReview()}
  />{/if}
{#if comparing}<PhotoCompare
    ids={photos.compareIDs}
    {photos}
    {cache}
    onclose={() => void closeReview()}
  />{/if}
{#snippet vaultReason()}Vault identity unavailable. Reload to retry.{/snippet}
<main class="photos-workspace" aria-label={albumID ? "Photo album" : "Photo library"}>
  {#if mapMode && !tiles.tile_url}
    <EmptyState
      title="Maps are disabled"
      description="Configure [web.map_tiles] in config.toml and restart Docbank."
    />
  {:else}
    {#if mapMode}
      <div class="map-disclosure">
        {tiles.host} receives Docbank's IP address and viewed map areas.
      </div>
      {#if photos.started}<div class="map-controls">
          <span
            >{photos.total.toLocaleString()} located photo{photos.total === 1
              ? ""
              : "s"}{photos.date
              ? ` ${photos.date.length === 10 ? "on" : "in"} ${dateLabel}`
              : ""}</span
          >
        </div>{/if}
      {#if photos.mapCapped}<p class="photo-search-limit" role="status">
          Showing the newest {photos.items.length.toLocaleString()} of {photos.total.toLocaleString()}
          located photos. Narrow your search or date to see the rest.
        </p>{/if}
      <div class="map-tabs">
        <Button
          size="sm"
          surface={mobileTab === "map" ? "solid" : "outline"}
          onclick={() => (mobileTab = "map")}>Map</Button
        ><Button
          size="sm"
          surface={mobileTab === "photos" ? "solid" : "outline"}
          onclick={() => (mobileTab = "photos")}>Photos</Button
        >
      </div>
    {/if}
    {#if mapMode && photos.loading}<div class="photo-loading" role="status">
        <Spinner size={14} />Loading located photos: {photos.items.length.toLocaleString()} of {Math.min(
          photos.total,
          mapPhotoLimit,
        ).toLocaleString()}
      </div>{/if}
    {#if mapMode && photos.error}
      <div class="photo-error" role="alert">
        <span>{photos.error}</span><Button size="sm" onclick={() => void retryNavigation()}
          >Retry</Button
        >
      </div>
    {/if}
    <div
      class:map-layout={mapMode}
      class:show-photos={mobileTab === "photos"}
      class="photo-layout"
      inert={!!photoID || culling || comparing}
    >
      {#snippet libraryGrid()}
        <div class="library-grid">
          <div class="photo-bar browser-toolbar">
            <div class="photo-crumbs">
              {#if searchable}<IconButton
                  size="sm"
                  title="Filters"
                  tone={photos.filtersOpen ? "info" : "neutral"}
                  ariaLabel={activeFilters
                    ? `Filters, ${activeFilters} active filter${activeFilters === 1 ? "" : "s"}`
                    : "Filters"}
                  ariaExpanded={photos.filtersOpen}
                  ariaControls={filtersID}
                  onclick={() => photos.toggleFilters()}
                >
                  <SlidersHorizontalIcon size="14" aria-hidden="true" />
                  {#if activeFilters}<Chip size="xs" tone="info">{activeFilters}</Chip>{/if}
                </IconButton>{/if}
              {#if albumID}<button
                  type="button"
                  class="album-back"
                  onclick={() => onnavigate("/photos/albums")}>Albums</button
                ><span aria-hidden="true">›</span>{/if}
              {#if renaming && renameAlbum}<form
                  onsubmit={(event) => {
                    event.preventDefault();
                    void rename();
                  }}
                >
                  {@render sourceSummary(renameAlbum)}<TextInput
                    ariaLabel="Album name"
                    bind:value={name}
                    bind:inputEl={renameInput}
                    onkeydown={(event) => {
                      if (event.key === "Escape") {
                        event.preventDefault();
                        renaming = false;
                      }
                    }}
                  /><Button type="submit" size="sm" disabled={albums?.busy || !name.trim()}
                    >Save</Button
                  ><Button size="sm" onclick={() => (renaming = false)}>Cancel</Button
                  >{#if renameError}<span role="alert">{renameError}</span>{/if}
                </form>
              {:else}<h1>
                  {mapMode ? "Photos in view" : albumID ? (album?.name ?? "Album") : title}
                </h1>{/if}
              {#if filtered}<Button
                  size="sm"
                  disabled={actionDisabled}
                  onclick={() => void photos.resetQuery()}>Clear filters</Button
                >
              {/if}
              {#if searchable && albums}<PhotoSmartSet {photos} {albums} {onnavigate} />{/if}
              {#if cull?.progress}<span role="status">{cull.progress}</span>{/if}
              {#if cull?.error}<span role="alert">{cull.error}</span>{/if}
            </div>
            <div class="toolbar-actions">
              <span title={`${photoCount}${photoDate}`}>
                <span class="photo-count-value">{photoCount}</span>{#if photoDate}<span
                    class="photo-count-date">{photoDate}</span
                  >{/if}
              </span>
              {#if albumID}<SelectDropdown
                  title="Sort photos"
                  value={photos.query.sort?.field ?? "added_time"}
                  options={albumSortOptions}
                  onchange={(value) =>
                    void photos.setSort({
                      field: value as "added_time" | "capture_time" | "import_time",
                      direction: "desc",
                    })}
                />
              {:else if searchable}<SelectDropdown
                  title="Sort photos"
                  disabled={actionDisabled}
                  value={photos.query.sort?.field}
                  options={sortOptions}
                  onchange={(value) =>
                    void photos.setSort({
                      field: value as "capture_time" | "relevance",
                      direction: "desc",
                    })}
                />{/if}
              {#if !mapMode && !photos.hidden}<nav class="photo-views" aria-label="Photo views">
                  <button
                    type="button"
                    class="photo-toggle kit-button kit-control-states kit-button--sm"
                    aria-pressed={photos.view === "grid"}
                    onclick={() => photos.setView("grid")}>Grid</button
                  ><button
                    type="button"
                    class="photo-toggle kit-button kit-control-states kit-button--sm"
                    aria-pressed={photos.view === "timeline"}
                    onclick={() => photos.setView("timeline")}>Timeline</button
                  >
                </nav>{/if}
              {#if !narrow.current && (!albumID || photos.query.sort?.field === "capture_time")}<SelectDropdown
                  title="Group photos"
                  disabled={photos.query.sort?.field !== "capture_time"}
                  value={photos.grouping}
                  options={groupingOptions}
                  onchange={(value) =>
                    relayout(() => (photos.grouping = value as "months" | "sessions"))}
                />{/if}
              {#if !narrow.current}<SelectDropdown
                  title="Grid density"
                  value={photos.density}
                  options={densityOptions}
                  onchange={(value) => relayout(() => photos.setDensity(value as Density))}
                />{/if}
              {#if album && albums}<IconButton
                  size="sm"
                  ariaLabel={album.starred ? "Unstar album" : "Star album"}
                  title={album.starred ? "Unstar album" : "Star album"}
                  ariaPressed={album.starred}
                  disabled={albums.busy || renaming}
                  onclick={() => void albums!.update(album!, { starred: !album!.starred })}
                  ><StarIcon size="14" fill={album.starred ? "currentColor" : "none"} /></IconButton
                >{/if}
              {#if !mapMode && !photos.hidden}
                {#if cull?.capturing}<Button size="sm" onclick={() => cull?.cancel()}
                    >Cancel capture</Button
                  >{/if}
                {#if !narrow.current}<Button
                    size="sm"
                    disabled={cullDisabled}
                    title={vaultID ? undefined : "Vault identity unavailable. Reload to retry."}
                    onclick={() => void startCull()}>Cull</Button
                  >
                  {#if cull?.saved}<Button
                      size="sm"
                      disabled={actionDisabled || cull.capturing}
                      onclick={() => void startCull(true)}>Resume cull</Button
                    >{/if}
                {/if}
              {/if}
              <Menu align="end">
                <MenuTrigger ariaLabel="More photo actions"><EllipsisIcon size="16" /></MenuTrigger>
                <MenuContent ariaLabel="More photo actions">
                  {#if narrow.current}
                    {#if !mapMode && !photos.hidden}<MenuItem
                        disabled={cullDisabled}
                        trailing={vaultID ? undefined : vaultReason}
                        onselect={() => void startCull()}>Cull</MenuItem
                      >
                      {#if cull?.saved}<MenuItem
                          disabled={actionDisabled || cull.capturing}
                          onselect={() => void startCull(true)}>Resume cull</MenuItem
                        >{/if}
                    {/if}
                    {#if !albumID || photos.query.sort?.field === "capture_time"}<MenuRadioGroup
                        ariaLabel="Group photos"
                        value={photos.grouping}
                        onchange={(value) =>
                          relayout(() => (photos.grouping = value as "months" | "sessions"))}
                        >{#each groupingOptions as option (option.value)}<MenuRadioItem
                            value={option.value}
                            disabled={photos.query.sort?.field !== "capture_time"}
                            >{option.label}</MenuRadioItem
                          >{/each}</MenuRadioGroup
                      >{/if}
                    <MenuRadioGroup
                      ariaLabel="Grid density"
                      value={photos.density}
                      onchange={(value) => relayout(() => photos.setDensity(value as Density))}
                      >{#each densityOptions as option (option.value)}<MenuRadioItem
                          value={option.value}>{option.label}</MenuRadioItem
                        >{/each}</MenuRadioGroup
                    >
                    <MenuSeparator />
                  {/if}
                  {#if !mapMode}
                    <MenuItem
                      disabled={actionDisabled || photos.rejectsLoading}
                      onselect={() => {
                        if (!editor.canChangeTarget()) return;
                        rejectsOpen = true;
                        void photos.previewRejects();
                      }}>Move rejects</MenuItem
                    >
                    <MenuItem
                      disabled={photos.loading ||
                        photos.total === 0 ||
                        photos.total > maxPhotoExportMembers}
                      onselect={() => exportPhotos()}
                      >{photos.total > maxPhotoExportMembers
                        ? `Export all (limit ${maxPhotoExportMembers} photos)`
                        : photos.total === 1
                          ? "Export 1 photo"
                          : `Export all ${photos.total.toLocaleString()} photos`}</MenuItem
                    >
                  {/if}
                  <MenuItem
                    disabled={actionDisabled || albums?.busy}
                    onselect={() => void refreshView()}>Refresh previews</MenuItem
                  >
                  {#if album && albums}
                    <MenuItem disabled={albums.busy || renaming} onselect={beginRename}
                      >Rename album</MenuItem
                    >
                    <MenuItem
                      disabled={albums.busy || renaming}
                      onselect={() => openModal("duplicate")}>Duplicate album…</MenuItem
                    >
                    <MenuItem
                      tone="danger"
                      disabled={albums.busy || renaming}
                      onselect={() => openModal("delete")}>Delete album…</MenuItem
                    >
                  {/if}
                </MenuContent>
              </Menu>
              {#if !mapMode && !photos.hidden && !albumID}<Button
                  size="sm"
                  tone="info"
                  surface="solid"
                  disabled={!onadd}
                  title={uploadChannelError || undefined}
                  onclick={onadd}><PlusIcon size="14" aria-hidden="true" />Add photos</Button
                >{/if}
            </div>
          </div>
          {#if !mapMode}<div class="photo-stack-controls browser-toolbar">
              {#if photos.groupingIncomplete}<p role="status">
                  Some camera details are missing or outdated. Stacks can change once they update.
                </p>{/if}
              <label class="burst-gap"
                >Burst gap (seconds) <input
                  aria-label="Burst gap (seconds)"
                  type="number"
                  min="1"
                  max="30"
                  value={photos.burstGapSeconds}
                  disabled={photos.loading || editor.busy}
                  onchange={async (event) => {
                    const input = event.currentTarget;
                    await photos.setBurstGap(input.valueAsNumber, preserve);
                    input.value = String(photos.burstGapSeconds);
                    onactionerror?.(photos.actionError);
                  }}
                /></label
              >
            </div>{/if}
          {#if photos.total > maxPhotoExportMembers}<p class="photo-export-limit">
              Select up to {maxPhotoExportMembers} photos to export.
            </p>{/if}
          {#if photos.actionError && !onactionerror}<p role="alert">{photos.actionError}</p>{/if}
          {#if !mapMode && photos.date}
            <div class="photo-date-selection">
              <Button size="sm" onclick={() => void photos.selectDate()}>Clear date</Button>
            </div>
          {/if}
          {#if !mapMode && photos.view === "timeline"}
            <PhotoTimeline
              facet={photos.timeline}
              loading={photos.timelineLoading}
              error={photos.timelineError}
              selected={photos.date}
              onselect={(date) => void photos.selectDate(date)}
              onretry={() => void photos.loadTimeline()}
            />
          {/if}
          {#if albums && modal !== "duplicate"}<PhotoAlbumPending {albums} {onnavigate} />{/if}
          {#if albums?.error}<div class="photo-error" role="alert">{albums.error}</div>{/if}
          {#if albums?.notice}<div class="photo-notice" role="status">
              {albums.notice}{#if albums.noticeID}<a
                  href={`/photos/albums/${albums.noticeID}`}
                  onclick={(event) => {
                    event.preventDefault();
                    onnavigate(`/photos/albums/${albums!.noticeID}`);
                  }}>Open album</a
                >{/if}
            </div>{/if}
          {#if photos.query.sort?.field === "relevance" && photos.total > photos.items.length}
            <p class="photo-search-limit">
              Showing the best 250 of {photos.total.toLocaleString()} matches. Refine the search or sort
              by capture date to see all.
            </p>
          {/if}
          {#if !mapMode && photos.error}
            <div class="photo-error" role="alert">
              <span>{photos.error}</span><Button
                size="sm"
                onclick={() => void photos.retry(preserve)}>Retry</Button
              >
            </div>
          {/if}
          <div class="photo-browser">
            {#if searchable && photos.filtersOpen}
              <div id={filtersID} class="photo-facets">
                {#if photos.facetsLoading}<div class="photo-loading" role="status">
                    <Spinner size={14} />Loading counts…
                  </div>{/if}
                {#if photos.facetsError}<div class="photo-error" role="alert">
                    <span>{photos.facetsError}</span>{#if photos.facetsRetryable}<Button
                        size="sm"
                        onclick={() => void photos.retryFacets()}>Retry counts</Button
                      >{/if}
                  </div>{/if}
                <FacetSidebar
                  disabled={photos.loading || photos.hiding || photos.trashing}
                  facets={photos.facets}
                  query={photos.baseQuery}
                  unit="photos"
                  onchange={(value) => void photos.setQuery(value)}
                />
              </div>
            {/if}
            <div class="photo-results">
              {#if !photoID && editor.error}<Notice tone="error" message={editor.error} />{/if}
              {#if !photoID && editor.receiptID}<Notice
                  tone="success"
                  message={editor.undoState.summary}
                  actionLabel="Undo last photo edit"
                  onaction={() => void editor.undo(metadataRefresh)}
                />{/if}
              {#if !photoID && editor.notice}<Notice
                  tone={editor.noticeTone}
                  message={editor.notice}
                />{/if}
              {#if albumID && albums && albums.initialized && !albums.loading && !albums.busy && modal !== "delete" && !albums.loadError && !album}<EmptyState
                  title="Album not found"
                  ><Button onclick={() => onnavigate("/photos/albums")}>Back to albums</Button
                  ></EmptyState
                >
              {:else if shown.length}
                {#key photos.baseQuery}
                  <PhotoGrid
                    bind:this={grid}
                    bind:scrollTop={photos.scrollTop}
                    {groups}
                    expandedStacks={photos.expandedStacks}
                    onexpand={(id) => photos.toggleStack(id)}
                    targetRowHeight={ROW_HEIGHTS[photos.density]}
                    loading={photos.loading || !!photoID}
                    {cache}
                    hidden={photos.hidden}
                    onhidden={(id) => void setHidden(id)}
                    selectedIDs={photos.selectedIDs}
                    onselect={select}
                    onopen={(id, element) => open(id, element)}
                    oncheck={(id, checked, range) => photos.check(id, checked, range, orderedIDs)}
                    onloadmore={() => {
                      if (!mapMode) void photos.loadMore(preserve);
                    }}
                    ondragstart={albums
                      ? (id, event) => albums?.startDrag(id, event, photos)
                      : undefined}
                    ondragend={albums
                      ? () => {
                          if (albums) albums.drag = undefined;
                        }
                      : undefined}
                  />
                {/key}
              {:else if !(photos.loading || photos.error)}
                <EmptyState
                  title={mapMode
                    ? photos.items.length
                      ? "No photos in this area"
                      : "No located photos"
                    : photos.date
                      ? `No photos ${photos.date.length === 10 ? "on this day" : photos.date.length === 7 ? "in this month" : "in this year"}`
                      : filtered
                        ? "No matching photos"
                        : photos.hidden
                          ? "No hidden photos"
                          : albumID
                            ? "This album is empty"
                            : "Your photo library is empty"}
                  description={mapMode || photos.date
                    ? undefined
                    : filtered
                      ? "Clear a filter or try another search."
                      : photos.hidden
                        ? "Choose Hide from a photo's actions menu in Library."
                        : albumID
                          ? "Go to Library, select photos, then choose Add to album or press B."
                          : "Choose a folder or files with Add photos. Large camera cards on the daemon's own disk import faster with docbank photos import."}
                >
                  {#snippet icon()}<ImageIcon size="24" />{/snippet}
                  {#if !mapMode && !photos.hidden && !albumID && !filtered && !photos.date}<Button
                      disabled={!onadd}
                      title={uploadChannelError || undefined}
                      onclick={onadd}>Add photos</Button
                    >{/if}
                  {#if albumID && !photos.date}<Button onclick={() => onnavigate("/photos")}
                      >Go to Library</Button
                    >{/if}
                </EmptyState>
              {/if}
              <div role="status">
                {#if !mapMode && photos.loading}<div class="photo-pill kit-popover-card">
                    <Spinner size={14} />Loading photos…
                  </div>{/if}
              </div>
              {#if !mapMode && photos.cursor && !photos.error}<div class="photo-loading">
                  {#if !photos.loading}<Button
                      size="sm"
                      onclick={() => void photos.loadMore(preserve)}>Load more</Button
                    >{/if}
                </div>{/if}
            </div>
            {#if editing && (editCount || editor.dirty)}<aside class="grid-inspector">
                <PhotoInspector
                  {editor}
                  {actionDisabled}
                  count={editCount}
                  onedit={edit}
                  onresponse={suggestionRefresh}
                />
              </aside>{/if}
          </div>
          {#if !photoID}
            <SelectionDock
              exportDisabled={exportSelectionCount === 0 ||
                exportSelectionCount > maxPhotoExportMembers}
              exportHelp={exportSelectionCount > maxPhotoExportMembers
                ? `Select up to ${maxPhotoExportMembers} photos to export. Clear the selection to choose fewer photos.`
                : ""}
              onexport={() => exportPhotos(true)}
              retainOpen={editor.dirty}
              context="photos"
              selectVisible={mapMode}
              outsideCount={outsideSelection}
              selectedCount={photos.selectedIDs.size}
              visibleDocumentCount={shown.length}
              loadedSelected={shown.every((item) => photos.selectedIDs.has(item.asset_id))}
              wholeQueryCount={photos.allResults && editor.count ? editor.count : photos.total}
              allResults={photos.allResults}
              onallresults={mapMode ? undefined : () => photos.selectAllResults()}
              onclear={() => photos.clearSelection()}
              onselectvisible={() => photos.selectLoaded(mapMode ? orderedIDs : undefined)}
              ontrash={() => {
                photos.trashError = "";
                trashOpen = true;
              }}
              trashDisabled={!photos.selectedIDs.size ||
                photos.trashing ||
                photos.hiding ||
                (!mapMode && (photos.loading || (photos.allResults && !editor.targets.length)))}
            >
              {#snippet photoActions()}
                {#if !mapMode && !photos.hidden}<Button
                    size="sm"
                    disabled={selectionCount < 2 ||
                      selectionCount > maxComparePhotos ||
                      photos.allResults}
                    title={`Select 2–${maxComparePhotos} photos to compare`}
                    onclick={compare}>Compare</Button
                  >{#if selectionCount > maxComparePhotos}<span
                      >Select 2–{maxComparePhotos} photos to compare.</span
                    >{/if}{/if}
                {#if selectedStack}
                  <Button
                    size="sm"
                    disabled={photos.loading ||
                      editor.busy ||
                      editor.loading ||
                      editor.stackLoading ||
                      editor.conflict}
                    onclick={() => void decideStack("all")}>Keep all</Button
                  >
                  <Button
                    size="sm"
                    disabled={photos.loading ||
                      editor.busy ||
                      editor.loading ||
                      editor.stackLoading ||
                      editor.conflict}
                    onclick={() => void decideStack("selected")}>Keep selected</Button
                  >
                  {#if photos.selectedIDs.size === 1}<Button
                      size="sm"
                      disabled={photos.loading ||
                        editor.busy ||
                        editor.loading ||
                        editor.stackLoading ||
                        editor.conflict ||
                        !editor.canChooseStackRepresentative([...photos.selectedIDs][0]!)}
                      onclick={() => void decideStack("representative")}
                      >Choose representative</Button
                    >{/if}
                  {#if editor.conflict}<Button
                      size="sm"
                      disabled={photos.loading || editor.busy || editor.loading}
                      onclick={() => void reloadStack()}>Reload stack</Button
                    >
                  {:else if editor.stackError}<Button
                      size="sm"
                      disabled={editor.stackLoading}
                      onclick={() =>
                        void editor.prepareStack(selectedStack!.asset_ids, photos.hidden, true)}
                      >Retry</Button
                    >{/if}
                  {#if editor.error}<span role="alert">{editor.error}</span>{/if}
                {/if}
                <Button
                  size="sm"
                  disabled={!editCount && !editor.dirty}
                  onclick={() => (editing = !editing)}>Metadata</Button
                >
                {#if albums}<PhotoAlbumPicker bind:this={picker} {photos} {albums} />{/if}
                {#if album && albums}<Button
                    size="sm"
                    disabled={!photos.selectedIDs.size ||
                      albums.busy ||
                      (!photos.allResults && photos.selectedIDs.size > 1000)}
                    onclick={() => void remove()}>Remove from album</Button
                  ><Button
                    size="sm"
                    disabled={albums.busy || photos.allResults || photos.selectedIDs.size !== 1}
                    onclick={() => void albums!.cover(album!, [...photos.selectedIDs][0])}
                    >Use as cover</Button
                  >{/if}
              {/snippet}
            </SelectionDock>
          {/if}
        </div>
      {/snippet}
      {#if mapMode}
        {#await import("./PhotoMap.svelte") then { default: PhotoMap }}
          <div class="map-pane">
            <PhotoMap
              bind:this={map}
              items={photos.items as PhotoMapRow[]}
              appendedFrom={photos.appendedFrom}
              {tiles}
              session={photos.session}
              onauthfailure={photos.onauthfailure}
              view={photos.mapView}
              onopen={(id, element) => open(id, element)}
              onvisible={(rows) => {
                visible = rows;
              }}
              onview={(view) => {
                photos.mapView = view;
              }}
            />
          </div>
          {@render libraryGrid()}
        {:catch}<EmptyState
            title="Map couldn't load"
            description="Reload this page to try again."
          />{/await}
      {:else}{@render libraryGrid()}{/if}
    </div>
  {/if}
  {#if photoID}<PhotoLoupe
      bind:metadataOpen={loupeMetadata}
      {editor}
      {actionDisabled}
      onedit={edit}
      onundo={() => void editor.undo(metadataRefresh)}
      onresponse={suggestionRefresh}
      photo={current}
      error={detailError}
      note={membershipNote}
      reloading={detailLoading}
      {reloadRevision}
      onreload={() => (reload = { id: photoID })}
      onretry={retryNavigation}
      {photos}
      {cache}
      items={fromGrid ? ordered : []}
      canLoadMore={fromGrid && (!mapMode || photos.items.length < mapPhotoLimit) && !!photos.cursor}
      onmove={move}
      onchoose={(id) => editor.canChangeTarget() && onphotochange(id, "step")}
      onclose={() => {
        openIntent = "";
        onphotochange("", "close");
      }}
    />{/if}
</main>
{#if modal && modalAlbum && albums}
  <Modal
    title={modal === "delete" ? "Delete album" : "Duplicate album"}
    onclose={() => {
      if (!albums.busy) modal = undefined;
    }}
  >
    {#if modal === "delete"}<p>
        Delete "{modalAlbum.name}"? Its {albums.loadError || modalAlbum.included_count === undefined
          ? ""
          : `${modalAlbum.included_count.toLocaleString()} `}photos stay in your library.
      </p>{:else}{@render sourceSummary(modalAlbum)}<TextInput
        ariaLabel="Copy name"
        bind:value={duplicateName}
      />{/if}
    {#if modalError}<p role="alert">{modalError}</p>{/if}
    {#if modal === "duplicate"}<PhotoAlbumPending
        {albums}
        onnavigate={(path) => {
          modal = undefined;
          onnavigate(path);
        }}
      />{/if}
    <div class="modal-actions">
      <Button disabled={albums.busy} onclick={() => (modal = undefined)}>Cancel</Button><Button
        tone={modal === "delete" ? "danger" : "info"}
        disabled={albums.busy ||
          (modal === "duplicate" && (!!albums.unconfirmed || !duplicateName.trim()))}
        onclick={() => void confirm()}
        >{modal === "delete" ? "Delete album" : "Duplicate album"}</Button
      >
    </div>
  </Modal>
{/if}

{#if rejectsOpen}
  <PhotoRejectsModal
    {photos}
    onclose={() => {
      photos.cancelRejects();
      rejectsOpen = false;
    }}
    onmove={async () => {
      const source = photos;
      if (await source.trashRejects(preserve, () => ontrashed?.(source))) rejectsOpen = false;
    }}
  />
{/if}

{#if trashOpen}
  <Modal
    title="Move selected photos to trash?"
    tone="danger"
    ariaLabel="Move selected photos to trash"
    onclose={() => {
      if (!photos.trashing) trashOpen = false;
    }}
    closeOnOverlayClick={!photos.trashing}
  >
    <p>
      Move {selectionCount} selected photo{selectionCount === 1 ? "" : "s"} and every RAW, image, video,
      and sidecar member to recoverable trash. Stored files and album membership stay intact.
    </p>
    {#if outsideSelection}<p>
        {outsideSelection} selected photo{outsideSelection === 1 ? " is" : "s are"} outside this view.
      </p>{/if}
    {#if photos.trashError}<p role="alert">
        {photos.trashError} Failed photos remain selected for retry.
      </p>{/if}
    {#snippet footer()}
      <Button disabled={photos.trashing} onclick={() => (trashOpen = false)}>Keep in Docbank</Button
      >
      <Button
        tone="danger"
        disabled={photos.trashing || selectionCount === 0}
        onclick={async () => {
          const source = photos;
          if (await source.trashSelected(preserve, () => ontrashed?.(source))) trashOpen = false;
        }}>{photos.trashing ? "Moving…" : "Move to trash"}</Button
      >
    {/snippet}
  </Modal>
{/if}

<style>
  .burst-gap {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    font-size: var(--font-size-sm);
  }
  .burst-gap input {
    width: 4rem;
    background: var(--bg-surface);
    color: var(--text-primary);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    padding: 4px 8px;
  }
  .photo-layout {
    display: flex;
    flex: 1;
    min-height: 0;
  }
  .map-layout .map-pane {
    flex: 0 0 60%;
    min-width: 0;
  }
  .map-layout .library-grid {
    width: 40%;
  }
  .map-disclosure,
  .map-controls {
    padding: 8px 16px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .map-controls {
    display: flex;
    align-items: center;
    gap: 12px;
  }
  .map-tabs {
    display: none;
  }
  @media (max-width: 900px) {
    .map-tabs {
      display: flex;
      gap: 8px;
      padding: 8px;
    }
    .map-layout .map-pane {
      flex-basis: 100%;
    }
    .map-layout .library-grid {
      display: none;
      width: 100%;
    }
    .map-layout.show-photos .map-pane {
      display: none;
    }
    .map-layout.show-photos .library-grid {
      display: flex;
    }
  }
  .library-grid {
    min-width: 0;
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
  }
  .photos-workspace {
    position: relative;
    max-height: calc(100dvh - var(--header-height));
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    overflow: hidden;
    background: var(--bg-surface);
  }
  .photo-search-limit {
    margin: 0;
    padding: var(--space-3) var(--space-5);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .photo-browser {
    display: flex;
    flex: 1;
    min-height: 0;
    overflow: hidden;
  }
  .photo-facets {
    display: flex;
    flex-direction: column;
    width: 230px;
    flex-shrink: 0;
    overflow: auto;
  }
  .photo-facets :global(.facets) {
    flex: 1;
  }
  .grid-inspector {
    width: 280px;
    flex-shrink: 0;
    padding: 16px;
    overflow: auto;
    border-left: 1px solid var(--border-default);
  }
  .photo-results {
    position: relative;
    display: flex;
    flex-direction: column;
    flex: 1;
    min-width: 0;
    min-height: 0;
  }
  @media (max-width: 640px) {
    .photo-crumbs {
      flex-basis: 100%;
    }
    .photo-bar .toolbar-actions {
      flex-basis: 100%;
      flex-wrap: wrap;
    }
    .photo-bar .toolbar-actions > :global(*) {
      flex-shrink: 0;
    }
    .photo-bar .toolbar-actions > span {
      white-space: nowrap;
    }
    .photos-workspace:has(.grid-inspector) {
      overflow: auto;
    }
    .library-grid:has(.grid-inspector) {
      flex: none;
    }
    .photo-browser {
      flex-direction: column;
    }
    .photo-browser:has(.grid-inspector) {
      flex: none;
      overflow: visible;
    }
    .photo-browser:has(.grid-inspector) .photo-results {
      flex: none;
      height: 180px;
      overflow: auto;
    }
    .photo-facets {
      width: auto;
      max-height: 180px;
    }
    .grid-inspector {
      width: auto;
      min-height: 0;
      max-height: 50dvh;
      flex-shrink: 0;
    }
  }
  .photo-date-selection {
    display: flex;
    gap: var(--space-3);
    align-items: center;
    padding: var(--space-2) var(--space-5);
    font-size: var(--font-size-sm);
    color: var(--text-primary);
  }
  .photo-views {
    display: flex;
    gap: var(--space-2);
  }
  .photo-error {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-5);
    color: var(--text-primary);
    background: var(--bg-inset);
  }
  .photo-notice {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-5);
    font-size: var(--font-size-sm);
  }
  .photo-notice a {
    color: var(--accent-blue);
  }
  form,
  .modal-actions {
    display: flex;
    gap: var(--space-2);
    align-items: center;
    flex-wrap: wrap;
  }
  .modal-actions {
    justify-content: flex-end;
    margin-top: var(--space-4);
  }
  .photo-loading,
  .photo-pill {
    flex-shrink: 0;
    display: flex;
    gap: var(--space-2);
    align-items: center;
    justify-content: center;
    padding: var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .photo-loading {
    height: 38px;
  }
  .photo-pill {
    position: absolute;
    left: 50%;
    bottom: var(--space-5);
    transform: translateX(-50%);
    border-radius: 999px;
    z-index: 4;
  }
  .photo-export-limit {
    margin: 0;
    padding: var(--space-2) var(--space-5);
    font-size: var(--font-size-sm);
    color: var(--text-muted);
  }
  .photo-bar,
  .photo-stack-controls {
    padding-left: var(--space-5);
    padding-right: var(--space-7);
  }
  .photo-bar {
    border-bottom: 1px solid var(--border-default);
  }
  .photo-stack-controls {
    justify-content: flex-start;
  }
  .photo-stack-controls p {
    flex-basis: 100%;
    margin: 0;
  }
  /* The shared mobile rule hides toolbar counts; here the count states the chosen date. */
  .photo-bar .toolbar-actions > span {
    display: inline-flex;
  }
  .photo-count-value {
    flex-shrink: 0;
  }
  .photo-count-date {
    white-space: pre;
  }
  .photo-bar .toolbar-actions :global(.kit-select-dropdown) {
    width: auto;
    min-width: 0;
  }
  .photo-crumbs {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-4);
    min-width: 0;
    color: var(--text-muted);
  }
  .photo-crumbs h1 {
    margin: 0;
    font-size: 18px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .album-back {
    border: 0;
    padding: 0;
    background: transparent;
    color: var(--accent-blue);
    font-size: var(--font-size-xs);
    cursor: pointer;
  }
</style>
