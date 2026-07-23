<script lang="ts">
  import { onMount } from "svelte";
  import ArrowLeftIcon from "@lucide/svelte/icons/arrow-left";
  import FileIcon from "@lucide/svelte/icons/file";
  import FolderIcon from "@lucide/svelte/icons/folder";
  import LogOutIcon from "@lucide/svelte/icons/log-out";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import SearchIcon from "@lucide/svelte/icons/search";
  import HistoryIcon from "@lucide/svelte/icons/history";
  import {
    Button,
    Card,
    Chip,
    CopyButton,
    EmptyState,
    IconButton,
    SearchInput,
    Spinner,
    Table,
    TableHeaderCell,
    ThemeToggle,
    TopBar,
    type SortDirection,
  } from "@kenn-io/kit-ui";
  import AuditHistoryDrawer from "./AuditHistoryDrawer.svelte";
  import {
    APIError,
    auditStatusForNode,
    children,
    revokeSession,
    search,
    statPath,
    takeFragmentSession,
    type AuditStatus,
    type Node,
    type SearchHit,
  } from "./api.js";
  import { basename, formatBytes, formatDate } from "./format.js";
  import { orderRows, reconcileSearchView, type SortField } from "./rows.js";

  type Row = { node: Node; path: string; match?: "name" | "content" };
  type Snapshot = {
    directory: Node;
    rows: Row[];
    selectedID?: number;
    activeQuery: string;
    searchQuery: string;
    truncated: boolean;
    sortField: SortField;
    sortDirection: SortDirection;
  };

  let webSession = $state("");
  let directory = $state<Node | null>(null);
  let rows = $state<Row[]>([]);
  let stack = $state<Snapshot[]>([]);
  let selectedID = $state<number | undefined>();
  let searchQuery = $state("");
  let activeQuery = $state("");
  let loading = $state(false);
  let error = $state("");
  let truncated = $state(false);
  let sortField = $state<SortField>("name");
  let sortDirection = $state<SortDirection>("asc");
  let selectedAudit = $state<AuditStatus | null>(null);
  let auditLoading = $state(false);
  let auditError = $state("");
  let historyOpen = $state(false);
  let generation = 0;
  let auditGeneration = 0;

  const selected = $derived(rows.find((row) => row.node.id === selectedID));
  const membership = $derived(selectedAudit?.membership);
  const sortedRows = $derived(
    orderRows(rows, sortField, sortDirection, activeQuery !== ""),
  );

  onMount(() => {
    webSession = takeFragmentSession();
    if (webSession) void loadRoot();
  });

  function handleFailure(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      webSession = "";
      historyOpen = false;
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

  async function loadDirectory(nodeID: number, remember: boolean): Promise<void> {
    const request = ++generation;
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
            searchQuery,
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
      selectNode(rows[0]?.node.id);
      activeQuery = "";
      truncated = page.total > page.items.length;
      sortField = "name";
      sortDirection = "asc";
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

  async function runSearch(): Promise<void> {
    const query = searchQuery.trim();
    if (!query) {
      if (directory) await loadDirectory(directory.id, false);
      return;
    }
    const request = ++generation;
    loading = true;
    error = "";
    try {
      const report = await search(webSession, query);
      if (request !== generation) return;
      rows = report.hits.map((hit: SearchHit) => ({
        node: hit.node,
        path: hit.path,
        match: hit.match,
      }));
      const view = reconcileSearchView(
        rows,
        query,
        activeQuery,
        sortField,
        sortDirection,
        selectedID,
      );
      activeQuery = query;
      truncated = report.truncated;
      sortField = view.sortField;
      sortDirection = view.sortDirection;
      selectNode(view.selectedID);
    } catch (cause) {
      if (request === generation) handleFailure(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  function goBack(): void {
    generation += 1;
    const previous = stack.at(-1);
    if (!previous) return;
    directory = previous.directory;
    rows = previous.rows;
    selectNode(previous.selectedID);
    stack = stack.slice(0, -1);
    activeQuery = previous.activeQuery;
    searchQuery = previous.searchQuery;
    truncated = previous.truncated;
    sortField = previous.sortField;
    sortDirection = previous.sortDirection;
    error = "";
    loading = false;
  }

  function clearSearch(): void {
    searchQuery = "";
    if (activeQuery && directory) void loadDirectory(directory.id, false);
  }

  function activate(row: Row): void {
    selectNode(row.node.id);
    if (row.node.kind === "dir") {
      void loadDirectory(row.node.id, true);
    }
  }

  function selectNode(nodeID: number | undefined): void {
    if (selectedID !== nodeID) historyOpen = false;
    selectedID = nodeID;
    selectedAudit = null;
    auditError = "";
    auditGeneration += 1;
    if (nodeID !== undefined && webSession) void loadAuditStatus(nodeID);
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

  async function lock(): Promise<void> {
    generation += 1;
    auditGeneration += 1;
    const session = webSession;
    webSession = "";
    directory = null;
    rows = [];
    stack = [];
    selectedID = undefined;
    selectedAudit = null;
    auditLoading = false;
    auditError = "";
    historyOpen = false;
    activeQuery = "";
    searchQuery = "";
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
          Run <code>docbank web</code> to create a new read-only browser session.
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
              <span>{activeQuery ? "Search results" : "Current folder"}</span>
              <strong>{activeQuery ? `“${activeQuery}”` : directory?.path ?? "/"}</strong>
            </div>
          </div>
          <div class="toolbar-actions">
            <span>{rows.length}{truncated ? "+" : ""} item{rows.length === 1 ? "" : "s"}</span>
            <IconButton
              size="sm"
              ariaLabel="Refresh current view"
              onclick={() => {
                if (activeQuery) void runSearch();
                else if (directory) void loadDirectory(directory.id, false);
              }}
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
            title={activeQuery ? "No matching documents" : "This folder is empty"}
            description={activeQuery
              ? "Try another name or phrase from extracted text."
              : "Use the CLI, API, or an agent to file documents here."}
          >
            {#snippet icon()}
              {#if activeQuery}<SearchIcon size="22" />{:else}<FolderIcon size="22" />{/if}
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
                      <span>{activeQuery ? row.path : row.node.name}</span>
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
            eyebrow={selected.node.kind === "dir" ? "FOLDER" : "DOCUMENT AUTHORITY"}
            title={basename(selected.path)}
            meta={`id:${selected.node.id}`}
          >
            <dl>
              <div><dt>Path</dt><dd>{selected.path}</dd></div>
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
                  onclick={() => (historyOpen = true)}
                >
                  <HistoryIcon size="14" aria-hidden="true" />
                  Audit history
                </Button>
              {:else if selectedAudit?.enabled}
                <p>This node is outside every permanent audit scope.</p>
              {:else if !auditLoading}
                <p>Permanent audited history has not been enabled for this vault.</p>
              {/if}
            </div>
            {#if selected.node.kind === "dir"}
              <Button size="sm" onclick={() => activate(selected)}>
                <FolderIcon size="14" aria-hidden="true" />
                Open folder
              </Button>
            {/if}
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
  </div>
{/if}
