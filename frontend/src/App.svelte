<script lang="ts">
  import { onMount } from "svelte";
  import ActivityIcon from "@lucide/svelte/icons/activity";
  import ArchiveIcon from "@lucide/svelte/icons/archive";
  import ArrowLeftIcon from "@lucide/svelte/icons/arrow-left";
  import FileIcon from "@lucide/svelte/icons/file";
  import FolderIcon from "@lucide/svelte/icons/folder";
  import HardDriveIcon from "@lucide/svelte/icons/hard-drive";
  import LogOutIcon from "@lucide/svelte/icons/log-out";
  import MapPinIcon from "@lucide/svelte/icons/map-pin";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import SearchIcon from "@lucide/svelte/icons/search";
  import ShieldCheckIcon from "@lucide/svelte/icons/shield-check";
  import TagIcon from "@lucide/svelte/icons/tag";
  import TagsIcon from "@lucide/svelte/icons/tags";
  import HistoryIcon from "@lucide/svelte/icons/history";
  import Trash2Icon from "@lucide/svelte/icons/trash-2";
  import UploadIcon from "@lucide/svelte/icons/upload";
  import {
    Button,
    Card,
    Chip,
    ChipStack,
    CopyButton,
    EmptyState,
    IconButton,
    SearchInput,
    SelectDropdown,
    Spinner,
    Table,
    TableHeaderCell,
    ThemeToggle,
    TopBar,
    type SelectDropdownOption,
    type SortDirection,
  } from "@kenn-io/kit-ui";
  import AuditEvidenceDrawer from "./AuditEvidenceDrawer.svelte";
  import AuditHistoryDrawer from "./AuditHistoryDrawer.svelte";
  import BackupDrawer from "./BackupDrawer.svelte";
  import DownloadButton from "./DownloadButton.svelte";
  import JobsDrawer from "./JobsDrawer.svelte";
  import ManageTagsModal from "./ManageTagsModal.svelte";
  import ProvenanceDrawer from "./ProvenanceDrawer.svelte";
  import StorageDrawer from "./StorageDrawer.svelte";
  import TagCatalogModal, {
    type TagDefinitionChange,
  } from "./TagCatalogModal.svelte";
  import TrashDrawer from "./TrashDrawer.svelte";
  import TrashNodeModal from "./TrashNodeModal.svelte";
  import UploadDrawer from "./UploadDrawer.svelte";
  import VersionHistoryDrawer from "./VersionHistoryDrawer.svelte";
  import {
    APIError,
    auditStatusForNode,
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
  import { basename, formatBytes, formatDate } from "./format.js";
  import { orderRows, reconcileSearchView, type SortField } from "./rows.js";
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
  let jobsOpen = $state(false);
  let auditEvidenceOpen = $state(false);
  let storageOpen = $state(false);
  let backupsOpen = $state(false);
  let trashOpen = $state(false);
  let manageTagsTarget = $state<Row | null>(null);
  let tagCatalogOpen = $state(false);
  let uploadTarget = $state<Node | null>(null);
  let trashTarget = $state<Row | null>(null);
  let generation = 0;
  let auditGeneration = 0;
  let tagGeneration = 0;
  let tagCatalogGeneration = 0;

  const selected = $derived(rows.find((row) => row.node.id === selectedID));
  const membership = $derived(selectedAudit?.membership);
  const tagOptions = $derived<SelectDropdownOption[]>([
    { value: "", label: "All tags" },
    ...tagCatalog.map((tag) => ({
      value: tag.id,
      label: `${tag.name} (${tag.assignment_count})`,
      triggerLabel: tag.name,
    })),
  ]);
  const activeTag = $derived(tagCatalog.find((tag) => tag.id === activeTagID));
  const tagBrowse = $derived(activeTagID !== "" && activeQuery === "");
  const sortedRows = $derived(
    orderRows(rows, sortField, sortDirection, activeQuery !== "" || tagBrowse),
  );

  onMount(() => {
    const session = takeFragmentSession();
    if (session) {
      webSession = session.token;
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
      return () => channel.close();
    }
  });

  function handleFailure(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      uploadChannel?.close();
      webSession = "";
      uploadChannel = null;
      historyOpen = false;
      versionsOpen = false;
      provenanceOpen = false;
      jobsOpen = false;
      auditEvidenceOpen = false;
      storageOpen = false;
      backupsOpen = false;
      trashOpen = false;
      tagCatalogOpen = false;
      uploadTarget = null;
      trashTarget = null;
      tagCatalog = [];
      tagCatalogListed = 0;
      tagCatalogLoading = false;
      selectedTags = [];
      selectedTagsTotal = 0;
      searchPending = false;
      error = "The browser session expired or was rejected. Run `docbank web` again.";
      return;
    }
    error = cause instanceof Error ? cause.message : String(cause);
  }

  async function loadRoot(): Promise<void> {
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
  ): Promise<void> {
    const request = ++generation;
    searchPending = false;
    loading = true;
    error = "";
    try {
      const page = await children(webSession, nodeID);
      if (request !== generation) return;
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
      rows = page.items.map((item) => ({
        node: item,
        path: path === "/" ? `/${item.name}` : `${path}/${item.name}`,
      }));
      selectNode(
        rows.some((row) => row.node.id === preferredSelectedID)
          ? preferredSelectedID
          : rows[0]?.node.id,
      );
      activeQuery = "";
      activeTagID = "";
      taggedInspected = 0;
      taggedTotal = 0;
      taggedTrashed = 0;
      truncated = page.total > page.items.length;
      if (!preserveSort) {
        sortField = "name";
        sortDirection = "asc";
      }
    } catch (cause) {
      if (request === generation) {
        if (cause instanceof APIError && cause.status === 404) {
          rows = [];
          selectedID = undefined;
          error = "This directory was moved to trash or removed. Go back or reload the vault.";
        } else {
          handleFailure(cause);
        }
      }
    } finally {
      if (request === generation) loading = false;
    }
  }

  async function runSearch(preferredSelectedID = selectedID): Promise<void> {
    const query = searchQuery.trim();
    if (!query) {
      if (tagFilterID) await loadTaggedNodes(tagFilterID);
      else if (directory) await loadDirectory(directory.id, false);
      return;
    }
    const request = ++generation;
    const requestedTagID = tagFilterID;
    searchPending = true;
    loading = true;
    error = "";
    try {
      const report = await search(webSession, query, requestedTagID);
      if (request !== generation) return;
      if ((report.tag_id ?? "") !== requestedTagID) {
        throw new Error("Search results did not honor the selected tag filter.");
      }
      rows = report.hits.map((hit: SearchHit) => ({
        node: hit.node,
        path: hit.path,
        match: hit.match,
      }));
      const view = reconcileSearchView(
        rows,
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
      rows = liveRows;
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
    generation += 1;
    searchPending = false;
    const previous = stack.at(-1);
    if (!previous) return;
    const preferredSelectedID = previous.selectedID;
    selectNode(undefined);
    directory = previous.directory;
    rows = [];
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
    if (selectedID !== nodeID) {
      historyOpen = false;
      versionsOpen = false;
      provenanceOpen = false;
    }
    selectedID = nodeID;
    selectedAudit = null;
    selectedTags = [];
    selectedTagsTotal = 0;
    selectedTagsError = "";
    auditError = "";
    auditGeneration += 1;
    tagGeneration += 1;
    if (nodeID !== undefined && webSession) void loadAuditStatus(nodeID);
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
      tagCatalog = selectedTagID === tagFilterID ? items : page.items;
      tagCatalogTotal = page.total;
      tagCatalogListed = page.items.length;
      if (selectedMissing && tagFilterID === selectedTagID) {
        const rerunSearch = Boolean(activeQuery || searchPending);
        const leaveTagBrowse = activeQuery === "" && activeTagID === selectedTagID;
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
      selectedTags = page.items;
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

  async function loadAuditStatus(nodeID: number): Promise<void> {
    const request = ++auditGeneration;
    const session = webSession;
    auditLoading = true;
    try {
      const status = await auditStatusForNode(session, nodeID);
      if (request !== auditGeneration || session !== webSession || selectedID !== nodeID) return;
      selectedAudit = status;
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

  function handleTrashed(_receipt: Node): void {
    trashTarget = null;
    selectNode(undefined);

    // Cached views may contain the removed node or pre-trash parent revisions.
    // Return to a freshly loaded root rather than leaving the current view
    // stranded without a valid Back destination.
    stack = [];
    directory = null;
    rows = [];
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
    rows = [];
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
    rows = rows.map((row) =>
      row.node.id === receipt.node.id
        ? {
            ...row,
            node: receipt.node,
            path: receipt.node.path ?? row.path,
          }
        : row,
    );
    if (selectedChanged) {
      if (assigned) {
        selectedTags = [
          ...selectedTags.filter((tag) => tag.id !== receipt.tag.id),
          receipt.tag,
        ].sort((left, right) => left.name.localeCompare(right.name));
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
          rows = [...rows, target];
          if (!activeQuery) {
            taggedInspected = rows.length;
            if (receipt.changed) taggedTotal += 1;
            taggedTotal = Math.max(rows.length, taggedTotal);
            truncated = taggedTotal > rows.length;
          }
          if (selectedID === undefined) selectNode(receipt.node.id);
        }
      } else {
        rows = rows.filter((row) => row.node.id !== receipt.node.id);
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
      tagCatalog = tagCatalog
        .map((tag) => (tag.id === changedTag.id ? changedTag : tag))
        .sort((left, right) => left.name.localeCompare(right.name));
      selectedTags = selectedTags
        .map((tag) => (tag.id === changedTag.id ? changedTag : tag))
        .sort((left, right) => left.name.localeCompare(right.name));
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
    generation += 1;
    auditGeneration += 1;
    tagGeneration += 1;
    tagCatalogGeneration += 1;
    const session = webSession;
    uploadChannel?.close();
    uploadChannel = null;
    webSession = "";
    directory = null;
    rows = [];
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
    auditLoading = false;
    auditError = "";
    historyOpen = false;
    versionsOpen = false;
    provenanceOpen = false;
    jobsOpen = false;
    auditEvidenceOpen = false;
    storageOpen = false;
    backupsOpen = false;
    trashOpen = false;
    manageTagsTarget = null;
    tagCatalogOpen = false;
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
            placeholder="Search names and extracted text"
            ariaLabel="Search documents"
            block
            onclear={clearSearch}
          />
        </form>
      {/snippet}
      {#snippet right()}
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
        <ThemeToggle size="sm" />
        <IconButton size="sm" ariaLabel="Lock web session" onclick={() => void lock()}>
          <LogOutIcon size="14" aria-hidden="true" />
        </IconButton>
      {/snippet}
    </TopBar>

    <main class="workspace">
      <Card class="browser" level="raised" padding="none" ariaLabel="Vault browser">
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
            <SelectDropdown
              value={tagFilterID}
              options={tagOptions}
              title={tagCatalogError
                ? `Tags unavailable: ${tagCatalogError}`
                : tagCatalogTotal > tagCatalogListed
                  ? `Browse or filter: showing ${tagCatalogListed} of ${tagCatalogTotal} tags`
                  : "Browse or filter by tag"}
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
                  tabindex="0"
                  aria-selected={row.node.id === selectedID}
                  ondblclick={() => activate(row)}
                  onclick={() => selectNode(row.node.id)}
                  onkeydown={(event) => {
                    if (event.key === "Enter") activate(row);
                  }}
                >
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
      </Card>

      <aside class="detail" aria-label="Document authority">
        {#if selected}
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
                      disabled={loading || selectedTagsLoading || selectedTagsError !== ""}
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
                      <Chip
                        size="sm"
                        tone="workspace"
                        uppercase={false}
                        title={`${tag.assignment_count} total assignment${tag.assignment_count === 1 ? "" : "s"} · ${tag.id}`}
                      >
                        {tag.name}
                      </Chip>
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
    {#if historyOpen && selected && membership?.protected}
      <AuditHistoryDrawer
        session={webSession}
        node={selected.node}
        path={selected.path}
        onclose={() => (historyOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if versionsOpen && selected?.node.kind === "file"}
      <VersionHistoryDrawer
        session={webSession}
        node={selected.node}
        path={selected.path}
        onclose={() => (versionsOpen = false)}
        onauthfailure={handleFailure}
      />
    {/if}
    {#if provenanceOpen && selected?.node.kind === "file"}
      <ProvenanceDrawer
        session={webSession}
        node={selected.node}
        path={selected.path}
        onclose={() => (provenanceOpen = false)}
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
    {#if auditEvidenceOpen}
      <AuditEvidenceDrawer
        session={webSession}
        onclose={() => (auditEvidenceOpen = false)}
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
