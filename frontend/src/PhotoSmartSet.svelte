<script lang="ts">
  import PhotoSearchStatus from "./PhotoSearchStatus.svelte";
  import { onMount, onDestroy, tick, untrack } from "svelte";
  import { Button, Modal, TextInput } from "@kenn-io/kit-ui";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoAlbums } from "./photoAlbums.svelte.js";
  import PhotoAlbumPending from "./PhotoAlbumPending.svelte";
  import { canonicalQuery } from "./query.js";
  import { createSavedQuery, subscribeSavedQueries } from "./savedQueries.js";
  import { runSavedQuery } from "./generated/docbank.js";
  import { APIError } from "./api-transport.js";

  let {
    photos,
    albums,
    onnavigate,
  }: { photos: Photos; albums: PhotoAlbums; onnavigate: (path: string) => void } = $props();
  let dialog = $state<"smart" | "album">();
  let name = $state("");
  let busy = $state(false);
  let error = $state("");
  const frozen = $derived(photos.frozen);
  let controller = new AbortController();
  const query = $derived(photos.savedPhotoQuery);
  const identity = $derived(canonicalQuery(query));
  const saved = $derived(photos.matchingSmartSet);
  const savedIdentity = $derived(`${photos.smartSet?.id}:${photos.smartSet?.revision}`);
  let freezeRecovery:
    | { signal: AbortSignal; identity: string; hidden: boolean; id: string }
    | undefined;
  let previousScope = "";
  $effect(() => {
    // A changed definition must rerun this scope check even when the query matches.
    void savedIdentity;
    const scope = `${identity}:${photos.hidden}`;
    const keepDraft = previousScope === scope && untrack(() => dialog === "smart");
    previousScope = scope;
    const recovery = freezeRecovery;
    const keepFreeze =
      recovery?.signal === controller.signal &&
      recovery.identity === identity &&
      recovery.hidden === photos.hidden &&
      (!photos.smartSet || photos.smartSet.id === recovery.id);
    if (!keepFreeze && !keepDraft) {
      controller.abort();
      controller = new AbortController();
      error = "";
      busy = false;
      dialog = undefined;
    }
    untrack(() => photos.reconcileFrozen());
  });
  onMount(() => {
    if (!photos.smartSet) void photos.recoverSmartSet();
    return subscribeSavedQueries((session) => {
      if (session === photos.session) void photos.recoverSmartSet();
    });
  });
  onDestroy(() => controller.abort());

  async function save() {
    if (busy || !name.trim()) return;
    if (dialog === "album" && albums.rejectBusy()) {
      error = albums.error;
      return;
    }
    const signal = controller.signal;
    busy = true;
    error = "";
    try {
      if (dialog === "smart") {
        const result = await createSavedQuery(photos.session, {
          name: name.trim(),
          kind: "query",
          payload: query,
        });
        if (signal.aborted) return;
        photos.rememberSmartSet(result);
        photos.clearFrozen();
        dialog = undefined;
      } else if (frozen) {
        const result = await albums.create(name, {
          snapshot_id: frozen.snapshot.snapshot_id,
          member_hash: frozen.snapshot.member_hash,
        });
        if (signal.aborted) return;
        if (result) {
          photos.clearFrozen();
          dialog = undefined;
          onnavigate(`/photos/albums/${result.id}`);
        } else {
          error = albums.error;
          if (albums.errorCode === "snapshot_gone") photos.expireFrozen();
          if (albums.errorCode === "photo_snapshot_member_not_in_library") {
            error = "A frozen photo left Library. Freeze again to use current matches.";
            photos.clearFrozen();
          }
          if (albums.errorCode === "snapshot_member_hash") {
            error = "Frozen results could not be verified. Freeze again to save this album.";
            photos.clearFrozen();
          }
          if (albums.errorCode === "snapshot_too_large") {
            error = "Too many photos. Narrow the search and freeze again.";
            photos.clearFrozen();
          }
        }
      }
    } catch (cause) {
      if (!signal.aborted) {
        if (cause instanceof APIError && cause.status === 401) photos.onauthfailure(cause);
        error = cause instanceof Error ? cause.message : String(cause);
      }
    } finally {
      if (!signal.aborted) busy = false;
    }
  }

  async function freeze() {
    if (!saved || busy) return;
    const definition = saved;
    const signal = controller.signal;
    const freezeIdentity = identity;
    const freezeHidden = photos.hidden;
    busy = true;
    error = "";
    try {
      const result = await runSavedQuery(
        definition.id,
        { photos: true, replaces_snapshot_id: frozen?.snapshot.snapshot_id },
        { "If-Match": `"${definition.revision}"` },
        { session: photos.session, signal },
      );
      const current = photos.matchingSmartSet;
      if (
        !signal.aborted &&
        identity === freezeIdentity &&
        photos.hidden === freezeHidden &&
        current?.id === definition.id &&
        current.revision === definition.revision
      )
        photos.setFrozen(result);
    } catch (cause) {
      if (!signal.aborted && cause instanceof APIError && cause.status === 401) {
        photos.onauthfailure(cause);
        return;
      }
      if (
        !signal.aborted &&
        identity === freezeIdentity &&
        photos.hidden === freezeHidden &&
        photos.smartSet?.id === definition.id &&
        cause instanceof APIError &&
        (cause.code === "stale_revision" || cause.status === 404)
      ) {
        freezeRecovery = {
          signal,
          identity: freezeIdentity,
          hidden: freezeHidden,
          id: definition.id,
        };
        await photos.recoverSmartSet();
        await tick();
        if (freezeRecovery?.signal === signal) freezeRecovery = undefined;
      }
      if (
        !signal.aborted &&
        identity === freezeIdentity &&
        photos.hidden === freezeHidden &&
        (!photos.smartSet || photos.smartSet.id === definition.id)
      )
        error =
          cause instanceof APIError && cause.status === 404
            ? "This smart set was deleted. Save the search as a new smart set."
            : cause instanceof APIError && cause.code === "stale_revision"
              ? "This smart set changed. Reopen it from Albums to use the latest version."
              : cause instanceof APIError && cause.code === "photo_snapshot_member_not_in_library"
                ? "A photo left Library while freezing. Try freezing again."
                : cause instanceof APIError && cause.code === "snapshot_gone"
                  ? "Frozen results are unavailable. Try freezing again."
                  : cause instanceof APIError && cause.code === "snapshot_too_large"
                    ? "Too many photos. Narrow the search and freeze again."
                    : cause instanceof APIError &&
                        ["snapshot_capacity", "snapshot_busy"].includes(cause.code)
                      ? "Photo searches are busy. Try freezing again in a moment."
                      : cause instanceof Error
                        ? cause.message
                        : String(cause);
    } finally {
      if (freezeRecovery?.signal === signal) freezeRecovery = undefined;
      if (!signal.aborted) busy = false;
    }
  }
</script>

<div class:has-frozen={!!frozen} class="smart-actions" aria-label="Search actions">
  {#if frozen}
    <PhotoSearchStatus status={frozen.snapshot} total={frozen.run.total} />
    <div class="frozen-summary" role="status">
      <strong>Frozen results</strong>
      <span
        >{frozen.run.total.toLocaleString()}
        {frozen.run.total === 1 ? "photo" : "photos"} · Expires {new Date(
          frozen.run.expires_at,
        ).toLocaleTimeString()}</span
      >
    </div>
    <Button size="sm" disabled={busy || photos.loading} onclick={() => void freeze()}
      >{busy ? "Freezing…" : "Freeze again"}</Button
    >
    <Button
      size="sm"
      tone="info"
      disabled={busy || albums.busy}
      onclick={() => {
        name = saved?.name ?? "";
        dialog = "album";
      }}>Save as album</Button
    >
    <Button size="sm" disabled={busy || albums.busy} onclick={() => photos.clearFrozen()}
      >Discard</Button
    >
  {:else if photos.frozenExpired && !dialog}
    <span role="alert">Frozen results expired.</span>
    {#if saved}<Button size="sm" disabled={busy || photos.loading} onclick={() => void freeze()}
        >Freeze again</Button
      >{/if}
  {:else if saved}
    <span class="smart-set-summary">
      <strong class="smart-set-name">{saved.name}</strong>
      <span class="smart-set-kind">Smart set</span>
    </span>
    <Button size="sm" disabled={busy || photos.loading} onclick={() => void freeze()}
      >{busy ? "Freezing…" : "Freeze results"}</Button
    >
  {:else}
    {#if photos.smartSet}<span class="smart-set-name" title={`Changes from ${photos.smartSet.name}`}
        >Changes from {photos.smartSet.name}</span
      >{/if}
    <Button
      size="sm"
      tone="info"
      disabled={busy || photos.loading}
      onclick={() => {
        name = "";
        error = "";
        dialog = "smart";
      }}>{photos.smartSet ? "Save as new search" : "Save search"}</Button
    >
  {/if}
  {#if error && !dialog}<p class="smart-error" role="alert">{error}</p>{/if}
</div>
{#if dialog}
  <Modal
    title={dialog === "smart" ? "Save search" : "Save frozen photos as album"}
    onclose={() => {
      if (!busy) dialog = undefined;
    }}
  >
    <form
      onsubmit={(event) => {
        event.preventDefault();
        void save();
      }}
    >
      <TextInput ariaLabel={dialog === "smart" ? "Search name" : "Album name"} bind:value={name} />
      {#if error}<p role="alert">{error}</p>{/if}
      {#if dialog === "album" && photos.frozenExpired}<p role="alert">
          Frozen results expired. Freeze again to save this album.
        </p>{/if}
      {#if dialog === "album" && !frozen}<Button
          disabled={busy || !saved}
          onclick={() => void freeze()}>Freeze again</Button
        >{/if}
      {#if dialog === "album"}<PhotoAlbumPending
          {albums}
          onnavigate={(path) => {
            dialog = undefined;
            onnavigate(path);
          }}
        />{/if}
      <div class="modal-actions">
        <Button disabled={busy} onclick={() => (dialog = undefined)}>Cancel</Button><Button
          type="submit"
          disabled={busy ||
            !name.trim() ||
            (dialog === "album" && (!frozen || albums.busy || !!albums.unconfirmed))}>Save</Button
        >
      </div>
    </form>
  </Modal>
{/if}

<style>
  .smart-actions {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    flex-wrap: wrap;
    gap: var(--space-4);
    min-width: 0;
    font-size: var(--font-size-xs);
  }
  .smart-actions.has-frozen {
    flex: 1;
    justify-content: flex-start;
    gap: var(--space-3);
    padding: var(--space-2);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }
  .smart-set-summary {
    display: flex;
    align-items: center;
    gap: var(--space-6);
    min-width: 0;
  }
  .smart-set-name {
    max-width: 24rem;
    overflow: hidden;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 600;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .smart-set-kind {
    display: inline-flex;
    flex-shrink: 0;
    align-items: center;
    gap: var(--space-2);
    padding: 2px var(--space-3);
    border: 1px solid color-mix(in srgb, var(--accent-blue) 40%, var(--border-default));
    border-radius: 999px;
    color: color-mix(in srgb, var(--accent-blue) 72%, var(--text-primary));
    background: color-mix(in srgb, var(--accent-blue) 10%, var(--bg-surface));
  }
  .smart-set-kind::before {
    width: 6px;
    height: 6px;
    flex: 0 0 6px;
    border-radius: 50%;
    background: var(--accent-blue);
    content: "";
  }
  .frozen-summary {
    display: flex;
    flex-direction: column;
    min-width: 0;
    margin-right: auto;
  }
  .frozen-summary strong {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .frozen-summary span,
  .smart-error {
    color: var(--text-muted);
  }
  .smart-error {
    flex-basis: 100%;
    margin: 0;
  }
  .modal-actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    margin-top: var(--space-4);
  }
</style>
