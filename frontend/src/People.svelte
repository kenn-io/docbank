<script lang="ts">
  import { Button, Modal, TextInput, Typeahead, Spinner, Checkbox } from "@kenn-io/kit-ui";
  import UserIcon from "@lucide/svelte/icons/user";
  import { untrack } from "svelte";
  import { listPeople, preparePhotoPreview, type PersonSummary } from "./generated/docbank.js";
  import type { People } from "./people.svelte.js";
  import PeopleMergeConflict from "./PeopleMergeConflict.svelte";
  import { PhotoPreviewCache, previewObjectURL } from "./photoPreviewCache.js";
  import { photoFaceCropStyle } from "./photoRotation.js";
  let {
    people,
    cache,
    onperson,
  }: {
    people: People;
    cache: PhotoPreviewCache;
    onperson: (id: string, name: string, hidden?: boolean) => void;
  } = $props();
  let editing = $state.raw<PersonSummary>();
  let name = $state("");
  let candidates = $state.raw<PersonSummary[]>([]);
  let searchQuery = $state("");
  let nextCursor = $state<string>();
  let searchError = $state("");
  let searching = $state(false);
  let searchController: AbortController | undefined;
  let images = $state.raw<Record<string, { url: string; style: string }>>({});
  const merge = $derived(people.merge);
  const unlocked = $derived(!!people.access?.deadline);
  function close() {
    if (people.busy) return;
    editing = undefined;
    candidates = [];
    searchController?.abort();
    people.closeMerge();
  }
  $effect(() => {
    void people.access?.scope;
    untrack(() => {
      searchController?.abort();
      editing = undefined;
      candidates = [];
      people.closeMerge();
    });
    return () => searchController?.abort();
  });
  // Portraits load per face, so in-place card updates and Load more keep the
  // images already shown. An access-scope change starts over.
  type Portrait = { abort: AbortController; release?: () => void };
  const portraits = new Map<string, Portrait>();
  let hiddenCache: PhotoPreviewCache | undefined;
  const portraitKey = (person: PersonSummary) => {
    const face = person.photo_summary?.face;
    return face
      ? `${person.person_id}\0${JSON.stringify([face, !!person.photo_summary?.face_hidden])}`
      : "";
  };
  function releasePortrait(key: string) {
    const portrait = portraits.get(key);
    portrait?.abort.abort();
    portrait?.release?.();
    portraits.delete(key);
    const id = key.split("\0")[0]!;
    if (images[id]) {
      const next = { ...images };
      delete next[id];
      images = next;
    }
  }
  function loadPortrait(key: string, person: PersonSummary) {
    const face = person.photo_summary!.face!;
    const hidden = !!person.photo_summary?.face_hidden;
    const portrait: Portrait = { abort: new AbortController() };
    const { signal } = portrait.abort;
    portraits.set(key, portrait);
    const load = async () => {
      try {
        signal.throwIfAborted();
        let generation = face.generation_id;
        if (!generation) {
          const slot = await preparePhotoPreview(
            face.asset_id,
            {
              content_version_id: face.content_version_id,
              person_id: person.person_id,
              size: "grid",
              hidden,
            },
            { session: people.session, signal },
          );
          if (slot.state !== "ready" || !slot.generation_id) return;
          generation = slot.generation_id;
        }
        signal.throwIfAborted();
        hiddenCache ??= new PhotoPreviewCache(people.session, people.onauthfailure, true);
        portrait.release = previewObjectURL(
          hidden ? hiddenCache : cache,
          face.asset_id,
          generation,
          async (url) => {
            try {
              const image = new Image();
              image.src = url;
              await image.decode();
              if (!signal.aborted)
                images = {
                  ...images,
                  [person.person_id]: {
                    url,
                    style: photoFaceCropStyle(
                      face.box,
                      image.naturalWidth,
                      image.naturalHeight,
                      face.rotation,
                      128,
                    ),
                  },
                };
            } catch {
              /* Keep the person placeholder when bytes cannot be decoded. */
            }
          },
          (cause) => {
            if (!signal.aborted) people.failure(cause);
          },
          false,
          person.person_id,
        );
      } catch (cause) {
        if (!signal.aborted) people.failure(cause);
      }
    };
    void people.run(load);
  }
  function syncPortraits(rows: PersonSummary[]) {
    const wanted = new Map(rows.map((person) => [portraitKey(person), person]));
    for (const key of portraits.keys()) if (!wanted.has(key)) releasePortrait(key);
    for (const [key, person] of wanted) if (key && !portraits.has(key)) loadPortrait(key, person);
  }
  $effect(() => {
    const rows = people.items;
    untrack(() => syncPortraits(rows));
  });
  $effect(() => {
    void people.access?.scope;
    untrack(() => syncPortraits(people.items));
    return () => {
      for (const key of portraits.keys()) releasePortrait(key);
      images = {};
      void hiddenCache?.dispose();
      hiddenCache = undefined;
    };
  });
  async function search(value: string, more = false) {
    searchController?.abort();
    const request = (searchController = new AbortController());
    searchQuery = value;
    if (!more) {
      candidates = [];
      nextCursor = undefined;
    }
    searching = true;
    searchError = "";
    try {
      const page = await people.run(() => {
        request.signal.throwIfAborted();
        return listPeople(
          {
            query: value,
            limit: 100,
            recent_first: true,
            ...(more && nextCursor ? { cursor: nextCursor } : {}),
          },
          { session: people.session, signal: request.signal },
        );
      });
      if (!request.signal.aborted) {
        candidates = [
          ...candidates,
          ...page.items.filter((person) => person.person_id !== people.merge?.source.person_id),
        ];
        nextCursor = page.next_cursor;
      }
    } catch (cause) {
      if (!request.signal.aborted) searchError = people.failure(cause);
    } finally {
      if (!request.signal.aborted) searching = false;
    }
  }
</script>

<main class="people-workspace" aria-label="People">
  <div class="browser-toolbar people-toolbar">
    <div>
      <h1>People</h1>
      <span>{unlocked ? "Library and unlocked Hidden photos" : "Library photos"}</span>
    </div>
    <form
      onsubmit={(event) => {
        event.preventDefault();
        void people.load(people.draft.trim());
      }}
    >
      <TextInput ariaLabel="Find people" placeholder="Find people" bind:value={people.draft} />
      <Button type="submit" disabled={people.loading}>Search</Button>
    </form>
  </div>
  {#if people.error}<p role="alert">
      {people.error}<Button onclick={() => void people.load(people.query, !!people.items.length)}
        >Retry</Button
      >
    </p>{/if}
  {#if people.loading && !people.items.length}<p role="status"><Spinner />Loading people…</p>
  {:else if !people.items.length && !people.error}<p>
      {people.query ? "No matching people" : "No people yet"}
    </p>{/if}
  <ul class="people-grid">
    {#each people.items as person (person.person_id)}
      {@const image = images[person.person_id]}
      {@const summary = person.photo_summary}
      <li class="person-card">
        <button
          type="button"
          class="person-open"
          onclick={() => onperson(person.person_id, person.display_name)}
        >
          <span class="face-tile">
            <span class="face-crop"
              >{#if image}<img
                  src={image.url}
                  alt={`Face of ${person.display_name}`}
                  style={image.style}
                  onerror={() => {
                    const next = { ...images };
                    delete next[person.person_id];
                    images = next;
                  }}
                />{:else}<UserIcon size="48" aria-hidden="true" />{/if}</span
            >
          </span>
          <strong>{person.display_name}</strong>
          <span class="count"
            >{summary?.library_count ?? 0} Library {summary?.library_count === 1
              ? "photo"
              : "photos"}</span
          >
        </button>
        {#if unlocked && summary?.hidden_count !== undefined}<button
            class="hidden-count"
            type="button"
            onclick={() => onperson(person.person_id, person.display_name, true)}
            >{summary.hidden_count} Hidden {summary.hidden_count === 1 ? "photo" : "photos"}</button
          >{/if}
        <div class="card-actions">
          <Button
            size="sm"
            disabled={people.busy}
            onclick={() => {
              close();
              editing = person;
              name = person.display_name;
            }}>Rename</Button
          ><Button
            size="sm"
            disabled={people.busy}
            onclick={() => {
              close();
              people.startMerge(person);
              void search("");
            }}>Merge</Button
          >
        </div>
      </li>
    {/each}
  </ul>
  {#if people.cursor}<Button
      disabled={people.loading}
      onclick={() => void people.load(people.query, true)}
      >{people.loading ? "Loading…" : "Load more people"}</Button
    >{/if}
</main>
{#if editing}
  <Modal title={`Rename ${editing.display_name}`} onclose={close}>
    <form
      class="rename-form"
      onsubmit={(event) => {
        event.preventDefault();
        const current =
          people.items.find((row) => row.person_id === editing!.person_id) ?? editing!;
        void people.rename(current, name).then((saved) => {
          if (saved) close();
        });
      }}
    >
      <TextInput ariaLabel="Person name" bind:value={name} disabled={people.busy} />
      {#if people.writeError}<p role="alert">{people.writeError}</p>{/if}
      <div class="modal-actions">
        <Button disabled={people.busy} onclick={close}>Cancel</Button><Button
          type="submit"
          disabled={people.busy || !name.trim()}>Rename</Button
        >
      </div>
    </form>
  </Modal>
{/if}
{#if merge}
  {@const { source, survivor } = merge}
  <Modal title={`Merge ${source.display_name}`} onclose={close}>
    <Typeahead
      remote
      title="Keep person"
      placeholder="Find the person to keep"
      value={survivor?.person_id ?? ""}
      fallbackLabel={survivor?.display_name ?? "Find the person to keep"}
      options={candidates.map((person) => ({
        name: person.person_id,
        label: `${person.display_name} · ${person.identity_label || person.person_id}`,
      }))}
      disabled={people.busy}
      loading={searching}
      error={searchError}
      onquery={(value) => search(value)}
      onselect={(value) => {
        const chosen = candidates.find((person) => person.person_id === value);
        people.chooseSurvivor(chosen);
        return !!chosen;
      }}
    />
    {#if searchError}<Button
        disabled={searching}
        onclick={() => void search(searchQuery, !!candidates.length)}>Retry search</Button
      >{/if}
    {#if nextCursor}<Button disabled={searching} onclick={() => void search(searchQuery, true)}
        >Load more people</Button
      >{/if}
    {#if merge.notice === "stale" && survivor}<p role="status">
        {source.display_name} or {survivor.display_name} changed. Check the current names and confirm
        again.
      </p>
    {:else if merge.notice === "reselect"}<p role="status">
        The person to keep is no longer available. Choose another person.
      </p>{/if}
    {#if survivor}<p>
        Keep {survivor.display_name}. Move {source.display_name}'s records into this person.
      </p>
      <Checkbox
        label="Confirm merge"
        checked={merge.confirmed}
        onchange={(checked) => people.confirmMerge(checked)}
        disabled={people.busy}
      />{/if}
    {#if people.writeError}<p role="alert">{people.writeError}</p>{/if}
    {#if merge.conflicts.length}<ul class="conflicts" aria-label="Faces to review">
        {#each merge.conflicts as conflict (conflict.face_id)}<PeopleMergeConflict
            {people}
            {conflict}
          />{/each}
      </ul>{/if}
    {#if merge.notice === "uncertain"}<p>
        The result couldn't be confirmed. Retry to check whether the merge completed.
      </p>{/if}
    <div class="modal-actions">
      <Button disabled={people.busy} onclick={close}>Cancel</Button><Button
        disabled={people.busy || !survivor || !merge.confirmed}
        onclick={() =>
          void people.submitMerge().then((saved) => {
            if (saved) close();
          })}>{merge.notice === "uncertain" ? "Retry merge" : "Merge"}</Button
      >
    </div>
  </Modal>
{/if}

<style>
  .people-workspace {
    flex: 1;
    min-width: 0;
    overflow: auto;
    background: var(--bg-surface);
  }
  .people-toolbar {
    border-bottom: 1px solid var(--border-default);
  }
  h1 {
    margin: 0 0 4px;
    font-size: var(--font-size-lg);
  }
  .people-toolbar span,
  .count {
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }
  form,
  .card-actions,
  .modal-actions {
    display: flex;
    gap: var(--space-2);
  }
  .modal-actions {
    justify-content: flex-end;
    margin-top: var(--space-4);
  }
  .rename-form {
    flex-wrap: wrap;
  }
  .people-grid {
    list-style: none;
    padding: var(--space-5);
    margin: 0;
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
    gap: 16px;
  }
  .person-card {
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    overflow: hidden;
  }
  .person-open {
    width: 100%;
    font: inherit;
    text-align: left;
    background: none;
    border: 0;
    cursor: pointer;
    display: flex;
    flex-direction: column;
    color: var(--text-primary);
    text-decoration: none;
    gap: 8px;
    padding: 12px;
  }
  strong {
    overflow-wrap: anywhere;
  }
  .face-tile {
    aspect-ratio: 1;
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--bg-inset);
  }
  .face-crop {
    width: 128px;
    height: 128px;
    max-width: 100%;
    position: relative;
    border-radius: 50%;
    overflow: hidden;
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--text-muted);
  }
  img {
    position: absolute;
    top: 50%;
    left: 50%;
    max-width: none;
  }
  .card-actions {
    padding: 12px;
  }
  .conflicts {
    padding-left: var(--space-4);
  }
  .conflicts :global(li) {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .hidden-count {
    margin: 0 12px;
    color: var(--text-muted);
    background: none;
    border: 0;
    cursor: pointer;
  }
  .people-workspace > p {
    padding: var(--space-4);
  }
  @media (max-width: 640px) {
    .people-grid {
      padding: var(--space-3);
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .people-toolbar {
      flex-wrap: wrap;
    }
  }
</style>
