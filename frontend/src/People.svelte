<script lang="ts">
  import { Button, Modal, TextInput, Typeahead, Spinner, Checkbox } from "@kenn-io/kit-ui";
  import UserIcon from "@lucide/svelte/icons/user";
  import { untrack } from "svelte";
  import { listPeople, preparePhotoPreview, type PersonSummary } from "./generated/docbank.js";
  import { People } from "./people.svelte.js";
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
  let query = $state("");
  let editing = $state.raw<PersonSummary>();
  let merging = $state.raw<PersonSummary>();
  let name = $state("");
  let survivor = $state.raw<PersonSummary>();
  let candidates = $state.raw<PersonSummary[]>([]);
  let searchQuery = $state("");
  let nextCursor = $state<string>();
  let searchError = $state("");
  let searching = $state(false);
  let searchController: AbortController | undefined;
  let images = $state.raw<Record<string, { url: string; style: string }>>({});
  let confirmed = $state(false);
  function close() {
    if (people.busy) return;
    editing = undefined;
    merging = undefined;
    survivor = undefined;
    candidates = [];
    searchController?.abort();
    people.writeError = "";
    confirmed = false;
  }
  $effect(() => {
    void people.access?.scope;
    untrack(() => {
      searchController?.abort();
      editing = undefined;
      merging = undefined;
      survivor = undefined;
      candidates = [];
    });
    return () => searchController?.abort();
  });
  $effect(() => {
    const rows = people.items;
    void people.access?.scope;
    const request = new AbortController();
    const releases: (() => void)[] = [];
    const hiddenCache = new PhotoPreviewCache(people.session, people.onauthfailure, true);
    images = {};
    for (const person of rows) {
      const face = person.photo_summary?.face;
      if (!face) continue;
      const hidden = !!person.photo_summary?.face_hidden;
      const load = async () => {
        try {
          request.signal.throwIfAborted();
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
              { session: people.session, signal: request.signal },
            );
            if (slot.state !== "ready" || !slot.generation_id) return;
            generation = slot.generation_id;
          }
          request.signal.throwIfAborted();
          releases.push(
            previewObjectURL(
              hidden ? hiddenCache : cache,
              face.asset_id,
              generation,
              async (url) => {
                try {
                  const image = new Image();
                  image.src = url;
                  await image.decode();
                  if (!request.signal.aborted)
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
                if (!request.signal.aborted) people.failure(cause);
              },
              false,
              person.person_id,
            ),
          );
        } catch (cause) {
          if (!request.signal.aborted) people.failure(cause);
        }
      };
      void (people.access ? people.access.run(load) : load());
    }
    return () => {
      request.abort();
      for (const release of releases) release();
      void hiddenCache.dispose();
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
      const read = () => {
        request.signal.throwIfAborted();
        return listPeople(
          { query: value, limit: 100, ...(more && nextCursor ? { cursor: nextCursor } : {}) },
          { session: people.session, signal: request.signal },
        );
      };
      const page = await (people.access ? people.access.run(read) : read());
      if (!request.signal.aborted) {
        candidates = [
          ...candidates,
          ...page.items.filter((person) => person.person_id !== merging?.person_id),
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
      <span>Library and unlocked Hidden photos</span>
    </div>
    <form
      onsubmit={(event) => {
        event.preventDefault();
        void people.load(query);
      }}
    >
      <TextInput ariaLabel="Find people" placeholder="Find people" bind:value={query} />
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
        <a
          href={`/photos#person=${person.person_id}`}
          onclick={(event) => {
            event.preventDefault();
            onperson(person.person_id, person.display_name);
          }}
        >
          <div class="face-tile">
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
          </div>
          <strong>{person.display_name}</strong>
          <span class="count"
            >{summary?.library_count ?? 0} Library {summary?.library_count === 1
              ? "photo"
              : "photos"}</span
          >
        </a>
        {#if summary?.hidden_count !== undefined}<button
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
              merging = person;
              nextCursor = undefined;
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
{#if merging}
  <Modal title={`Merge ${merging.display_name}`} onclose={close}>
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
        survivor = candidates.find((person) => person.person_id === value);
        confirmed = false;
        return !!survivor;
      }}
    />
    {#if searchError}<Button
        disabled={searching}
        onclick={() => void search(searchQuery, !!candidates.length)}>Retry search</Button
      >{/if}
    {#if nextCursor}<Button disabled={searching} onclick={() => void search(searchQuery, true)}
        >Load more people</Button
      >{/if}
    {#if survivor}<p>
        Keep {survivor.display_name}. Move {merging.display_name}'s records into this person.
      </p>
      <Checkbox label="Confirm merge" bind:checked={confirmed} disabled={people.busy} />{/if}
    {#if people.writeError}<p role="alert">{people.writeError}</p>{/if}
    {#if people.uncertain}<p>
        The result couldn't be confirmed. Retry to check whether the merge completed.
      </p>{/if}
    <div class="modal-actions">
      <Button disabled={people.busy} onclick={close}>Cancel</Button><Button
        disabled={people.busy || !survivor || !confirmed}
        onclick={() =>
          void people.merge(merging!, survivor!).then((saved) => {
            if (saved) close();
          })}>{people.uncertain ? "Retry merge" : "Merge"}</Button
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
  a {
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
