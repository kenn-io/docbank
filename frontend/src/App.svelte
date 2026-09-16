<script lang="ts">
  import { onMount, tick } from "svelte";
  import ActivityIcon from "@lucide/svelte/icons/activity";
  import ArchiveIcon from "@lucide/svelte/icons/archive";
  import ArrowLeftIcon from "@lucide/svelte/icons/arrow-left";
  import FileIcon from "@lucide/svelte/icons/file";
  import FolderIcon from "@lucide/svelte/icons/folder";
  import FoldersIcon from "@lucide/svelte/icons/folders";
  import HardDriveIcon from "@lucide/svelte/icons/hard-drive";
  import LogOutIcon from "@lucide/svelte/icons/log-out";
  import MapPinIcon from "@lucide/svelte/icons/map-pin";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import SearchIcon from "@lucide/svelte/icons/search";
  import BookmarkIcon from "@lucide/svelte/icons/bookmark";
  import ShieldCheckIcon from "@lucide/svelte/icons/shield-check";
  import TagIcon from "@lucide/svelte/icons/tag";
  import TagsIcon from "@lucide/svelte/icons/tags";
  import HistoryIcon from "@lucide/svelte/icons/history";
  import KeyboardIcon from "@lucide/svelte/icons/keyboard";
  import Trash2Icon from "@lucide/svelte/icons/trash-2";
  import UploadIcon from "@lucide/svelte/icons/upload";
  import {
    Button,
    Card,
    Checkbox,
    Chip,
    ChipStack,
    CopyButton,
    EmptyState,
    IconButton,
    SearchInput,
    Spinner,
    Table,
    TableHeaderCell,
    ThemeToggle,
    TopBar,
    createShortcutManager,
    formatShortcutKeys,
    type SortDirection,
  } from "@kenn-io/kit-ui";
  import AuditEvidenceDrawer from "./AuditEvidenceDrawer.svelte";
  import AuditHistoryDrawer from "./AuditHistoryDrawer.svelte";
  import ActionRecoveryModal from "./ActionRecoveryModal.svelte";
  import BackupDrawer from "./BackupDrawer.svelte";
  import CollectionsDrawer from "./CollectionsDrawer.svelte";
  import DownloadButton from "./DownloadButton.svelte";
  import FacetSidebar from "./FacetSidebar.svelte";
  import JobsDrawer from "./JobsDrawer.svelte";
  import ManageTagsModal from "./ManageTagsModal.svelte";
  import BatchTagsModal from "./BatchTagsModal.svelte";
  import type { BatchTagReceipt } from "./batch-tags.js";
  import ProcessingDrawer from "./ProcessingDrawer.svelte";
  import ProvenanceDrawer from "./ProvenanceDrawer.svelte";
  import SelectionDock from "./SelectionDock.svelte";
  import type { SelectionTarget } from "./selection.js";
  import ShortcutHelpModal from "./ShortcutHelpModal.svelte";
  import RenditionDrawer from "./RenditionDrawer.svelte";
  import StorageDrawer from "./StorageDrawer.svelte";
  import SavedQueriesDrawer from "./SavedQueriesDrawer.svelte";
  import QueryBar from "./QueryBar.svelte";
  import ResultsPager from "./ResultsPager.svelte";
  import SnapshotActions, { type SnapshotActionChoice } from "./SnapshotActions.svelte";
  import { parseQuery, type Query } from "./query.js";
  import { queryFromFragment, replaceQueryURL } from "./queryURL.js";
  import TagCatalogModal, {
    type TagDefinitionChange,
  } from "./TagCatalogModal.svelte";
  import TagLabel from "./TagLabel.svelte";
  import TagPicker from "./TagPicker.svelte";
  import TrashDrawer from "./TrashDrawer.svelte";
  import TrashNodeModal from "./TrashNodeModal.svelte";
  import UploadDrawer from "./UploadDrawer.svelte";
  import VersionHistoryDrawer from "./VersionHistoryDrawer.svelte";
  import {
    APIError,
    auditStatusForNode,
    changeNodeTag,
    children,
    liveTaggedNodes,
    nodeTags,
    revokeSession,
    search,
    statPath,
    tagByID,
    tags,
    takeFragmentSession,
    type AuditStatus,
    type Node,
    type SearchHit,
    type Tag,
    type TagAssignmentReceipt,
  } from "./api.js";
  import { downloadVisiblePageCSV, selectedVisibleCSVRows } from "./csv.js";
  import { basename, formatBytes, formatDate } from "./format.js";
  import { orderRows, reconcileSearchView, type SortField } from "./rows.js";
  import { sortTags } from "./tagPresentation.js";
  import { isAppShortcutSuppressed, moveInspection } from "./shortcuts.js";
  import { applySnapshotReceiptOverlay, snapshotTargetRevision, visibleSnapshotOverlay, type SnapshotReceiptOverlays } from "./snapshotOverlays.js";
  import { ActionJournal, type PersistedAction } from "./actionJournal.js";
  import { prepareAction, decodeRecovery } from "./actionRecovery.js";
  import { readActionVaultID } from "./actionRunner.js";
  import { SnapshotSession, type SnapshotState } from "./snapshotState.js";
  import { captureSnapshotTargets, type SnapshotOptions, type SnapshotRow } from "./snapshots.js";
  import {
    clearSelection,
    reconcileSelection,
    selectVisibleDocuments,
    selectedTargets,
    toggleDocumentSelection,
    type SelectionState,
  } from "./selection.js";
  import {
    TAG_HOTKEYS,
    isTagHotkeyVaultID,
    loadTagHotkeys,
    saveTagHotkeys,
    type TagHotkey,
    type TagHotkeyBindings,
  } from "./tag-hotkeys.js";
  import { VerifiedUploadChannel } from "./upload.js";

  type Row = { node: Node; path: string; match?: "name" | "content" };
  type Snapshot = {
    directory: Node;
    rows: Row[];
    selectedID?: number;
    activeQuery: string;
    activeTagID: string;
    searchQuery: string;
    tagFilterID: string;
    taggedInspected: number;
    taggedTotal: number;
    taggedTrashed: number;
    truncated: boolean;
    sortField: SortField;
    sortDirection: SortDirection;
  };

  let webSession = $state("");
  let uploadChannel = $state<VerifiedUploadChannel | null>(null);
  let uploadChannelError = $state("");
  let directory = $state<Node | null>(null);
  let rows = $state<Row[]>([]);
  let stack = $state<Snapshot[]>([]);
  let selectedID = $state<number | undefined>();
  let bulkSelection = $state<SelectionState>(clearSelection());
  let searchQuery = $state("");
  let activeQuery = $state("");
  let tagFilterID = $state("");
  let activeTagID = $state("");
  let taggedInspected = $state(0);
  let taggedTotal = $state(0);
  let taggedTrashed = $state(0);
  let tagCatalog = $state<Tag[]>([]);
  let tagCatalogTotal = $state(0);
  let tagCatalogListed = $state(0);
  let tagCatalogLoading = $state(false);
  let tagCatalogError = $state("");
  let vaultID = $state("");
  let tagHotkeys = $state<TagHotkeyBindings>({});
  let pendingTagHotkey = $state<TagHotkey | "">("");
  let shortcutNotice = $state("");
  let shortcutError = $state("");
  let shortcutHelpOpen = $state(false);
  let searchInputEl = $state<HTMLInputElement>();
  let selectedTags = $state<Tag[]>([]);
  let selectedTagsTotal = $state(0);
  let selectedTagsLoading = $state(false);
  let selectedTagsError = $state("");
  let loading = $state(false);
  let searchPending = $state(false);
  let error = $state("");
  let truncated = $state(false);
  let sortField = $state<SortField>("name");
  let sortDirection = $state<SortDirection>("asc");
  let selectedAudit = $state<AuditStatus | null>(null);
  let auditLoading = $state(false);
  let auditError = $state("");
  let historyOpen = $state(false);
  let versionsOpen = $state(false);
  let provenanceOpen = $state(false);
  let processingTarget = $state<Row | null>(null);
  let renditionTarget = $state<{ attachmentID: string; path: string } | null>(null);
  let jobsOpen = $state(false);
  let auditEvidenceOpen = $state(false);
  let storageOpen = $state(false);
  let backupsOpen = $state(false);
  let savedQueriesOpen = $state(false);
  let queryBarOpen = $state(false);
  let savedQueryDraft = $state<Query | null>(null);
  let queryEditorInitial = $state<Query>(parseQuery("{}"));
  let queryURLError = $state("");
  let snapshotState = $state<Readonly<SnapshotState>>({ status: "idle", offset: 0 });
  let snapshotController: SnapshotSession | undefined;
  let snapshotEpoch = 0;
  let selectedSnapshotID = $state<number | undefined>();
  let snapshotSelection = $state<Set<number>>(new Set());
  let snapshotOverlays = $state<SnapshotReceiptOverlays>({});
  let snapshotActionsOpen = $state(false);
  let snapshotActionBusy = $state(false);
  let snapshotActionController: AbortController | undefined;
  let snapshotActionGeneration = 0;
  let snapshotActionError = $state("");
  let recoveryJournal = $state<ActionJournal | null>(null);
  let recoveryAction = $state<PersistedAction | null>(null);
  let recoveryTag = $state<Tag | null>(null);
  let recoveryVaultID = $state("");
  let recoverySnapshotEpoch = 0;
  let recoverySnapshotID: string | undefined;
  let collectionsOpen = $state(false);
  let trashOpen = $state(false);
  let manageTagsTarget = $state<Row | null>(null);
  let batchTagsTargets = $state<SelectionTarget[] | null>(null);
  let batchTagsContext = $state<"live" | "snapshot">("live");
  let batchTagsChoice = $state<SnapshotActionChoice>();
  let batchTagsSnapshotID: string | undefined;
  let batchTagsSnapshotEpoch = 0;
  let tagCatalogOpen = $state(false);
  let uploadTarget = $state<Node | null>(null);
  let trashTarget = $state<Row | null>(null);
  let generation = 0;
  let auditGeneration = 0;
  let tagGeneration = 0;
  let tagCatalogGeneration = 0;
  let tagHotkeyGeneration = 0;
  let pendingSelectionRange = false;

  const selected = $derived(rows.find((row) => row.node.id === selectedID));
  const snapshotActive = $derived(snapshotState.status !== "idle");
  const snapshotPage = $derived(snapshotState.page);
  const snapshotQuery = $derived(snapshotState.query);
  const selectedSnapshot = $derived(snapshotPage?.rows.find((row) => row.node_id === selectedSnapshotID));
  const selectedSnapshotOverlay = $derived(selectedSnapshot ? snapshotOverlay(selectedSnapshot) : undefined);
  const selectedSnapshotRows = $derived(snapshotPage?.rows.filter((row) => snapshotSelection.has(row.node_id)) ?? []);
  const snapshotTargets = $derived(selectedSnapshotRows.map((row) => ({
    node_id: row.node_id,
    revision: snapshotTargetRevision(row, snapshotOverlays),
  })));
  const allVisibleSnapshotRowsSelected = $derived(
    Boolean(snapshotPage?.rows.length) && selectedSnapshotRows.length === snapshotPage?.rows.length,
  );
  const membership = $derived(selectedAudit?.membership);
  const tagPickerTitle = $derived(
    tagCatalogError
      ? `Tags unavailable: ${tagCatalogError}`
      : tagCatalogTotal > tagCatalogListed
        ? `Browse or filter: showing ${tagCatalogListed} of ${tagCatalogTotal} tags`
        : "Browse or filter by tag",
  );
  const activeTag = $derived(tagCatalog.find((tag) => tag.id === activeTagID));
  const tagBrowse = $derived(activeTagID !== "" && activeQuery === "");
  const sortedRows = $derived(
    orderRows(rows, sortField, sortDirection, activeQuery !== "" || tagBrowse),
  );
  const visibleDocumentCount = $derived(
    sortedRows.filter((row) => row.node.kind === "file").length,
  );
  const selectedCount = $derived(bulkSelection.selectedIDs.size);
  const bulkTargets = $derived(selectedTargets(sortedRows, bulkSelection.selectedIDs));
  const allVisibleDocumentsSelected = $derived(
    visibleDocumentCount > 0 && selectedCount === visibleDocumentCount,
  );

  onMount(() => {
    try { savedQueryDraft = queryFromFragment(location.hash); }
    catch (cause) { queryURLError = cause instanceof Error ? cause.message : String(cause); }
    const shortcutManager = createShortcutManager();
    const unregister = [
      shortcutManager.register("/", focusSearch, {
        description: "Focus search",
      }),
      shortcutManager.register("j", () => void inspectRelative(1), {
        description: "Inspect next loaded row",
      }),
      shortcutManager.register("k", () => void inspectRelative(-1), {
        description: "Inspect previous loaded row",
      }),
      shortcutManager.register("space", toggleInspectedSelection, {
        description: "Toggle inspected file selection",
      }),
      shortcutManager.register("enter", activateInspected, {
        description: "Open inspected row",
      }),
      shortcutManager.register("escape", clearShortcutSelection, {
        description: "Clear page selection",
      }),
      shortcutManager.register("shift+/", openShortcutHelp, {
        description: "Show keyboard shortcuts",
      }),
      ...TAG_HOTKEYS.map((key) =>
        shortcutManager.register(
          key,
          (event) => void toggleInspectedTag(key, event),
          { description: `Toggle configured tag ${key}` },
        ),
      ),
    ];
    const handleShortcut = (event: KeyboardEvent): void => {
      if (
        isAppShortcutSuppressed(
          event,
          !webSession || loading || searchPending || snapshotActive,
        )
      ) {
        return;
      }
      shortcutManager.handleKeydown(event);
    };
    window.addEventListener("keydown", handleShortcut);
    const detachShortcuts = (): void => {
      window.removeEventListener("keydown", handleShortcut);
      unregister.forEach((remove) => remove());
    };

    const session = takeFragmentSession();
    if (savedQueryDraft) replaceQueryURL(savedQueryDraft);
    if (session) {
      webSession = session.token;
      if (savedQueryDraft) queryBarOpen = true;
      void loadRoot();
      void loadTagCatalog();
      const channel = new VerifiedUploadChannel(session, undefined, () => {
        if (uploadChannel === channel) {
          uploadChannelError =
            "The verified upload channel ended. Run `docbank web` again before selecting more files.";
        }
      });
      void channel.connect().then(
        () => {
          if (webSession === session.token) uploadChannel = channel;
          else channel.close();
        },
        (cause) => {
          uploadChannelError = cause instanceof Error ? cause.message : String(cause);
        },
      );
      return () => {
        channel.close();
        detachShortcuts();
        snapshotController?.dispose();
        invalidateSnapshotActions();
      };
    }
    return detachShortcuts;
  });

  function clearBulkSelection(): void {
    bulkSelection = clearSelection();
    pendingSelectionRange = false;
  }

  function invalidateTagHotkeyMutation(): void {
    tagHotkeyGeneration += 1;
    pendingTagHotkey = "";
  }

  function localPreferenceStorage(): Storage | undefined {
    try {
      return globalThis.localStorage;
    } catch {
      return undefined;
    }
  }

  function clearShortcutFeedback(): void {
    shortcutNotice = "";
    shortcutError = "";
  }

  function focusSearch(): void {
    clearShortcutFeedback();
    searchInputEl?.focus();
  }

  async function revealInspectedRow(nodeID: number): Promise<void> {
    await tick();
    const row = document.querySelector<HTMLElement>(`tr[data-node-id="${nodeID}"]`);
    if (!row) return;
    row.focus({ preventScroll: true });
    row.scrollIntoView({ block: "nearest" });
    const dock = document.querySelector<HTMLElement>(".selection-dock");
    const scroller = row.closest<HTMLElement>(".kit-table-wrapper");
    if (!dock || !scroller) return;
    const rowBounds = row.getBoundingClientRect();
    const dockBounds = dock.getBoundingClientRect();
    const overlap = rowBounds.bottom - dockBounds.top;
    if (overlap > 0) scroller.scrollBy({ top: overlap + 8, behavior: "auto" });
  }

  async function inspectRelative(direction: -1 | 1): Promise<void> {
    clearShortcutFeedback();
    const move = moveInspection(
      sortedRows.map((row) => ({ id: row.node.id })),
      selectedID,
      direction,
    );
    if (move.boundary === "empty") {
      shortcutNotice = "No rows are loaded.";
      return;
    }
    if (move.id !== undefined && move.id !== selectedID) selectNode(move.id);
    if (move.id !== undefined) await revealInspectedRow(move.id);
    if (move.boundary === "first") shortcutNotice = "First loaded row reached.";
    else if (move.boundary === "last") shortcutNotice = "Last loaded row reached.";
  }

  function toggleInspectedSelection(): void {
    clearShortcutFeedback();
    if (!selected) {
      shortcutNotice = "No row is inspected.";
      return;
    }
    if (selected.node.kind !== "file") {
      shortcutNotice = "The inspected row is a folder; only files can be selected.";
      return;
    }
    toggleBulkSelection(
      selected,
      !bulkSelection.selectedIDs.has(selected.node.id),
    );
  }

  function activateInspected(): void {
    clearShortcutFeedback();
    if (!selected) {
      shortcutNotice = "No row is inspected.";
      return;
    }
    activate(selected);
  }

  function clearShortcutSelection(): void {
    clearShortcutFeedback();
    clearBulkSelection();
    shortcutNotice = "Page selection cleared.";
  }

  function openShortcutHelp(): void {
    clearShortcutFeedback();
    shortcutHelpOpen = true;
  }

  function changeTagHotkeyBinding(key: TagHotkey, tagID: string): void {
    if (!vaultID) return;
    const next = { ...tagHotkeys };
    if (tagID) next[key] = tagID;
    else delete next[key];
    const storage = localPreferenceStorage();
    if (!storage || !saveTagHotkeys(storage, vaultID, next)) {
      shortcutError = "This browser blocked saving tag shortcut preferences.";
      return;
    }
    tagHotkeys = next;
    clearShortcutFeedback();
    shortcutNotice = tagID
      ? `Tag shortcut ${key} saved for this vault.`
      : `Tag shortcut ${key} cleared for this vault.`;
  }

  async function toggleInspectedTag(
    key: TagHotkey,
    event: KeyboardEvent,
  ): Promise<void> {
    if (event.repeat || pendingTagHotkey) return;
    clearShortcutFeedback();
    const tagID = tagHotkeys[key];
    if (!tagID) {
      shortcutNotice = `No tag is assigned to shortcut ${key}.`;
      return;
    }
    const target = selected;
    if (!target || target.node.kind !== "file") {
      shortcutNotice = "Inspect a file before using a tag shortcut.";
      return;
    }
    const tag = tagCatalog.find((item) => item.id === tagID);
    if (!tag) {
      shortcutNotice = `Tag shortcut ${key} is unavailable because its saved tag is not in the loaded catalog.`;
      return;
    }
    if (
      selectedTagsLoading ||
      Boolean(selectedTagsError) ||
      selectedTagsTotal !== selectedTags.length
    ) {
      shortcutNotice = "Tag shortcuts are unavailable until all assigned tags for this file are known.";
      return;
    }

    const session = webSession;
    const expectedNodeID = target.node.id;
    const expectedRevision = target.node.revision;
    const assigned = selectedTags.some((item) => item.id === tagID);
    const request = ++tagHotkeyGeneration;
    pendingTagHotkey = key;
    try {
      const receipt = await changeNodeTag(
        session,
        expectedNodeID,
        expectedRevision,
        tagID,
        !assigned,
      );
      const current = rows.find((row) => row.node.id === expectedNodeID);
      if (
        request !== tagHotkeyGeneration ||
        session !== webSession ||
        selectedID !== expectedNodeID ||
        current?.node.revision !== expectedRevision
      ) {
        return;
      }
      handleTagChanged(receipt, !assigned);
      shortcutNotice = assigned
        ? `Removed ${receipt.tag.name} from ${target.node.name}.`
        : `Added ${receipt.tag.name} to ${target.node.name}.`;
    } catch (cause) {
      if (
        request !== tagHotkeyGeneration ||
        session !== webSession ||
        selectedID !== expectedNodeID
      ) {
        return;
      }
      if (cause instanceof APIError && cause.status === 401) {
        handleFailure(cause);
        return;
      }
      shortcutError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === tagHotkeyGeneration) pendingTagHotkey = "";
    }
  }

  function replaceRows(nextRows: Row[], reconcile: boolean): void {
    rows = nextRows;
    bulkSelection = reconcile
      ? reconcileSelection(bulkSelection, nextRows)
      : clearSelection();
    pendingSelectionRange = false;
  }

  function toggleBulkSelection(row: Row, checked: boolean): void {
    bulkSelection = toggleDocumentSelection(
      bulkSelection,
      sortedRows,
      row.node.id,
      checked,
      pendingSelectionRange,
    );
    pendingSelectionRange = false;
  }

  function selectAllVisibleDocuments(checked = true): void {
    bulkSelection = checked
      ? selectVisibleDocuments(sortedRows)
      : clearSelection();
    pendingSelectionRange = false;
  }

  function exportPageCSV(): void {
    const selectedRows = selectedVisibleCSVRows(sortedRows, bulkSelection.selectedIDs);
    if (selectedRows.length > 0) downloadVisiblePageCSV(selectedRows);
  }

  function handleFailure(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      leaveSnapshotMode();
      savedQueriesOpen = false;
      savedQueryDraft = null;
      replaceQueryURL(null);
      uploadChannel?.close();
      webSession = "";
      uploadChannel = null;
      historyOpen = false;
      versionsOpen = false;
      provenanceOpen = false;
      processingTarget = null;
      renditionTarget = null;
      jobsOpen = false;
      auditEvidenceOpen = false;
      storageOpen = false;
      backupsOpen = false;
      collectionsOpen = false;
      trashOpen = false;
      tagCatalogOpen = false;
      shortcutHelpOpen = false;
      uploadTarget = null;
      trashTarget = null;
      tagCatalog = [];
      tagCatalogListed = 0;
      tagCatalogLoading = false;
      vaultID = "";
      tagHotkeys = {};
      invalidateTagHotkeyMutation();
      selectedTags = [];
      selectedTagsTotal = 0;
      searchPending = false;
      clearBulkSelection();
      error = "The browser session expired or was rejected. Run `docbank web` again.";
      return;
    }
    error = cause instanceof Error ? cause.message : String(cause);
  }

  async function loadRoot(): Promise<void> {
    invalidateTagHotkeyMutation();
    leaveSnapshotMode();
    clearBulkSelection();
    const request = ++generation;
    const session = webSession;
    loading = true;
    error = "";
    try {
      const root = await statPath(session, "/");
      if (request !== generation || session !== webSession) return;
      await loadDirectory(root.id, false);
    } catch (cause) {
      if (request !== generation || session !== webSession) return;
      handleFailure(cause);
      loading = false;
    }
  }

  async function loadDirectory(
    nodeID: number,
    remember: boolean,
    preferredSelectedID?: number,
    preserveSort = false,
    preferredRow?: Row,
  ): Promise<void> {
    invalidateTagHotkeyMutation();
    leaveSnapshotMode();
    const refreshing = !remember && directory?.id === nodeID && !activeQuery && !activeTagID;
    const request = ++generation;
    searchPending = false;
    loading = true;
    error = "";
    try {
      const page = await children(webSession, nodeID);
      if (request !== generation) return;
      if (preferredRow && !page.items.some((item) => item.id === preferredSelectedID)) {
        const current = await statPath(webSession, preferredRow.path);
        if (request !== generation) return;
        if (
          current.id !== preferredSelectedID || current.parent_id !== nodeID ||
          current.path !== preferredRow.path ||
          current.path !== `${page.directory.path === "/" ? "" : page.directory.path}/${current.name}`
        ) {
          throw new Error("This collection member changed while opening. Reload the collection.");
        }
        preferredRow = { node: current, path: preferredRow.path };
      }
      if (remember && directory) {
        stack = [
          ...stack,
          {
            directory,
            rows,
            selectedID,
            activeQuery,
            activeTagID,
            searchQuery,
            tagFilterID,
            taggedInspected,
            taggedTotal,
            taggedTrashed,
            truncated,
            sortField,
            sortDirection,
          },
        ];
      }
      directory = page.directory;
      const path = page.directory.path;
      if (!path) throw new Error("The selected directory is no longer live.");
      const nextRows = page.items.map((item) => ({
        node: item,
        path: path === "/" ? `/${item.name}` : `${path}/${item.name}`,
      }));
      if (
        preferredRow &&
        preferredRow.node.id === preferredSelectedID &&
        !nextRows.some((row) => row.node.id === preferredSelectedID)
      ) {
        nextRows.push(preferredRow);
      }
      replaceRows(nextRows, refreshing);
      selectNode(
        rows.some((row) => row.node.id === preferredSelectedID)
          ? preferredSelectedID
          : preferredSelectedID === undefined
            ? rows[0]?.node.id
            : undefined,
      );
      activeQuery = "";
      activeTagID = "";
      taggedInspected = 0;
      taggedTotal = 0;
      taggedTrashed = 0;
      truncated = page.total > rows.length;
      if (!preserveSort) {
        sortField = "name";
        sortDirection = "asc";
      }
    } catch (cause) {
      if (request === generation) {
        if (cause instanceof APIError && cause.status === 404) {
          replaceRows([], false);
          selectedID = undefined;
          error = preferredRow
            ? "This collection member moved or was removed. Reload the collection."
            : "This directory was moved to trash or removed. Go back or reload the vault.";
        } else {
          handleFailure(cause);
        }
      }
    } finally {
      if (request === generation) loading = false;
    }
  }

  async function runSearch(preferredSelectedID = selectedID): Promise<void> {
    invalidateTagHotkeyMutation();
    leaveSnapshotMode();
    const query = searchQuery.trim();
    if (!query) {
      if (tagFilterID) await loadTaggedNodes(tagFilterID);
      else if (directory) await loadDirectory(directory.id, false);
      return;
    }
    const request = ++generation;
    const requestedTagID = tagFilterID;
    const refreshing = activeQuery === query && activeTagID === requestedTagID;
    searchPending = true;
    loading = true;
    error = "";
    try {
      const report = await search(webSession, query, requestedTagID);
      if (request !== generation) return;
      if ((report.tag_id ?? "") !== requestedTagID) {
        throw new Error("Search results did not honor the selected tag filter.");
      }
      const nextRows = report.hits.map((hit: SearchHit) => ({
        node: hit.node,
        path: hit.path,
        match: hit.match,
      }));
      replaceRows(nextRows, refreshing);
      const view = reconcileSearchView(
        nextRows,
        query,
        requestedTagID === activeTagID ? activeQuery : "",
        sortField,
        sortDirection,
        preferredSelectedID,
      );
      activeQuery = query;
      activeTagID = requestedTagID;
      taggedInspected = 0;
      taggedTotal = 0;
      taggedTrashed = 0;
      truncated = report.truncated;
      sortField = view.sortField;
      sortDirection = view.sortDirection;
      selectNode(view.selectedID);
    } catch (cause) {
      if (request === generation) handleFailure(cause);
    } finally {
      if (request === generation) {
        searchPending = false;
        loading = false;
      }
    }
  }

  async function loadTaggedNodes(
    tagID: string,
    preferredSelectedID?: number,
  ): Promise<void> {
    invalidateTagHotkeyMutation();
    leaveSnapshotMode();
    if (!directory) return;
    const request = ++generation;
    const refreshing = activeQuery === "" && activeTagID === tagID;
    const selectedToPreserve = preferredSelectedID ?? (refreshing ? selectedID : undefined);
    searchPending = false;
    loading = true;
    error = "";
    try {
      const page = await liveTaggedNodes(webSession, tagID);
      if (request !== generation) return;
      const liveRows = page.items.map((item) => ({ node: item.node, path: item.path! }));
      replaceRows(liveRows, refreshing);
      activeQuery = "";
      activeTagID = tagID;
      taggedInspected = liveRows.length;
      taggedTotal = page.total;
      taggedTrashed = page.omitted_trashed ?? 0;
      truncated = page.total > liveRows.length;
      if (!refreshing) {
        sortField = "name";
        sortDirection = "asc";
      }
      selectNode(
        liveRows.some((row) => row.node.id === selectedToPreserve)
          ? selectedToPreserve
          : liveRows[0]?.node.id,
      );
    } catch (cause) {
      if (request === generation) {
        tagFilterID = activeTagID;
        handleFailure(cause);
      }
    } finally {
      if (request === generation) loading = false;
    }
  }

  function goBack(): void {
    invalidateTagHotkeyMutation();
    leaveSnapshotMode();
    generation += 1;
    searchPending = false;
    const previous = stack.at(-1);
    if (!previous) return;
    const preferredSelectedID = previous.selectedID;
    clearBulkSelection();
    selectNode(undefined);
    directory = previous.directory;
    replaceRows([], false);
    stack = stack.slice(0, -1);
    activeQuery = previous.activeQuery;
    activeTagID = previous.activeTagID;
    searchQuery = previous.searchQuery;
    tagFilterID = previous.tagFilterID;
    taggedInspected = 0;
    taggedTotal = 0;
    taggedTrashed = 0;
    truncated = false;
    sortField = previous.sortField;
    sortDirection = previous.sortDirection;
    error = "";
    loading = false;
    void loadTagCatalog();
    if (activeQuery) void runSearch(preferredSelectedID);
    else if (activeTagID) {
      void loadTaggedNodes(activeTagID, preferredSelectedID);
    } else {
      void loadDirectory(directory.id, false, preferredSelectedID, true);
    }
  }

  function clearSearch(): void {
    searchQuery = "";
    if (!activeQuery && !searchPending) return;
    if (tagFilterID) void loadTaggedNodes(tagFilterID);
    else if (directory) void loadDirectory(directory.id, false);
  }

  function changeTagFilter(tagID: string): void {
    tagFilterID = tagID;
    if (activeQuery || searchPending) void runSearch();
    else if (tagID) void loadTaggedNodes(tagID);
    else if (directory) void loadDirectory(directory.id, false);
  }

  function activate(row: Row): void {
    selectNode(row.node.id);
    if (row.node.kind === "dir") {
      void loadDirectory(row.node.id, true);
    }
  }

  function selectNode(nodeID: number | undefined): void {
    if (selectedID === nodeID && pendingTagHotkey) return;
    if (selectedID !== nodeID) {
      invalidateTagHotkeyMutation();
      historyOpen = false;
      versionsOpen = false;
      provenanceOpen = false;
      processingTarget = null;
      renditionTarget = null;
    }
    selectedID = nodeID;
    selectedAudit = null;
    selectedTags = [];
    selectedTagsTotal = 0;
    selectedTagsError = "";
    auditError = "";
    auditGeneration += 1;
    tagGeneration += 1;
    if (webSession) void loadAuditStatus(nodeID);
    if (nodeID !== undefined && webSession) void loadSelectedTags(nodeID);
  }

  async function loadTagCatalog(): Promise<void> {
    const request = ++tagCatalogGeneration;
    const session = webSession;
    const selectedTagID = tagFilterID;
    tagCatalogLoading = true;
    tagCatalogError = "";
    try {
      const page = await tags(session);
      if (request !== tagCatalogGeneration || session !== webSession) return;
      let items = page.items;
      let selectedMissing = false;
      if (
        selectedTagID &&
        selectedTagID === tagFilterID &&
        !items.some((tag) => tag.id === selectedTagID)
      ) {
        if (page.total > page.items.length) {
          try {
            const selectedTag = await tagByID(session, selectedTagID);
            if (request !== tagCatalogGeneration || session !== webSession) return;
            items = [selectedTag, ...items];
          } catch (cause) {
            if (request !== tagCatalogGeneration || session !== webSession) return;
            if (cause instanceof APIError && cause.status === 404) selectedMissing = true;
            else throw cause;
          }
        } else {
          selectedMissing = true;
        }
      }
      if (request !== tagCatalogGeneration || session !== webSession) return;
      tagCatalog = sortTags(selectedTagID === tagFilterID ? items : page.items);
      tagCatalogTotal = page.total;
      tagCatalogListed = page.items.length;
      if (selectedMissing && tagFilterID === selectedTagID) {
        const rerunSearch = Boolean(activeQuery || searchPending);
        const leaveTagBrowse = activeQuery === "" && activeTagID === selectedTagID;
        clearBulkSelection();
        tagFilterID = "";
        activeTagID = "";
        taggedInspected = 0;
        taggedTotal = 0;
        taggedTrashed = 0;
        if (rerunSearch && searchQuery.trim()) void runSearch();
        else if (leaveTagBrowse && directory) void loadDirectory(directory.id, false);
      }
    } catch (cause) {
      if (request !== tagCatalogGeneration || session !== webSession) return;
      if (cause instanceof APIError && cause.status === 401) {
        handleFailure(cause);
        return;
      }
      tagCatalogError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === tagCatalogGeneration) tagCatalogLoading = false;
    }
  }

  async function loadSelectedTags(nodeID: number): Promise<void> {
    const request = ++tagGeneration;
    const session = webSession;
    selectedTagsLoading = true;
    try {
      const page = await nodeTags(session, nodeID);
      if (request !== tagGeneration || session !== webSession || selectedID !== nodeID) return;
      selectedTags = sortTags(page.items);
      selectedTagsTotal = page.total;
    } catch (cause) {
      if (request !== tagGeneration || session !== webSession || selectedID !== nodeID) return;
      if (cause instanceof APIError && cause.status === 401) {
        handleFailure(cause);
        return;
      }
      selectedTagsError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === tagGeneration) selectedTagsLoading = false;
    }
  }

  async function loadAuditStatus(nodeID?: number): Promise<void> {
    const request = ++auditGeneration;
    const session = webSession;
    auditLoading = true;
    try {
      const status = await auditStatusForNode(session, nodeID);
      if (request !== auditGeneration || session !== webSession || selectedID !== nodeID) return;
      selectedAudit = status;
      if (isTagHotkeyVaultID(status.vault_id)) {
        if (vaultID !== status.vault_id) {
          vaultID = status.vault_id;
          const storage = localPreferenceStorage();
          tagHotkeys = storage ? loadTagHotkeys(storage, vaultID) : {};
        }
      } else {
        vaultID = "";
        tagHotkeys = {};
      }
    } catch (cause) {
      if (request !== auditGeneration || session !== webSession || selectedID !== nodeID) return;
      if (cause instanceof APIError && cause.status === 401) {
        handleFailure(cause);
        return;
      }
      auditError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === auditGeneration) auditLoading = false;
    }
  }

  function sortBy(field: SortField): void {
    if (sortField === field) {
      sortDirection = sortDirection === "asc" ? "desc" : "asc";
    } else {
      sortField = field;
      sortDirection = field === "name" ? "asc" : "desc";
    }
  }

  function refreshCurrentView(): void {
    void loadTagCatalog();
    if (activeQuery) void runSearch();
    else if (activeTagID) void loadTaggedNodes(activeTagID);
    else if (directory) void loadDirectory(directory.id, false);
    else void loadRoot();
  }

  function openBatchTags(targets: readonly SelectionTarget[]): void {
    if (loading) return;
    batchTagsChoice = undefined;
    batchTagsContext = "live";
    batchTagsTargets = targets.map((target) => ({ ...target }));
  }

  function handleBatchTagsChanged(_receipt: BatchTagReceipt): void {
    if (batchTagsContext === "snapshot") {
      if (batchTagsSnapshotEpoch !== snapshotEpoch || batchTagsSnapshotID !== snapshotState.firstPage?.snapshot_id) return;
      applySnapshotReceipt(_receipt);
      return;
    }
    // A replay describes a historical success. Reload current observations
    // instead of overwriting newer rows with the receipt's old revisions.
    refreshCurrentView();
    if (selectedID !== undefined) void loadSelectedTags(selectedID);
  }

  function leaveSnapshotMode(): void {
    invalidateSnapshotActions();
    if (snapshotState.status === "idle" && snapshotController === undefined) return;
    snapshotController?.dispose();
    snapshotController = undefined;
    snapshotState = { status: "idle", offset: 0 };
    selectedSnapshotID = undefined;
    snapshotSelection = new Set();
    snapshotOverlays = {};
    snapshotActionsOpen = false;
  }

  function snapshotSession(): SnapshotSession {
    if (snapshotController) return snapshotController;
    snapshotController = new SnapshotSession(webSession, (next) => {
      const prior = snapshotState;
      snapshotState = next;
      const page = next.page;
      if (page !== prior.page) snapshotSelection = new Set();
      if (next.firstPage?.snapshot_id !== prior.firstPage?.snapshot_id) snapshotOverlays = {};
      selectedSnapshotID = page?.rows.some((row) => row.node_id === selectedSnapshotID)
        ? selectedSnapshotID
        : page?.rows[0]?.node_id;
      if (next.error instanceof APIError && next.error.status === 401) handleFailure(next.error);
    });
    return snapshotController;
  }

  function runSnapshot(query: Query, options: SnapshotOptions): void {
    invalidateSnapshotActions();
    invalidateTagHotkeyMutation();
    generation += 1;
    searchPending = false;
    loading = false;
    keepQueryDraft(query);
    void snapshotSession().run(query, options);
  }

  function runSnapshotAgain(): void {
    if (!snapshotState.query || !snapshotState.options) return;
    runSnapshot(snapshotState.query, snapshotState.options);
  }

  function changeSnapshotQuery(query: Query): void {
    if (!snapshotState.options) return;
    runSnapshot(query, snapshotState.options);
  }

  function sortSnapshot(field: Query["sort"]["field"]): void {
    const query = snapshotState.query;
    if (!query || !snapshotState.options) return;
    const direction = query.sort.field === field
      ? query.sort.direction === "asc" ? "desc" : "asc"
      : field === "name" || field === "path" || field === "media_type" ? "asc" : "desc";
    changeSnapshotQuery(parseQuery(JSON.stringify({ ...query, sort: { field, direction } })));
  }

  function pageSnapshot(direction: "previous" | "next"): void {
    void snapshotController?.page(direction);
  }

  function toggleSnapshotSelection(row: SnapshotRow, checked: boolean): void {
    const next = new Set(snapshotSelection);
    if (checked) next.add(row.node_id);
    else next.delete(row.node_id);
    snapshotSelection = next;
  }

  function selectVisibleSnapshotRows(checked = true): void {
    snapshotSelection = checked && snapshotPage
      ? new Set(snapshotPage.rows.map((row) => row.node_id))
      : new Set();
  }

  function snapshotOverlay(row: SnapshotRow) {
    return visibleSnapshotOverlay(row, snapshotOverlays);
  }

  function applySnapshotReceipt(receipt: BatchTagReceipt): void {
    const tagLabel = tagCatalog.find((tag) => tag.id === receipt.tag_id)?.name ?? receipt.tag_id;
    snapshotOverlays = applySnapshotReceiptOverlay(snapshotOverlays, receipt, tagLabel);
  }

  function closeRecovery(): void {
    recoveryJournal = null;
    recoveryAction = null;
    recoveryTag = null;
    recoveryVaultID = "";
  }

  function invalidateSnapshotActions(): void {
    snapshotEpoch++;
    cancelSnapshotAction();
    if (batchTagsContext === "snapshot") batchTagsTargets = null;
    closeRecovery();
  }

  function openSnapshotBatchTags(choice?: SnapshotActionChoice): void {
    if (snapshotState.status !== "ready" || snapshotTargets.length === 0 || snapshotTargets.length > 1000) return;
    batchTagsChoice = choice;
    batchTagsContext = "snapshot";
    batchTagsSnapshotID = snapshotState.firstPage?.snapshot_id;
    batchTagsSnapshotEpoch = snapshotEpoch;
    batchTagsTargets = snapshotTargets.map((target) => ({ ...target }));
    snapshotActionsOpen = false;
  }

  function handleRecoveryProgress(action: Readonly<PersistedAction>, receipt?: BatchTagReceipt): void {
    if (recoverySnapshotEpoch !== snapshotEpoch || recoverySnapshotID !== snapshotState.firstPage?.snapshot_id ||
        recoveryAction?.action_id !== action.action_id) return;
    recoveryAction = action;
    if (receipt && recoverySnapshotID) applySnapshotReceipt(receipt);
  }

  function cancelSnapshotAction(): void {
    snapshotActionGeneration++;
    snapshotActionController?.abort();
    snapshotActionController = undefined;
    snapshotActionBusy = false;
    snapshotActionError = "";
    snapshotActionsOpen = false;
  }

  function beginSnapshotAction() {
    const session = webSession;
    const request = ++snapshotActionGeneration;
    const epoch = snapshotEpoch;
    const snapshotID = snapshotState.firstPage?.snapshot_id;
    const controller = new AbortController();
    snapshotActionController = controller;
    snapshotActionBusy = true;
    snapshotActionError = "";
    return { session, signal: controller.signal,
      current: () => request === snapshotActionGeneration && session === webSession && !controller.signal.aborted &&
        epoch === snapshotEpoch && snapshotID === snapshotState.firstPage?.snapshot_id };
  }

  async function showRecovery(
    journal: ActionJournal,
    action: PersistedAction,
    vaultID: string,
    operation: ReturnType<typeof beginSnapshotAction>,
  ): Promise<void> {
    if (!operation.current()) return;
    const currentTag = await tagByID(operation.session, action.tag_id).catch((cause) => {
      if (cause instanceof APIError && cause.status === 404) return null;
      throw cause;
    });
    if (!operation.current()) return;
    recoverySnapshotEpoch = snapshotEpoch;
    recoverySnapshotID = snapshotState.firstPage?.snapshot_id;
    recoveryJournal = journal;
    recoveryAction = action;
    recoveryTag = currentTag;
    recoveryVaultID = vaultID;
    snapshotActionsOpen = false;
  }

  async function startSnapshotAction(choice: SnapshotActionChoice): Promise<void> {
    if (snapshotActionBusy || snapshotState.status !== "ready") return;
    snapshotActionError = "";
    if (choice.scope === "selection") {
      openSnapshotBatchTags(choice);
      return;
    }
    const firstPage = snapshotState.firstPage;
    if (!firstPage || firstPage.total === 0) return;
    const operation = beginSnapshotAction();
    try {
      // Complete enumeration and hash/byte verification happen before random
      // operation identities are prepared or anything is persisted.
      const targets = await captureSnapshotTargets(operation.session, firstPage, operation.signal, snapshotOverlays);
      if (!operation.current()) return;
      const vaultID = await readActionVaultID(operation.session);
      if (!operation.current()) return;
      const journal = await ActionJournal.open(vaultID);
      const prepared = await prepareAction(vaultID, targets, choice.tagID, choice.assign);
      if (!operation.current()) return;
      await journal.prepare(prepared, operation.signal);
      const action = await journal.load();
      if (!action) throw new Error("The prepared action was not retained in durable storage.");
      await showRecovery(journal, action, vaultID, operation);
    } catch (cause) {
      if (!operation.current()) return;
      if (cause instanceof APIError && cause.status === 401) handleFailure(cause);
      else snapshotActionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (operation.current()) {
        snapshotActionBusy = false;
        snapshotActionController = undefined;
      }
    }
  }

  async function importSnapshotAction(bytes: Uint8Array): Promise<void> {
    if (snapshotActionBusy) return;
    const operation = beginSnapshotAction();
    try {
      const prepared = await decodeRecovery(bytes);
      if (!operation.current()) return;
      const vaultID = await readActionVaultID(operation.session);
      if (!operation.current()) return;
      if (prepared.vault_id !== vaultID) throw new Error("The recovery action belongs to a different vault.");
      const journal = await ActionJournal.open(vaultID);
      if (!operation.current()) return;
      await journal.prepare(prepared, operation.signal);
      await journal.verifyCheckpoint(bytes);
      const action = await journal.load();
      if (!action) throw new Error("The imported action was not retained in durable storage.");
      await showRecovery(journal, action, vaultID, operation);
    } catch (cause) {
      if (!operation.current()) return;
      if (cause instanceof APIError && cause.status === 401) handleFailure(cause);
      else snapshotActionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (operation.current()) {
        snapshotActionBusy = false;
        snapshotActionController = undefined;
      }
    }
  }

  async function resumeSnapshotAction(): Promise<void> {
    if (snapshotActionBusy) return;
    const operation = beginSnapshotAction();
    try {
      const vaultID = await readActionVaultID(operation.session);
      if (!operation.current()) return;
      const journal = await ActionJournal.open(vaultID);
      const action = await journal.load();
      if (!action) throw new Error("No retained action is available for this vault. Select a recovery file instead.");
      await showRecovery(journal, action, vaultID, operation);
    } catch (cause) {
      if (!operation.current()) return;
      if (cause instanceof APIError && cause.status === 401) handleFailure(cause);
      else snapshotActionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (operation.current()) {
        snapshotActionBusy = false;
        snapshotActionController = undefined;
      }
    }
  }

  async function abandonSnapshotAction(): Promise<void> {
    if (snapshotActionBusy) return;
    const operation = beginSnapshotAction();
    try {
      const vaultID = await readActionVaultID(operation.session);
      if (!operation.current()) return;
      await ActionJournal.abandon(vaultID);
      if (!operation.current()) return;
      snapshotActionsOpen = false;
    } catch (cause) {
      if (!operation.current()) return;
      if (cause instanceof APIError && cause.status === 401) handleFailure(cause);
      else snapshotActionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (operation.current()) {
        snapshotActionBusy = false;
        snapshotActionController = undefined;
      }
    }
  }

  async function openCollectionMember(
    member: Node,
    current: () => boolean,
  ): Promise<void> {
    const path = member.path;
    if (!path || !path.startsWith("/") || path === "/") {
      throw new Error("This collection member no longer has a live document path.");
    }
    const session = webSession;
    const exact = await statPath(session, path);
    if (session !== webSession || !current()) return;
    if (exact.id !== member.id || exact.kind !== "file") {
      throw new Error(
        "This collection member moved or its old path now belongs to another document. Reload the collection before opening it.",
      );
    }
    const split = path.lastIndexOf("/");
    const parentPath = split === 0 ? "/" : path.slice(0, split);
    const parent = await statPath(session, parentPath);
    if (session !== webSession || !current()) return;
    if (parent.kind !== "dir") {
      throw new Error("The collection member's parent is no longer a live directory.");
    }
    if (!current()) return;
    collectionsOpen = false;
    await loadDirectory(parent.id, true, exact.id, false, { node: exact, path });
  }

  function handleTrashed(_receipt: Node): void {
    trashTarget = null;
    selectNode(undefined);

    // Cached views may contain the removed node or pre-trash parent revisions.
    // Return to a freshly loaded root rather than leaving the current view
    // stranded without a valid Back destination.
    stack = [];
    directory = null;
    replaceRows([], false);
    searchQuery = "";
    activeQuery = "";
    tagFilterID = "";
    activeTagID = "";
    taggedInspected = 0;
    taggedTotal = 0;
    taggedTrashed = 0;
    truncated = false;
    void loadRoot();
    void loadTagCatalog();
  }

  function handleRestored(_receipt: Node): void {
    selectNode(undefined);

    // Restore can advance an arbitrary destination parent and make every
    // cached path snapshot stale. Keep the trash drawer's authoritative
    // receipt visible while reacquiring the live tree from root.
    stack = [];
    directory = null;
    replaceRows([], false);
    searchQuery = "";
    activeQuery = "";
    tagFilterID = "";
    activeTagID = "";
    taggedInspected = 0;
    taggedTotal = 0;
    taggedTrashed = 0;
    truncated = false;
    void loadRoot();
    void loadTagCatalog();
  }

  function handleTagChanged(
    receipt: TagAssignmentReceipt,
    assigned: boolean,
  ): void {
    const selectedChanged = selectedID === receipt.node.id;
    const modalChanged = manageTagsTarget?.node.id === receipt.node.id;
    if (!selectedChanged && !modalChanged) return;
    if (modalChanged && manageTagsTarget) {
      manageTagsTarget = {
        ...manageTagsTarget,
        node: receipt.node,
        path: receipt.node.path ?? manageTagsTarget.path,
      };
    }
    if (selectedChanged) {
      tagGeneration += 1;
      selectedTagsLoading = false;
      selectedTagsError = "";
    }
    replaceRows(
      rows.map((row) =>
        row.node.id === receipt.node.id
          ? {
              ...row,
              node: receipt.node,
              path: receipt.node.path ?? row.path,
            }
          : row,
      ),
      true,
    );
    if (selectedChanged) {
      if (assigned) {
        selectedTags = sortTags([
          ...selectedTags.filter((tag) => tag.id !== receipt.tag.id),
          receipt.tag,
        ]);
        if (receipt.changed) selectedTagsTotal += 1;
      } else {
        selectedTags = selectedTags.filter((tag) => tag.id !== receipt.tag.id);
        if (receipt.changed) {
          selectedTagsTotal = Math.max(0, selectedTagsTotal - 1);
        }
      }
    }
    tagCatalog = tagCatalog.map((tag) =>
      tag.id === receipt.tag.id ? receipt.tag : tag,
    );
    if (activeTagID === receipt.tag.id) {
      if (assigned) {
        const target = manageTagsTarget;
        if (target && !rows.some((row) => row.node.id === receipt.node.id)) {
          replaceRows([...rows, target], true);
          if (!activeQuery) {
            taggedInspected = rows.length;
            if (receipt.changed) taggedTotal += 1;
            taggedTotal = Math.max(rows.length, taggedTotal);
            truncated = taggedTotal > rows.length;
          }
          if (selectedID === undefined) selectNode(receipt.node.id);
        }
      } else {
        replaceRows(
          rows.filter((row) => row.node.id !== receipt.node.id),
          true,
        );
        if (!activeQuery) {
          taggedInspected = rows.length;
          taggedTotal = Math.max(rows.length, taggedTotal - 1);
          truncated = taggedTotal > rows.length;
        }
        if (selectedID === receipt.node.id) {
          selectNode(rows[0]?.node.id);
        }
      }
      if (activeQuery) void runSearch();
      else void loadTaggedNodes(receipt.tag.id);
    }
    void loadTagCatalog();
    if (selectedID === receipt.node.id) {
      void loadAuditStatus(receipt.node.id);
    }
  }

  function handleTagDefinitionChanged(change: TagDefinitionChange): void {
    const changedTag = change.tag;
    if (change.kind === "renamed") {
      tagCatalog = sortTags(
        tagCatalog.map((tag) =>
          tag.id === changedTag.id ? changedTag : tag,
        ),
      );
      selectedTags = sortTags(
        selectedTags.map((tag) =>
          tag.id === changedTag.id ? changedTag : tag,
        ),
      );
    } else if (change.kind === "deleted") {
      tagCatalog = tagCatalog.filter((tag) => tag.id !== changedTag.id);
      tagCatalogTotal = Math.max(0, tagCatalogTotal - 1);
      tagCatalogListed = Math.min(tagCatalogListed, tagCatalog.length);
      if (selectedTags.some((tag) => tag.id === changedTag.id)) {
        selectedTags = selectedTags.filter((tag) => tag.id !== changedTag.id);
        selectedTagsTotal = Math.max(0, selectedTagsTotal - 1);
      }
      stack = stack.map((snapshot) =>
        snapshot.tagFilterID === changedTag.id ||
        snapshot.activeTagID === changedTag.id
          ? {
              ...snapshot,
              tagFilterID:
                snapshot.tagFilterID === changedTag.id
                  ? ""
                  : snapshot.tagFilterID,
              activeTagID:
                snapshot.activeTagID === changedTag.id
                  ? ""
                  : snapshot.activeTagID,
            }
          : snapshot,
      );
    }

    void loadTagCatalog();
    if (change.kind === "created") return;

    const preferredSelectedID = selectedID;
    if (change.kind === "deleted" && activeTagID === changedTag.id) {
      clearBulkSelection();
      tagFilterID = "";
      activeTagID = "";
      taggedInspected = 0;
      taggedTotal = 0;
      taggedTrashed = 0;
    }
    if (activeQuery) void runSearch(preferredSelectedID);
    else if (activeTagID) {
      void loadTaggedNodes(activeTagID, preferredSelectedID);
    } else if (directory) {
      void loadDirectory(directory.id, false, preferredSelectedID, true);
    }
  }

  async function lock(): Promise<void> {
    leaveSnapshotMode();
    savedQueriesOpen = false;
    savedQueryDraft = null;
    replaceQueryURL(null);
    generation += 1;
    auditGeneration += 1;
    tagGeneration += 1;
    tagCatalogGeneration += 1;
    invalidateTagHotkeyMutation();
    const session = webSession;
    uploadChannel?.close();
    uploadChannel = null;
    webSession = "";
    directory = null;
    replaceRows([], false);
    stack = [];
    selectedID = undefined;
    selectedAudit = null;
    selectedTags = [];
    selectedTagsTotal = 0;
    selectedTagsLoading = false;
    selectedTagsError = "";
    tagCatalog = [];
    tagCatalogTotal = 0;
    tagCatalogListed = 0;
    tagCatalogLoading = false;
    tagCatalogError = "";
    vaultID = "";
    tagHotkeys = {};
    shortcutNotice = "";
    shortcutError = "";
    auditLoading = false;
    auditError = "";
    historyOpen = false;
    versionsOpen = false;
    provenanceOpen = false;
    processingTarget = null;
    renditionTarget = null;
    jobsOpen = false;
    auditEvidenceOpen = false;
    storageOpen = false;
    backupsOpen = false;
    collectionsOpen = false;
    trashOpen = false;
    manageTagsTarget = null;
    batchTagsTargets = null;
    batchTagsContext = "live";
    snapshotActionsOpen = false;
    snapshotActionBusy = false;
    snapshotActionError = "";
    recoveryJournal = null;
    recoveryAction = null;
    recoveryTag = null;
    recoveryVaultID = "";
    tagCatalogOpen = false;
    shortcutHelpOpen = false;
    uploadTarget = null;
    trashTarget = null;
    activeQuery = "";
    activeTagID = "";
    taggedInspected = 0;
    taggedTotal = 0;
    taggedTrashed = 0;
    searchPending = false;
    searchQuery = "";
    tagFilterID = "";
    error = "";
    try {
      await revokeSession(session);
    } catch {
      // The local UI is locked even if the daemon disappeared first. Its
      // in-memory session disappears with it.
    }
  }

  function currentQueryDraft(): Query {
    if (savedQueryDraft) return savedQueryDraft;
    if (snapshotState.query) return snapshotState.query;
    return {
      ...parseQuery("{}"),
      text: searchQuery,
      filters: tagFilterID ? { tag_ids: [tagFilterID] } : {},
      sort: { field: sortField === "modified" ? "modified_at" : sortField === "name" && (activeQuery !== "" || tagBrowse) ? "path" : sortField, direction: sortDirection },
    };
  }

  function keepQueryDraft(query: Query): void {
    replaceQueryURL(query);
    savedQueryDraft = query;
    queryEditorInitial = query;
    queryURLError = "";
  }

  function openSavedQueries(): void {
    queryEditorInitial = currentQueryDraft();
    savedQueriesOpen = true;
  }

  function openQueryEditor(value?: Query): void {
    try {
      keepQueryDraft(value ?? currentQueryDraft());
      queryBarOpen = true;
    } catch (cause) {
      queryURLError = cause instanceof Error ? cause.message : String(cause);
    }
  }

  function discardQueryDraft(): void {
    queryBarOpen = false;
    savedQueryDraft = null;
    replaceQueryURL(null);
  }
</script>

{#if !webSession}
  <main class="unlock-shell">
    <Card level="raised" title="Open your Docbank" eyebrow="LOCAL VAULT">
      <div class="unlock-copy">
        <p>
          Run <code>docbank web</code> to create a new scoped browser session.
          The vault API key is never stored in the browser.
        </p>
        {#if error}<p class="error" role="alert">{error}</p>{/if}
        {#if queryURLError}<p class="error" role="alert">Query URL could not be loaded: {queryURLError}</p>{/if}
      </div>
    </Card>
  </main>
{:else}
  <div class="app-shell">
    <TopBar>
      {#snippet left()}
        <div class="brand">
          <span class="brand-mark">D</span>
          <div>
            <strong>Docbank</strong>
            <span>documents for you and your agents</span>
          </div>
        </div>
      {/snippet}
      {#snippet search()}
        <form
          class="search"
          onsubmit={(event) => {
            event.preventDefault();
            void runSearch();
          }}
        >
          <SearchInput
            bind:value={searchQuery}
            bind:inputEl={searchInputEl}
            placeholder="Search names and extracted text"
            ariaLabel="Search documents"
            keys={formatShortcutKeys("/")}
            block
            onclear={clearSearch}
          />
        </form>
      {/snippet}
      {#snippet right()}
        <Button size="sm" onclick={() => openQueryEditor()}>Edit query</Button>
        <Button size="sm" disabled={snapshotActive && snapshotState.status !== "ready"}
          onclick={() => { snapshotActionError = ""; snapshotActionsOpen = true; }}>Snapshot actions</Button>
        <IconButton size="sm" ariaLabel="Saved queries and highlights" onclick={openSavedQueries}>
          <BookmarkIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton
          size="sm"
          ariaLabel="Import collections"
          onclick={() => {
            historyOpen = false;
            versionsOpen = false;
            provenanceOpen = false;
            jobsOpen = false;
            auditEvidenceOpen = false;
            storageOpen = false;
            backupsOpen = false;
            trashOpen = false;
            uploadTarget = null;
            trashTarget = null;
            collectionsOpen = true;
          }}
        >
          <FoldersIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton
          size="sm"
          ariaLabel="Recoverable trash"
          onclick={() => {
            historyOpen = false;
            versionsOpen = false;
            provenanceOpen = false;
            jobsOpen = false;
            auditEvidenceOpen = false;
            storageOpen = false;
            backupsOpen = false;
            uploadTarget = null;
            trashTarget = null;
            trashOpen = true;
          }}
        >
          <Trash2Icon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton
          size="sm"
          ariaLabel="Backup snapshots"
          onclick={() => {
            historyOpen = false;
            versionsOpen = false;
            provenanceOpen = false;
            jobsOpen = false;
            auditEvidenceOpen = false;
            storageOpen = false;
            trashOpen = false;
            backupsOpen = true;
            uploadTarget = null;
          }}
        >
          <ArchiveIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton
          size="sm"
          ariaLabel="Storage status"
          onclick={() => {
            historyOpen = false;
            versionsOpen = false;
            provenanceOpen = false;
            jobsOpen = false;
            auditEvidenceOpen = false;
            backupsOpen = false;
            trashOpen = false;
            uploadTarget = null;
            storageOpen = true;
          }}
        >
          <HardDriveIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton
          size="sm"
          ariaLabel="Background jobs"
          onclick={() => {
            historyOpen = false;
            versionsOpen = false;
            provenanceOpen = false;
            auditEvidenceOpen = false;
            storageOpen = false;
            backupsOpen = false;
            trashOpen = false;
            uploadTarget = null;
            jobsOpen = true;
          }}
        >
          <ActivityIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton
          size="sm"
          ariaLabel="Verify permanent audit evidence"
          onclick={() => {
            historyOpen = false;
            versionsOpen = false;
            provenanceOpen = false;
            jobsOpen = false;
            storageOpen = false;
            backupsOpen = false;
            trashOpen = false;
            uploadTarget = null;
            trashTarget = null;
            auditEvidenceOpen = true;
          }}
        >
          <ShieldCheckIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton
          size="sm"
          ariaLabel="Keyboard shortcuts and tag hotkeys"
          title="Keyboard shortcuts and tag hotkeys (?)"
          onclick={openShortcutHelp}
        >
          <KeyboardIcon size="14" aria-hidden="true" />
        </IconButton>
        <ThemeToggle size="sm" />
        <IconButton size="sm" ariaLabel="Lock web session" onclick={() => void lock()}>
          <LogOutIcon size="14" aria-hidden="true" />
        </IconButton>
      {/snippet}
    </TopBar>

    {#if queryURLError}<p class="error" role="alert">Query URL could not be loaded: {queryURLError}</p>{/if}
    {#if savedQueryDraft}
      <div class="query-draft-notice">
        <span>{snapshotActive ? "Query draft retained · Run to replace the accepted frozen snapshot" : "Query draft retained · not applied to live results"}</span>
        <Button size="sm" onclick={discardQueryDraft}>Discard query draft</Button>
      </div>
    {/if}

    {#if queryBarOpen && savedQueryDraft}
      <QueryBar session={webSession} query={savedQueryDraft} profile={snapshotState.options?.profile ?? ""}
        onchange={keepQueryDraft} onrun={runSnapshot} onsave={openSavedQueries}
        onclose={() => (queryBarOpen = false)} onauthfailure={handleFailure} />
    {/if}

    <main class="workspace">
      <Card class="browser" level="raised" padding="none" ariaLabel="Vault browser">
        {#if snapshotActive}
          <div class="browser-toolbar">
            <div class="location">
              <div>
                <span>Frozen query snapshot</span>
                <strong>{snapshotQuery?.text || "All documents"}</strong>
              </div>
            </div>
            <div class="toolbar-actions">
              {#if snapshotState.options?.profile}<span>Profile: {snapshotState.options.profile}</span>{/if}
              <Button size="sm" tone="info" disabled={snapshotState.status !== "ready"}
                onclick={() => { snapshotActionError = ""; snapshotActionsOpen = true; }}>Tag or recover</Button>
              <Button size="sm" onclick={leaveSnapshotMode}>Back to live folder</Button>
            </div>
          </div>
          {#if snapshotState.error}
            <div class="banner error" role="alert">{snapshotState.error.message}</div>
          {/if}
          {#if snapshotPage && snapshotQuery}
            <div class="snapshot-browser">
              <FacetSidebar facets={snapshotPage.facets} query={snapshotQuery}
                disabled={snapshotState.status !== "ready"} onchange={changeSnapshotQuery} />
              <section class="snapshot-results" aria-label="Frozen query results">
                {#if snapshotPage.rows.length === 0}
                  <EmptyState title="No matching documents" description="Change the query or a supported facet, then run another frozen snapshot.">
                    {#snippet icon()}<SearchIcon size="22" />{/snippet}
                  </EmptyState>
                {:else}
                  <Table class="snapshot-table" ariaLabel="Snapshot documents">
                    {#snippet header()}
                      <th class="selection-column" scope="col">
                        <Checkbox checked={allVisibleSnapshotRowsSelected}
                          indeterminate={selectedSnapshotRows.length > 0 && !allVisibleSnapshotRowsSelected}
                          ariaLabel="Select visible frozen documents" onchange={selectVisibleSnapshotRows} />
                      </th>
                      <TableHeaderCell label="Document" sortable
                        sortDirection={snapshotQuery.sort.field === "name" || snapshotQuery.sort.field === "path" ? snapshotQuery.sort.direction : null}
                        onsort={() => sortSnapshot("name")} />
                      <TableHeaderCell label="Type" sortable
                        sortDirection={snapshotQuery.sort.field === "media_type" ? snapshotQuery.sort.direction : null}
                        onsort={() => sortSnapshot("media_type")} />
                      <TableHeaderCell label="Size" numeric sortable
                        sortDirection={snapshotQuery.sort.field === "size" ? snapshotQuery.sort.direction : null}
                        onsort={() => sortSnapshot("size")} />
                      <TableHeaderCell label="Modified" sortable
                        sortDirection={snapshotQuery.sort.field === "modified_at" ? snapshotQuery.sort.direction : null}
                        onsort={() => sortSnapshot("modified_at")} />
                    {/snippet}
                    {#snippet children()}
                      {#each snapshotPage.rows as row (`${row.node_id}:${row.content_version_id}`)}
                        <tr class:selected={row.node_id === selectedSnapshotID} tabindex="0"
                          aria-selected={row.node_id === selectedSnapshotID} onclick={() => (selectedSnapshotID = row.node_id)}
                          onkeydown={(event) => { if (event.key === "Enter") selectedSnapshotID = row.node_id; }}>
                          <td class="selection-column" onclick={(event) => event.stopPropagation()}>
                            <Checkbox checked={snapshotSelection.has(row.node_id)} ariaLabel={`Select ${row.path}`}
                              onchange={(checked) => toggleSnapshotSelection(row, checked)} />
                          </td>
                          <td><span class="document-name"><FileIcon size="15" aria-hidden="true" /><span>{row.path}</span></span></td>
                          <td>{row.mime_type || "File"}</td>
                          <td class="numeric">{formatBytes(row.size)}</td>
                          <td>{formatDate(row.modified_at)}</td>
                        </tr>
                      {/each}
                    {/snippet}
                  </Table>
                {/if}
                <ResultsPager page={snapshotPage} offset={snapshotState.offset} status={snapshotState.status}
                  onpage={pageSnapshot} onrunagain={runSnapshotAgain} />
              </section>
            </div>
          {:else if snapshotState.status === "loading"}
            <div class="loading"><Spinner size={16} /> Creating frozen snapshot…</div>
          {:else}
            <div class="snapshot-failure">
              <p>No snapshot results were accepted.</p>
              {#if snapshotState.query && snapshotState.options}<Button onclick={runSnapshotAgain}>Run again</Button>{/if}
            </div>
          {/if}
        {:else}
        <div class="browser-toolbar">
          <div class="location">
            <IconButton
              size="sm"
              ariaLabel="Back to previous directory"
              disabled={stack.length === 0}
              onclick={goBack}
            >
              <ArrowLeftIcon size="14" aria-hidden="true" />
            </IconButton>
            <div>
              <span>
                {activeQuery ? "Search results" : tagBrowse ? "Documents tagged" : "Current folder"}
              </span>
              <strong>
                {activeQuery
                  ? `“${activeQuery}”${activeTag ? ` · ${activeTag.name}` : ""}`
                  : tagBrowse
                    ? activeTag?.name ?? activeTagID
                  : directory?.path ?? "/"}
              </strong>
            </div>
          </div>
          <div class="toolbar-actions">
            <TagPicker
              value={tagFilterID}
              tags={tagCatalog}
              title={tagPickerTitle}
              placeholder="All tags"
              includeAll
              disabled={!directory || tagCatalog.length === 0}
              onchange={changeTagFilter}
            />
            <IconButton
              size="sm"
              ariaLabel="Manage tag definitions"
              title="Create, rename, or delete tag definitions"
              disabled={loading || tagCatalogLoading}
              onclick={() => {
                if (loading || tagCatalogLoading) return;
                historyOpen = false;
                versionsOpen = false;
                provenanceOpen = false;
                jobsOpen = false;
                auditEvidenceOpen = false;
                storageOpen = false;
                backupsOpen = false;
                trashOpen = false;
                manageTagsTarget = null;
                uploadTarget = null;
                tagCatalogOpen = true;
              }}
            >
              <TagsIcon size="14" aria-hidden="true" />
            </IconButton>
            {#if tagBrowse}
              <span>
                {rows.length} live shown
                {#if taggedTrashed > 0} · {taggedTrashed} trashed omitted{/if}
                {#if truncated} · first {taggedInspected} of {taggedTotal} assignments{/if}
              </span>
            {:else}
              <span>{rows.length}{truncated ? "+" : ""} item{rows.length === 1 ? "" : "s"}</span>
            {/if}
            <IconButton
              size="sm"
              ariaLabel="Upload files to current folder"
              title={activeQuery || tagBrowse
                ? "Return to a folder before uploading"
                : uploadChannelError
                  ? uploadChannelError
                  : !uploadChannel
                    ? "Establishing the verified upload channel"
                : directory?.path
                  ? `Upload files to ${directory.path}`
                  : "Upload files"}
              disabled={!directory || loading || Boolean(activeQuery) || tagBrowse ||
                !uploadChannel || Boolean(uploadChannelError)}
              onclick={() => {
                if (!directory || activeQuery || tagBrowse) return;
                historyOpen = false;
                versionsOpen = false;
                provenanceOpen = false;
                jobsOpen = false;
                auditEvidenceOpen = false;
                storageOpen = false;
                backupsOpen = false;
                trashOpen = false;
                uploadTarget = directory;
              }}
            >
              <UploadIcon size="14" aria-hidden="true" />
            </IconButton>
            <IconButton
              size="sm"
              ariaLabel="Refresh current view"
              onclick={refreshCurrentView}
            >
              <RefreshCwIcon size="14" aria-hidden="true" />
            </IconButton>
          </div>
        </div>

        {#if error}
          <div class="banner error" role="alert">{error}</div>
        {/if}
        {#if shortcutError}
          <div class="banner error" role="alert" aria-label="Keyboard shortcut error">
            {shortcutError}
          </div>
        {/if}
        {#if shortcutNotice}
          <div
            class="banner shortcut-notice"
            role="status"
            aria-live="polite"
            aria-label="Keyboard shortcut status"
          >
            {shortcutNotice}
          </div>
        {/if}
        {#if loading}
          <div class="loading"><Spinner size={16} /> Loading vault…</div>
        {:else if rows.length === 0}
          <EmptyState
            title={activeQuery
              ? "No matching documents"
              : tagBrowse
                ? "No live documents carry this tag"
                : "This folder is empty"}
            description={activeQuery
              ? "Try another name or phrase from extracted text."
              : tagBrowse
                ? taggedTrashed > 0
                  ? `${taggedTrashed} trashed assignment${taggedTrashed === 1 ? " is" : "s are"} omitted from this live view.`
                  : "Choose another tag or return to the current folder."
                : "Use the CLI, API, or an agent to file documents here."}
          >
            {#snippet icon()}
              {#if activeQuery}
                <SearchIcon size="22" />
              {:else if tagBrowse}
                <TagIcon size="22" />
              {:else}
                <FolderIcon size="22" />
              {/if}
            {/snippet}
          </EmptyState>
        {:else}
          <Table ariaLabel="Documents">
            {#snippet header()}
              <th class="selection-column" scope="col">
                <Checkbox
                  checked={allVisibleDocumentsSelected}
                  disabled={visibleDocumentCount === 0}
                  indeterminate={selectedCount > 0 && !allVisibleDocumentsSelected}
                  ariaLabel="Select visible documents"
                  onchange={selectAllVisibleDocuments}
                />
              </th>
              <TableHeaderCell
                label="Document"
                sortable
                sortDirection={sortField === "name" ? sortDirection : null}
                onsort={() => sortBy("name")}
              />
              <TableHeaderCell label="Type" />
              <TableHeaderCell
                label="Size"
                numeric
                sortable
                sortDirection={sortField === "size" ? sortDirection : null}
                onsort={() => sortBy("size")}
              />
              <TableHeaderCell
                label="Modified"
                sortable
                sortDirection={sortField === "modified" ? sortDirection : null}
                onsort={() => sortBy("modified")}
              />
              {#if activeQuery}<TableHeaderCell label="Match" />{/if}
            {/snippet}
            {#snippet children()}
              {#each sortedRows as row (row.node.id)}
                <tr
                  class:selected={row.node.id === selectedID}
                  data-node-id={row.node.id}
                  tabindex="0"
                  aria-selected={row.node.id === selectedID}
                  ondblclick={() => activate(row)}
                  onclick={() => selectNode(row.node.id)}
                  onkeydown={(event) => {
                    if (event.key === "Enter") {
                      event.preventDefault();
                      event.stopPropagation();
                      activate(row);
                    }
                  }}
                >
                  <td
                    class="selection-column"
                    onclick={(event) => {
                      event.stopPropagation();
                      pendingSelectionRange = event.shiftKey;
                    }}
                    ondblclick={(event) => event.stopPropagation()}
                    onkeydown={(event) => event.stopPropagation()}
                  >
                    {#if row.node.kind === "file"}
                      <Checkbox
                        checked={bulkSelection.selectedIDs.has(row.node.id)}
                        ariaLabel={`Select ${activeQuery || tagBrowse ? row.path : row.node.name}`}
                        onchange={(checked) => toggleBulkSelection(row, checked)}
                      />
                    {/if}
                  </td>
                  <td>
                    <span class="document-name">
                      {#if row.node.kind === "dir"}
                        <FolderIcon size="15" aria-hidden="true" />
                      {:else}
                        <FileIcon size="15" aria-hidden="true" />
                      {/if}
                      <span>{activeQuery || tagBrowse ? row.path : row.node.name}</span>
                    </span>
                  </td>
                  <td>{row.node.kind === "dir" ? "Folder" : row.node.mime_type || "File"}</td>
                  <td class="numeric">{row.node.kind === "dir" ? "—" : formatBytes(row.node.size)}</td>
                  <td>{formatDate(row.node.modified_at)}</td>
                  {#if activeQuery}
                    <td><Chip size="xs" tone={row.match === "content" ? "info" : "neutral"}>{row.match}</Chip></td>
                  {/if}
                </tr>
              {/each}
            {/snippet}
          </Table>
        {/if}
        {/if}
      </Card>

      <aside class="detail" aria-label="Document authority">
        {#if selectedSnapshot}
          <Card level="raised" padding="sm" ariaLabel={`Frozen snapshot authority for ${selectedSnapshot.name}`}>
            <div class="authority-content">
              <header class="authority-header">
                <div><span>Frozen snapshot authority</span><Chip size="xs" tone="muted" uppercase={false}>id:{selectedSnapshot.node_id}</Chip></div>
                <h2>{selectedSnapshot.name}</h2>
              </header>
              <dl>
                <div class="wide-fact"><dt>Path</dt><dd>{selectedSnapshot.path}</dd></div>
                <div><dt>Revision</dt><dd>{selectedSnapshot.revision}</dd></div>
                {#if selectedSnapshotOverlay}
                  <div><dt>Confirmed later</dt><dd>Revision {selectedSnapshotOverlay.revision}</dd></div>
                {/if}
                <div><dt>Observed</dt><dd>{formatDate(snapshotState.page?.observed_at ?? "")}</dd></div>
                <div><dt>Size</dt><dd>{formatBytes(selectedSnapshot.size)} ({selectedSnapshot.size} bytes)</dd></div>
                <div><dt>Media type</dt><dd>{selectedSnapshot.mime_type || "application/octet-stream"}</dd></div>
                <div class="identity"><dt>Version</dt><dd><code>{selectedSnapshot.content_version_id}</code><CopyButton text={selectedSnapshot.content_version_id} ariaLabel="Copy snapshot version ID" /></dd></div>
                <div class="identity"><dt>SHA-256</dt><dd><code>{selectedSnapshot.blob_hash}</code><CopyButton text={selectedSnapshot.blob_hash} ariaLabel="Copy snapshot SHA-256" /></dd></div>
                <div class="identity"><dt>Snapshot</dt><dd><code>{snapshotState.page?.snapshot_id}</code></dd></div>
              </dl>
              <div class="node-tags">
                <div class="node-tags-heading"><span><TagIcon size="13" aria-hidden="true" /> Tags at observation</span><span>{selectedSnapshot.tags.length} assigned</span></div>
                {#if selectedSnapshot.tags.length === 0}<p>No tags were recorded in this snapshot.</p>
                {:else}<div class="snapshot-tags">{#each selectedSnapshot.tags as tag (tag.id)}<Chip size="sm" uppercase={false}>{tag.name}</Chip>{/each}</div>{/if}
              </div>
              {#if selectedSnapshotOverlay}
                <div class="node-tags">
                  <div class="node-tags-heading"><span><TagIcon size="13" aria-hidden="true" /> Validated receipt overlays</span><span>after frozen observation</span></div>
                  <div class="snapshot-tags">
                    {#each Object.entries(selectedSnapshotOverlay.assignments) as [tagID, assignment] (tagID)}
                      <Chip size="sm" tone={assignment.assign ? "success" : "muted"} uppercase={false}>
                        {assignment.assign ? "Added" : "Removed"}: {assignment.label}
                      </Chip>
                    {/each}
                  </div>
                </div>
              {/if}
            </div>
          </Card>
        {:else if !snapshotActive && selected}
          <Card
            level="raised"
            padding="sm"
            ariaLabel={`${selected.node.kind === "dir" ? "Folder" : "Document authority"} for ${basename(selected.path)}`}
          >
            <div class="authority-content">
              <header class="authority-header">
                <div>
                  <span>{selected.node.kind === "dir" ? "Folder" : "Document authority"}</span>
                  <Chip size="xs" tone="muted" uppercase={false}>id:{selected.node.id}</Chip>
                </div>
                <h2>{basename(selected.path)}</h2>
              </header>
              <dl>
                <div class="wide-fact"><dt>Path</dt><dd>{selected.path}</dd></div>
                <div><dt>Revision</dt><dd>{selected.node.revision}</dd></div>
                <div><dt>Modified</dt><dd>{formatDate(selected.node.modified_at)}</dd></div>
                {#if selected.node.kind === "file"}
                  <div><dt>Size</dt><dd>{formatBytes(selected.node.size)} ({selected.node.size} bytes)</dd></div>
                  <div><dt>Media type</dt><dd>{selected.node.mime_type || "application/octet-stream"}</dd></div>
                  <div class="identity">
                    <dt>Version</dt>
                    <dd>
                      <code>{selected.node.current_version_id}</code>
                      {#if selected.node.current_version_id}
                        <CopyButton text={selected.node.current_version_id} ariaLabel="Copy version ID" />
                      {/if}
                    </dd>
                  </div>
                  <div class="identity">
                    <dt>SHA-256</dt>
                    <dd>
                      <code>{selected.node.blob_hash}</code>
                      {#if selected.node.blob_hash}
                        <CopyButton text={selected.node.blob_hash} ariaLabel="Copy SHA-256" />
                      {/if}
                    </dd>
                  </div>
                {/if}
              </dl>
              <div class="node-tags">
                <div class="node-tags-heading">
                  <span><TagIcon size="13" aria-hidden="true" /> Tags</span>
                  <div class="node-tags-controls">
                    {#if selectedTagsLoading}
                      <Spinner size={13} />
                    {:else if selectedTagsError}
                      <Chip size="xs" tone="warning">Unavailable</Chip>
                    {:else}
                      <span>
                        {selectedTags.length < selectedTagsTotal
                          ? `${selectedTags.length} of ${selectedTagsTotal}`
                          : selectedTagsTotal}
                        assigned
                      </span>
                    {/if}
                    <Button
                      size="sm"
                      tone="info"
                      surface="soft"
                      disabled={loading || selectedTagsLoading || selectedTagsError !== "" || pendingTagHotkey !== ""}
                      onclick={() => {
                        if (!loading && selected) manageTagsTarget = selected;
                      }}
                    >
                      Manage
                    </Button>
                  </div>
                </div>
                {#if selectedTagsError}
                  <p>{selectedTagsError}</p>
                {:else if !selectedTagsLoading && selectedTags.length === 0}
                  <p>No tags are assigned to this node.</p>
                {:else if selectedTags.length > 0}
                  <ChipStack
                    items={selectedTags}
                    key={(tag) => tag.id}
                    maxVisible={6}
                    size="sm"
                    ariaLabel="Assigned tags"
                  >
                    {#snippet chip(tag)}
                      <TagLabel
                        {tag}
                        size="sm"
                        title={`${tag.name} · ${tag.assignment_count} total assignment${tag.assignment_count === 1 ? "" : "s"} · ${tag.id}`}
                      />
                    {/snippet}
                  </ChipStack>
                  {#if selectedTags.length < selectedTagsTotal}
                    <p>Showing the first {selectedTags.length} assigned tags.</p>
                  {/if}
                {/if}
              </div>
              {#if selected.node.kind === "file"}
                <div class="document-actions">
                  {#key selected.node.id}
                    <DownloadButton
                      session={webSession}
                      node={selected.node}
                      onauthfailure={handleFailure}
                    />
                  {/key}
                  <Button
                    size="sm"
                    tone="info"
                    surface="soft"
                    onclick={() => {
                      historyOpen = false;
                      provenanceOpen = false;
                      jobsOpen = false;
                      auditEvidenceOpen = false;
                      storageOpen = false;
                      backupsOpen = false;
                      trashOpen = false;
                      uploadTarget = null;
                      versionsOpen = true;
                    }}
                  >
                    <HistoryIcon size="14" aria-hidden="true" />
                    Version history
                  </Button>
                  <Button
                    size="sm"
                    surface="soft"
                    onclick={() => {
                      historyOpen = false;
                      versionsOpen = false;
                      jobsOpen = false;
                      auditEvidenceOpen = false;
                      storageOpen = false;
                      backupsOpen = false;
                      trashOpen = false;
                      uploadTarget = null;
                      provenanceOpen = true;
                    }}
                  >
                    <MapPinIcon size="14" aria-hidden="true" />
                    Provenance
                  </Button>
                  <Button
                    size="sm"
                    tone="info"
                    surface="soft"
                    disabled={!selected.node.current_version_id}
                    onclick={() => {
                      historyOpen = false;
                      versionsOpen = false;
                      provenanceOpen = false;
                      jobsOpen = false;
                      auditEvidenceOpen = false;
                      storageOpen = false;
                      backupsOpen = false;
                      trashOpen = false;
                      uploadTarget = null;
                      renditionTarget = null;
                      processingTarget = selected;
                    }}
                  >
                    <ActivityIcon size="14" aria-hidden="true" />
                    Process and retrieve
                  </Button>
                  <Button
                    size="sm"
                    tone="danger"
                    surface="soft"
                    onclick={() => {
                      historyOpen = false;
                      versionsOpen = false;
                      provenanceOpen = false;
                      jobsOpen = false;
                      auditEvidenceOpen = false;
                      storageOpen = false;
                      backupsOpen = false;
                      trashOpen = false;
                      uploadTarget = null;
                      trashTarget = selected;
                    }}
                  >
                    <Trash2Icon size="14" aria-hidden="true" />
                    Move to trash
                  </Button>
                </div>
              {/if}
              <div class="audit-protection">
                <div class="audit-protection-heading">
                  <span>Permanent audit</span>
                  {#if auditLoading}
                    <Spinner size={14} />
                  {:else if auditError}
                    <Chip size="xs" tone="warning">Unavailable</Chip>
                  {:else if membership?.protected}
                    <Chip size="xs" tone="success" dot>Protected</Chip>
                  {:else if selectedAudit?.enabled}
                    <Chip size="xs" tone="muted">Not audited</Chip>
                  {:else}
                    <Chip size="xs" tone="muted">Dormant</Chip>
                  {/if}
                </div>
                {#if auditError}
                  <p>{auditError}</p>
                {:else if membership?.protected}
                  <p>
                    Permanently protected by {membership.scope_ids.length}
                    scope{membership.scope_ids.length === 1 ? "" : "s"}.
                  </p>
                  <Button
                    size="sm"
                    tone="info"
                    surface="soft"
                    onclick={() => {
                      jobsOpen = false;
                      versionsOpen = false;
                      provenanceOpen = false;
                      auditEvidenceOpen = false;
                      storageOpen = false;
                      backupsOpen = false;
                      trashOpen = false;
                      uploadTarget = null;
                      historyOpen = true;
                    }}
                  >
                    <HistoryIcon size="14" aria-hidden="true" />
                    Audit history
                  </Button>
                {:else if selectedAudit?.enabled}
                  <p>This node is outside every permanent audit scope.</p>
                {:else if !auditLoading}
                  <p>No permanent audit scope has been enabled for this vault.</p>
                {/if}
              </div>
              {#if selected.node.kind === "dir"}
                <div class="directory-actions">
                  <Button size="sm" onclick={() => activate(selected)}>
                    <FolderIcon size="14" aria-hidden="true" />
                    Open folder
                  </Button>
                  <Button
                    size="sm"
                    tone="danger"
                    surface="soft"
                    onclick={() => {
                      historyOpen = false;
                      versionsOpen = false;
                      provenanceOpen = false;
                      jobsOpen = false;
                      auditEvidenceOpen = false;
                      storageOpen = false;
                      backupsOpen = false;
                      trashOpen = false;
                      uploadTarget = null;
                      trashTarget = selected;
                    }}
                  >
                    <Trash2Icon size="14" aria-hidden="true" />
                    Move to trash
                  </Button>
                </div>
              {/if}
            </div>
          </Card>
        {:else}
          <Card level="raised" title="Document authority">
            <EmptyState
              title="Select a document"
              description="Choose a row to inspect its stable identity, current version, and verified content hash."
            >
              {#snippet icon()}<FileIcon size="22" />{/snippet}
            </EmptyState>
          </Card>
        {/if}
      </aside>
    </main>
    {#if snapshotActive && snapshotTargets.length > 0}
      <SelectionDock
        selectedCount={snapshotTargets.length}
        visibleDocumentCount={snapshotPage?.rows.length ?? 0}
        truncated={(snapshotPage?.total ?? 0) > (snapshotPage?.rows.length ?? 0)}
        context="snapshot"
        wholeQueryCount={snapshotPage?.total ?? 0}
        onclear={() => selectVisibleSnapshotRows(false)}
        onselectvisible={() => selectVisibleSnapshotRows()}
        tagsDisabled={snapshotState.status !== "ready"}
        ontags={() => openSnapshotBatchTags()}
        onwholequerytags={() => { snapshotActionError = ""; snapshotActionsOpen = true; }}
      />
    {/if}
    {#if !snapshotActive && selectedCount > 0}
      <SelectionDock
        {selectedCount}
        {visibleDocumentCount}
        {truncated}
        onclear={clearBulkSelection}
        onselectvisible={() => selectAllVisibleDocuments()}
        ontags={() => openBatchTags(bulkTargets)}
        tagsDisabled={loading || pendingTagHotkey !== ""}
        oncsv={exportPageCSV}
      />
    {/if}
    {#if shortcutHelpOpen}
      <ShortcutHelpModal
        vaultReady={vaultID !== ""}
        catalog={tagCatalog}
        catalogTotal={tagCatalogTotal}
        bindings={tagHotkeys}
        onbindingchange={changeTagHotkeyBinding}
        onclose={() => (shortcutHelpOpen = false)}
      />
    {/if}
    {#if !snapshotActive && historyOpen && selected && membership?.protected}
      <AuditHistoryDrawer
        session={webSession}
        node={selected.node}
        path={selected.path}
        onclose={() => (historyOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if !snapshotActive && versionsOpen && selected?.node.kind === "file"}
      <VersionHistoryDrawer
        session={webSession}
        node={selected.node}
        path={selected.path}
        onclose={() => (versionsOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if !snapshotActive && provenanceOpen && selected?.node.kind === "file"}
      <ProvenanceDrawer
        session={webSession}
        node={selected.node}
        path={selected.path}
        onclose={() => (provenanceOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if processingTarget?.node.kind === "file"}
      <ProcessingDrawer
        session={webSession}
        node={processingTarget.node}
        path={processingTarget.path}
        onclose={() => (processingTarget = null)}
        onauthfailure={handleFailure}
        onrendition={(attachmentID) => {
          if (!processingTarget) return;
          renditionTarget = { attachmentID, path: processingTarget.path };
          processingTarget = null;
        }}
      />
    {/if}
    {#if renditionTarget}
      <RenditionDrawer
        session={webSession}
        attachmentID={renditionTarget.attachmentID}
        path={renditionTarget.path}
        onclose={() => (renditionTarget = null)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if jobsOpen}
      <JobsDrawer
        session={webSession}
        onclose={() => (jobsOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if collectionsOpen}
      <CollectionsDrawer
        session={webSession}
        onclose={() => (collectionsOpen = false)}
        onauthfailure={handleFailure}
        onopenmember={openCollectionMember}
        onqualityquery={(query) => {openQueryEditor(query);collectionsOpen=false;}}
        onnewquery={(id) => {
          openQueryEditor(parseQuery(JSON.stringify({filters:{collection_ids:[id]}})));
          collectionsOpen = false;
        }}
      />
    {/if}
    {#if auditEvidenceOpen}
      <AuditEvidenceDrawer
        session={webSession}
        onclose={() => (auditEvidenceOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if savedQueriesOpen}
      <SavedQueriesDrawer
        session={webSession}
        initialQuery={queryEditorInitial}
        onload={keepQueryDraft}
        onopenquery={(query) => { openQueryEditor(query); savedQueriesOpen = false; }}
        onclose={() => (savedQueriesOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if backupsOpen}
      <BackupDrawer
        session={webSession}
        onclose={() => (backupsOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if storageOpen}
      <StorageDrawer
        session={webSession}
        onclose={() => (storageOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if trashOpen}
      <TrashDrawer
        session={webSession}
        onclose={() => (trashOpen = false)}
        onrestored={handleRestored}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if manageTagsTarget}
      <ManageTagsModal
        session={webSession}
        node={manageTagsTarget.node}
        catalog={tagCatalog}
        catalogTotal={tagCatalogTotal}
        assignedTags={selectedTags}
        assignedTotal={selectedTagsTotal}
        disabled={loading}
        onclose={() => (manageTagsTarget = null)}
        onchanged={handleTagChanged}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if batchTagsTargets}
      <BatchTagsModal
        session={webSession}
        targets={batchTagsTargets}
        catalog={tagCatalog}
        catalogTotal={tagCatalogTotal}
        disabled={loading || (batchTagsContext === "snapshot" && snapshotState.status !== "ready")}
        context={batchTagsContext}
        initialChoice={batchTagsChoice}
        onclose={() => (batchTagsTargets = null)}
        onchanged={handleBatchTagsChanged}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if snapshotActionsOpen}
      <SnapshotActions
        selectedCount={snapshotTargets.length}
        total={snapshotState.firstPage?.total ?? 0}
        catalog={tagCatalog}
        catalogTotal={tagCatalogTotal}
        disabled={snapshotActionBusy || (snapshotActive && snapshotState.status !== "ready")}
        errorMessage={snapshotActionError}
        onstart={(choice) => void startSnapshotAction(choice)}
        onimport={(bytes) => void importSnapshotAction(bytes)}
        onresume={() => void resumeSnapshotAction()}
        onabandon={() => void abandonSnapshotAction()}
        onclose={cancelSnapshotAction}
      />
    {/if}
    {#if recoveryJournal && recoveryAction}
      <ActionRecoveryModal
        session={webSession}
        sessionVaultID={recoveryVaultID}
        journal={recoveryJournal}
        initialAction={recoveryAction}
        tag={recoveryTag}
        disabled={snapshotActive && snapshotState.status !== "ready"}
        onprogress={handleRecoveryProgress}
        onclose={closeRecovery}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if tagCatalogOpen}
      <TagCatalogModal
        session={webSession}
        catalog={tagCatalog}
        catalogTotal={tagCatalogTotal}
        disabled={loading || tagCatalogLoading}
        onclose={() => (tagCatalogOpen = false)}
        onchanged={handleTagDefinitionChanged}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if uploadTarget && uploadChannel}
      <UploadDrawer
        channel={uploadChannel}
        directory={uploadTarget}
        disabledReason={uploadChannelError}
        onclose={() => (uploadTarget = null)}
        oncomplete={async () => {
          if (uploadTarget) await loadDirectory(uploadTarget.id, false);
        }}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if trashTarget}
      <TrashNodeModal
        session={webSession}
        node={trashTarget.node}
        path={trashTarget.path}
        onclose={() => (trashTarget = null)}
        ontrashed={handleTrashed}
        onauthfailure={handleFailure}
      />
    {/if}
  </div>
{/if}

<style>
  :global(.browser th.selection-column),
  :global(.browser td.selection-column) {
    width: 38px;
    min-width: 38px;
    padding: 6px 8px;
    text-align: center;
    cursor: default;
  }

  :global(.browser th.selection-column) {
    background: var(--bg-inset);
    border-bottom: 1px solid var(--border-default);
  }

  .shortcut-notice {
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-inset);
  }
</style>
