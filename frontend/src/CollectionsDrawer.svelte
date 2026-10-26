<script lang="ts">
  import DrawerShell from "./DrawerShell.svelte";
  import ArrowLeftIcon from "@lucide/svelte/icons/arrow-left";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import SearchIcon from "@lucide/svelte/icons/search";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import { Button, IconButton, Spinner, TextInput } from "@kenn-io/kit-ui";
  import "./toolForm.css";
  import InspectorDetails from "./InspectorDetails.svelte";
  import { APIError } from "./api-transport.js";
  import { type Node } from "./generated/docbank.js";
  import {
    collectionMembers,
    collectionLabel,
    collections,
    displayName,
    setCollectionLabel,
    type Collection,
    type CollectionLabel,
  } from "./collections.js";
  import { formatBytes, formatDate } from "./format.js";
  import QualityPanel from "./QualityPanel.svelte";
  import type { Query } from "./query.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
    onopenmember: (node: Node, current: () => boolean) => void | Promise<void>;
    onnewquery?: (id: string) => void;
    onqualityquery?: (query: Query) => void;
  }

  let { session, onclose, onauthfailure, onopenmember, onnewquery, onqualityquery }: Props =
    $props();
  let qualityOpen = $state(false);
  let profile = $state("");

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
  let openingMemberID = $state<number | null>(null);
  let openError = $state("");
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
      profile = "";
      items = [];
      total = 0;
    }
    selected = null;
    members = [];
    memberTotal = 0;
    memberError = "";
    observedLabel = null;
    labelDraft = "";
    labelError = "";
    openingMemberID = null;
    openError = "";
    try {
      const page = await collections(currentSession);
      if (request !== generation || currentSession !== session) return;
      items = page.items;
      total = page.total;
      loadedSession = currentSession;
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
    void reloadLabel(collection);
    try {
      const page = await collectionMembers(currentSession, collection.id);
      if (
        request !== memberGeneration ||
        currentSession !== session ||
        selected?.id !== collection.id
      )
        return;
      selected = page.collection;
      members = page.items;
      memberTotal = page.total;
    } catch (cause) {
      if (
        request !== memberGeneration ||
        currentSession !== session ||
        selected?.id !== collection.id
      )
        return;
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
    try {
      const label = await collectionLabel(currentSession, collection.id);
      if (
        request !== labelGeneration ||
        currentSession !== session ||
        selected?.id !== collection.id
      )
        return;
      observedLabel = label;
      labelDraft = label.label ?? "";
      applyLabel(label);
    } catch (cause) {
      if (
        request !== labelGeneration ||
        currentSession !== session ||
        selected?.id !== collection.id
      )
        return;
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
    try {
      const label = await setCollectionLabel(currentSession, fence, value);
      if (
        request !== labelGeneration ||
        currentSession !== session ||
        selected?.id !== collectionID
      )
        return;
      observedLabel = label;
      labelDraft = label.label ?? "";
      applyLabel(label);
    } catch (cause) {
      if (
        request !== labelGeneration ||
        currentSession !== session ||
        selected?.id !== collectionID
      )
        return;
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
      request === openGeneration && currentSession === session && selected?.id === collectionID;
    openingMemberID = member.id;
    openError = "";
    try {
      await onopenmember(member, current);
    } catch (cause) {
      if (current() && !authFailure(cause)) openError = message(cause);
    } finally {
      if (current()) openingMemberID = null;
    }
  }
</script>

<DrawerShell
  title="Import history"
  ariaLabel="Import history"
  closeLabel="Close import history"
  onclose={close}
>
  {#snippet actions()}
    <IconButton
      size="sm"
      ariaLabel="Refresh import history"
      title="Refresh"
      disabled={loading}
      onclick={() => void refresh()}
    >
      <RefreshCwIcon size="14" aria-hidden="true" />
    </IconButton>
  {/snippet}

  <div class="tool-body">
    {#if loading && items.length === 0}
      <p class="tool-muted" role="status"><Spinner size={14} /> Loading…</p>
    {:else if error && items.length === 0}
      <div class="tool-heading">
        <p class="tool-error" role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if items.length === 0}
      <p class="tool-muted">
        No imports yet. Run <code>docbank add &lt;directory&gt; --dest /documents</code>.
      </p>
    {:else if selected}
      {@const current = selected}
      <section class="tool-section" aria-labelledby="import-heading">
        <div class="tool-heading">
          <div class="title">
            <IconButton
              size="sm"
              ariaLabel="All imports"
              title="All imports"
              onclick={() => {
                memberGeneration += 1;
                labelGeneration += 1;
                openGeneration += 1;
                selected = null;
                qualityOpen = false;
              }}><ArrowLeftIcon size="14" aria-hidden="true" /></IconButton
            >
            <h3 id="import-heading">{displayName(current)}</h3>
          </div>
          <div class="tool-row-actions">
            <Button
              size="sm"
              ariaExpanded={qualityOpen}
              onclick={() => (qualityOpen = !qualityOpen)}>Quality</Button
            >
            {#if onnewquery}<Button size="sm" onclick={() => onnewquery?.(current.id)}
                ><SearchIcon size="14" aria-hidden="true" /> Search this import</Button
              >{/if}
          </div>
        </div>
        <InspectorDetails
          label="Import details"
          facts={[
            {
              label: "Documents",
              value: `${current.file_count} document${current.file_count === 1 ? "" : "s"}`,
            },
            { label: "Size", value: formatBytes(current.total_bytes) },
            { label: "Imported", value: formatDate(current.started_at) },
          ]}
        />
        <p class="tool-muted">{current.source_description}</p>
        <div class="tool-field">
          <label for="import-label">Label</label>
          {#if labelLoading && !observedLabel}
            <p class="tool-muted"><Spinner size={14} /> Loading…</p>
          {:else}
            <form
              class="label-controls"
              onsubmit={(event) => {
                event.preventDefault();
                if (labelDraft !== "") void saveLabel(labelDraft);
              }}
            >
              <TextInput
                id="import-label"
                bind:value={labelDraft}
                block
                ariaLabel="Import label"
                placeholder="Unlabeled"
                disabled={labelLoading || labelPending || !observedLabel}
                invalid={Boolean(labelError)}
              />
              {#if observedLabel?.label !== null && observedLabel?.label !== undefined}<Button
                  disabled={labelLoading || labelPending}
                  onclick={() => void saveLabel(null)}>Clear</Button
                >{/if}
              <Button
                type="submit"
                tone="info"
                surface="solid"
                disabled={labelLoading ||
                  labelPending ||
                  !observedLabel ||
                  labelDraft === "" ||
                  labelDraft === (observedLabel?.label ?? "")}
                >{labelPending ? "Saving…" : "Save label"}</Button
              >
            </form>
          {/if}
          {#if labelError}<div class="tool-heading">
              <p class="tool-error" role="alert">{labelError}</p>
              <Button size="sm" disabled={labelLoading} onclick={() => void reloadLabel()}
                >Reload label</Button
              >
            </div>{/if}
        </div>
        {#if qualityOpen}<QualityPanel
            {session}
            collectionID={current.id}
            {profile}
            onprofile={(name) => (profile = name)}
            onnewquery={onqualityquery}
            {onauthfailure}
          />{/if}
      </section>

      <section class="tool-section" aria-labelledby="import-members-heading">
        <div class="tool-heading">
          <h3 id="import-members-heading">Documents</h3>
          {#if memberTotal > members.length}<span class="tool-muted"
              >{members.length} of {memberTotal}</span
            >{/if}
        </div>
        {#if memberLoading}
          <p class="tool-muted"><Spinner size={14} /> Loading…</p>
        {:else if memberError}
          <div class="tool-heading">
            <p class="tool-error" role="alert">{memberError}</p>
            <Button size="sm" onclick={() => void browse(current)}>Try again</Button>
          </div>
        {:else if members.length === 0}
          <p class="tool-muted">No documents.</p>
        {:else}
          <ul class="tool-list">
            {#each members as member (member.id)}
              <li>
                <div class="tool-row-main">
                  <strong title={member.name}>{member.name}</strong>
                  <span class="tool-truncate-start" title={member.path}
                    ><bdi>{member.path} · {formatBytes(member.size)}</bdi></span
                  >
                </div>
                <Button
                  size="sm"
                  disabled={openingMemberID !== null}
                  onclick={() => void openMember(member)}
                  ariaLabel={`Open document ${member.path}`}
                  >{openingMemberID === member.id ? "Opening…" : "Open"}</Button
                >
              </li>
            {/each}
          </ul>
          {#if openError}<p class="tool-error" role="alert">{openError}</p>{/if}
        {/if}
      </section>
    {:else}
      {#if error}<p class="tool-error" role="alert">{error}</p>{/if}
      <section class="tool-section" aria-label="Import history">
        <ul class="tool-list">
          {#each items as collection (collection.id)}
            <li class="import-row">
              <button
                class="import-summary"
                type="button"
                aria-label={`Browse import ${displayName(collection)}`}
                title={collection.source_description}
                onclick={() => void browse(collection)}
              >
                <span class="tool-row-main">
                  <strong>{displayName(collection)}</strong>
                  <span
                    >{collection.file_count} document{collection.file_count === 1 ? "" : "s"} · {formatBytes(
                      collection.total_bytes,
                    )} · {formatDate(collection.started_at)}</span
                  >
                </span>
                <ChevronRightIcon size="16" aria-hidden="true" />
              </button>
            </li>
          {/each}
        </ul>
        {#if total > items.length}
          <p class="tool-muted">{items.length} of {total} imports</p>
        {/if}
      </section>
    {/if}
  </div>
</DrawerShell>

<style>
  .tool-list > li.import-row {
    padding: 0;
  }
  .import-summary {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-5);
    width: 100%;
    min-width: 0;
    padding: var(--space-4) var(--space-3);
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .import-summary:hover {
    background: var(--bg-inset);
  }
  .import-summary:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
  }
  .import-summary > :global(svg) {
    flex-shrink: 0;
    color: var(--text-muted);
  }
  .title {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
  }
  .title h3 {
    margin: 0;
    overflow: hidden;
    font-size: var(--font-size-md);
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .label-controls {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    max-width: 480px;
  }
  .label-controls :global(.kit-text-input) {
    flex: 1;
  }
</style>
