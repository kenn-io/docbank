<script lang="ts">
  import ArchiveIcon from "@lucide/svelte/icons/archive";
  import FileIcon from "@lucide/svelte/icons/file";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Card,
    Chip,
    DetailDrawer,
    EmptyState,
    IconButton,
    Spinner,
    TextInput,
  } from "@kenn-io/kit-ui";
  import { APIError, type Node } from "./api.js";
  import {
    collectionMembers,
    collectionLabel,
    collections,
    setCollectionLabel,
    type Collection,
    type CollectionLabel,
  } from "./collections.js";
  import { formatBytes, formatDate } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
    onopenmember: (node: Node, current: () => boolean) => void | Promise<void>;
  }

  let { session, onclose, onauthfailure, onopenmember }: Props = $props();

  let items = $state<Collection[]>([]);
  let total = $state(0);
  let selected = $state<Collection | null>(null);
  let members = $state<Node[]>([]);
  let memberTotal = $state(0);
  let loading = $state(true);
  let memberLoading = $state(false);
  let error = $state("");
  let memberError = $state("");
  let observedLabel = $state<CollectionLabel | null>(null);
  let labelDraft = $state("");
  let labelLoading = $state(false);
  let labelPending = $state(false);
  let labelError = $state("");
  let labelNotice = $state("");
  let openingMemberID = $state<number | null>(null);
  let openError = $state("");
  let refreshedAt = $state("");
  let loadedSession = "";
  let generation = 0;
  let memberGeneration = 0;
  let labelGeneration = 0;
  let openGeneration = 0;

  $effect(() => {
    const currentSession = session;
    void refresh(currentSession);
    return () => {
      generation += 1;
      memberGeneration += 1;
      labelGeneration += 1;
      openGeneration += 1;
    };
  });

  function message(cause: unknown): string {
    return cause instanceof Error ? cause.message : String(cause);
  }

  function authFailure(cause: unknown): boolean {
    if (!(cause instanceof APIError) || cause.status !== 401) return false;
    onauthfailure(cause);
    onclose();
    return true;
  }

  function close(): void {
    openGeneration += 1;
    onclose();
  }

  async function refresh(currentSession = session): Promise<void> {
    const request = ++generation;
    memberGeneration += 1;
    labelGeneration += 1;
    openGeneration += 1;
    loading = true;
    error = "";
    if (currentSession !== loadedSession) {
      items = [];
      total = 0;
      refreshedAt = "";
    }
    selected = null;
    members = [];
    memberTotal = 0;
    memberError = "";
    observedLabel = null;
    labelDraft = "";
    labelError = "";
    labelNotice = "";
    openingMemberID = null;
    openError = "";
    try {
      const page = await collections(currentSession);
      if (request !== generation || currentSession !== session) return;
      items = page.items;
      total = page.total;
      loadedSession = currentSession;
      refreshedAt = new Date().toISOString();
    } catch (cause) {
      if (request !== generation || currentSession !== session) return;
      if (!authFailure(cause)) error = message(cause);
    } finally {
      if (request === generation && currentSession === session) loading = false;
    }
  }

  async function browse(collection: Collection): Promise<void> {
    const request = ++memberGeneration;
    openGeneration += 1;
    const currentSession = session;
    selected = collection;
    members = [];
    memberTotal = 0;
    memberError = "";
    memberLoading = true;
    openingMemberID = null;
    openError = "";
    observedLabel = null;
    labelDraft = collection.label ?? "";
    labelError = "";
    labelNotice = "";
    void reloadLabel(collection);
    try {
      const page = await collectionMembers(currentSession, collection.id);
      if (
        request !== memberGeneration ||
        currentSession !== session ||
        selected?.id !== collection.id
      ) return;
      selected = page.collection;
      members = page.items;
      memberTotal = page.total;
    } catch (cause) {
      if (
        request !== memberGeneration ||
        currentSession !== session ||
        selected?.id !== collection.id
      ) return;
      if (!authFailure(cause)) memberError = message(cause);
    } finally {
      if (request === memberGeneration) memberLoading = false;
    }
  }

  function applyLabel(label: CollectionLabel): void {
    const update = (collection: Collection): Collection =>
      collection.id === label.ingest_id
        ? {
            ...collection,
            label: label.label,
            label_revision: label.revision,
            label_updated_at: label.updated_at,
          }
        : collection;
    items = items.map(update);
    if (selected?.id === label.ingest_id) selected = update(selected);
  }

  async function reloadLabel(collection = selected): Promise<void> {
    if (!collection) return;
    const request = ++labelGeneration;
    const currentSession = session;
    labelLoading = true;
    labelPending = false;
    labelError = "";
    labelNotice = "";
    try {
      const label = await collectionLabel(currentSession, collection.id);
      if (
        request !== labelGeneration ||
        currentSession !== session ||
        selected?.id !== collection.id
      ) return;
      observedLabel = label;
      labelDraft = label.label ?? "";
      applyLabel(label);
    } catch (cause) {
      if (
        request !== labelGeneration ||
        currentSession !== session ||
        selected?.id !== collection.id
      ) return;
      if (!authFailure(cause)) labelError = message(cause);
    } finally {
      if (request === labelGeneration) labelLoading = false;
    }
  }

  async function saveLabel(value: string | null): Promise<void> {
    if (!observedLabel || !selected) return;
    const collectionID = selected.id;
    const request = ++labelGeneration;
    const currentSession = session;
    const fence = observedLabel;
    labelPending = true;
    labelError = "";
    labelNotice = "";
    try {
      const label = await setCollectionLabel(currentSession, fence, value);
      if (
        request !== labelGeneration ||
        currentSession !== session ||
        selected?.id !== collectionID
      ) return;
      observedLabel = label;
      labelDraft = label.label ?? "";
      applyLabel(label);
      labelNotice = label.label === null ? "Collection label cleared." : "Collection label saved.";
    } catch (cause) {
      if (
        request !== labelGeneration ||
        currentSession !== session ||
        selected?.id !== collectionID
      ) return;
      if (!authFailure(cause)) labelError = message(cause);
    } finally {
      if (request === labelGeneration) labelPending = false;
    }
  }

  async function openMember(member: Node): Promise<void> {
    const request = ++openGeneration;
    const currentSession = session;
    const collectionID = selected?.id;
    const current = () =>
      request === openGeneration &&
      currentSession === session &&
      selected?.id === collectionID;
    openingMemberID = member.id;
    openError = "";
    try {
      await onopenmember(member, current);
    } catch (cause) {
      if (current()) openError = message(cause);
    } finally {
      if (current()) openingMemberID = null;
    }
  }

  function displayName(collection: Collection): string {
    return collection.label ?? `Unlabeled import ${collection.id.slice(0, 8)}`;
  }
</script>

<DetailDrawer width="min(760px, 100vw)" ariaLabel="Import collections" onclose={close}>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>IMPORT RUNS</span>
        <strong>Import collections</strong>
        <small>
          {total} collection{total === 1 ? "" : "s"}
          {#if refreshedAt} · Refreshed {formatDate(refreshedAt)}{/if}
        </small>
      </div>
      <div class="drawer-actions">
        <IconButton
          size="sm"
          ariaLabel="Refresh import collections"
          disabled={loading}
          onclick={() => void refresh()}
        >
          <RefreshCwIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton size="sm" ariaLabel="Close import collections" onclick={close}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    </div>
  {/snippet}

  <div class="collections-browser">
    {#if loading && items.length === 0}
      <div class="loading"><Spinner size={16} /> Loading import collections…</div>
    {:else if error && items.length === 0}
      <div class="load-error">
        <p role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if items.length === 0}
      <EmptyState
        title="No import collections"
        description="Collections appear after a filesystem import records at least one document."
      >
        {#snippet icon()}<ArchiveIcon size="22" />{/snippet}
      </EmptyState>
    {:else}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <section aria-labelledby="collections-heading">
        <div class="section-heading">
          <div>
            <span>COLLECTIONS</span>
            <strong id="collections-heading">Import history</strong>
          </div>
          <small>{items.length} shown</small>
        </div>
        <div class="collection-list">
          {#each items as collection (collection.id)}
            <Card
              level="default"
              padding="sm"
              eyebrow={`${collection.source_kind} import`}
              title={displayName(collection)}
              selected={selected?.id === collection.id}
              ariaLabel={`Browse collection ${displayName(collection)}`}
              onclick={() => void browse(collection)}
            >
              <div class="collection-facts">
                <p>{collection.source_description}</p>
                <div>
                  <Chip size="xs" tone="neutral">
                    {collection.file_count} document{collection.file_count === 1 ? "" : "s"}
                  </Chip>
                  <Chip size="xs" tone="neutral">{formatBytes(collection.total_bytes)}</Chip>
                  <span>Imported {formatDate(collection.started_at)}</span>
                </div>
              </div>
            </Card>
          {/each}
        </div>
        {#if total > items.length}
          <p class="bounded">Showing the first {items.length} of {total} collections.</p>
        {/if}
      </section>

      {#if selected}
        <section class="label-editor" aria-labelledby="collection-label-heading">
          <div class="section-heading">
            <div>
              <span>LABEL</span>
              <strong id="collection-label-heading">Collection label</strong>
            </div>
            {#if observedLabel}<small>Revision {observedLabel.revision}</small>{/if}
          </div>
          {#if labelLoading && !observedLabel}
            <div class="loading"><Spinner size={16} /> Reading label revision…</div>
          {:else}
            <div class="label-controls">
              <TextInput
                bind:value={labelDraft}
                block
                ariaLabel="Collection label"
                placeholder="Unlabeled import"
                disabled={labelLoading || labelPending || !observedLabel}
                invalid={Boolean(labelError)}
              />
              <Button
                size="sm"
                tone="info"
                surface="solid"
                disabled={labelLoading || labelPending || !observedLabel || labelDraft === ""}
                onclick={() => void saveLabel(labelDraft)}
              >{labelPending ? "Saving…" : "Save label"}</Button>
              <Button
                size="sm"
                surface="soft"
                disabled={labelLoading || labelPending || !observedLabel || observedLabel.label === null}
                onclick={() => void saveLabel(null)}
              >Clear label</Button>
              <Button
                size="sm"
                surface="soft"
                disabled={labelLoading || labelPending}
                onclick={() => void reloadLabel()}
              >Reload label</Button>
            </div>
          {/if}
          <p class="quality-note">Quality counters are unavailable for direct collection browsing.</p>
          {#if labelError}<p class="error" role="alert">{labelError}</p>{/if}
          {#if labelNotice}<p class="notice" role="status">{labelNotice}</p>{/if}
        </section>

        <section aria-labelledby="collection-members-heading">
          <div class="section-heading">
            <div>
              <span>DIRECT MEMBERSHIP</span>
              <strong id="collection-members-heading">Documents in this collection</strong>
            </div>
            <small>{displayName(selected)}</small>
          </div>
          {#if memberLoading}
            <div class="loading"><Spinner size={16} /> Loading direct members…</div>
          {:else if memberError}
            <div class="load-error">
              <p role="alert">{memberError}</p>
              <Button
                size="sm"
                onclick={() => {
                  if (selected) void browse(selected);
                }}
              >Try again</Button>
            </div>
          {:else if members.length === 0}
            <EmptyState
              title="No live documents"
              description="This retained collection currently has no live document members."
            >
              {#snippet icon()}<FileIcon size="22" />{/snippet}
            </EmptyState>
          {:else}
            <p class="member-summary">
              {#if memberTotal > members.length}
                Showing {members.length} of {memberTotal} direct members.
              {:else}
                {memberTotal} direct member{memberTotal === 1 ? "" : "s"}
              {/if}
              This list browses collection membership directly; it is not a search filter.
            </p>
            <div class="member-list">
              {#each members as member (member.id)}
                <Card level="inset" padding="sm" title={member.name} meta={formatBytes(member.size)}>
                  <div class="member-row">
                    <code>{member.path}</code>
                    <Button
                      size="sm"
                      tone="info"
                      surface="soft"
                      disabled={openingMemberID !== null}
                      onclick={() => void openMember(member)}
                      ariaLabel={`Open document ${member.path}`}
                    >{openingMemberID === member.id ? "Opening…" : "Open"}</Button>
                  </div>
                </Card>
              {/each}
            </div>
            {#if openError}<p class="error" role="alert">{openError}</p>{/if}
          {/if}
        </section>
      {/if}
    {/if}
  </div>
</DetailDrawer>

<style>
  .drawer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }

  .drawer-heading > div:first-child,
  .section-heading > div {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .drawer-heading span,
  .section-heading span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-bold);
    letter-spacing: var(--letter-spacing-label, 0.04em);
  }

  .drawer-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-lg);
  }

  .drawer-heading small,
  .section-heading small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .drawer-actions,
  .collection-facts > div,
  .member-row,
  .label-controls {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .collections-browser {
    display: grid;
    gap: var(--space-6);
    padding: var(--space-5);
  }

  section,
  .collection-list,
  .member-list {
    display: grid;
    gap: var(--space-3);
  }

  .section-heading {
    display: flex;
    align-items: end;
    justify-content: space-between;
    gap: var(--space-4);
  }

  .section-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }

  .collection-facts {
    display: grid;
    gap: var(--space-3);
    min-width: 0;
  }

  .collection-facts p,
  .member-summary,
  .bounded,
  .quality-note,
  .notice,
  .load-error p,
  .error {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .collection-facts p,
  .member-row code {
    overflow-wrap: anywhere;
  }

  .collection-facts span,
  .member-summary,
  .bounded,
  .quality-note,
  .notice {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .member-row {
    justify-content: space-between;
  }

  .label-controls {
    align-items: stretch;
    flex-wrap: wrap;
  }

  .label-controls :global(.kit-text-input) {
    flex: 1 1 240px;
  }

  .member-row code {
    min-width: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .loading {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .load-error {
    display: grid;
    justify-items: start;
    gap: var(--space-3);
  }

  .load-error p,
  .error {
    color: var(--accent-red);
  }
</style>
