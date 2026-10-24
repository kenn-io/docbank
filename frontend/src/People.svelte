<script lang="ts">
  import { Button, Modal, TextInput, Spinner, Checkbox } from "@kenn-io/kit-ui";
  import UserIcon from "@lucide/svelte/icons/user";
  import { untrack } from "svelte";
  import type { PersonSummary } from "./generated/docbank.js";
  import { distinctLabel } from "./personLabel.js";
  import { mergeRoles, type People } from "./people.svelte.js";
  import { PhotoPreviewCache, faceCrop } from "./photoPreviewCache.js";
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
  let images = $state.raw<Record<string, { url: string; style: string }>>({});
  const unlocked = $derived(!!people.access?.deadline);
  const merge = $derived(people.merge);
  const roles = $derived(merge && mergeRoles(merge));
  const selected = (person: PersonSummary) =>
    people.selected.some((row) => row.person_id === person.person_id);
  function close() {
    if (people.busy) return;
    editing = undefined;
    people.closeMerge();
  }
  $effect(() => {
    void people.access?.scope;
    untrack(() => {
      editing = undefined;
      people.closeMerge();
    });
  });
  // Portraits load per face, so in-place card updates and Load more keep the
  // images already shown. An access-scope change starts over.
  type Portrait = { abort: AbortController; releases: (() => void)[] };
  const portraits = new Map<string, Portrait>();
  const portraitKey = (person: PersonSummary) => {
    const face = person.photo_summary?.face;
    return face ? `${person.person_id}\0${JSON.stringify(face)}` : "";
  };
  function releasePortrait(key: string) {
    const portrait = portraits.get(key);
    portrait?.abort.abort();
    for (const release of portrait?.releases ?? []) release();
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
    const portrait: Portrait = { abort: new AbortController(), releases: [] };
    const { signal } = portrait.abort;
    portraits.set(key, portrait);
    void (async () => {
      try {
        signal.throwIfAborted();
        const image = await faceCrop(
          cache,
          face,
          { id: person.person_id, hidden: person.photo_summary?.face_hidden ?? false, size: 128 },
          signal,
          (release) => portrait.releases.push(release),
        );
        if (image && !signal.aborted) images = { ...images, [person.person_id]: image };
      } catch (cause) {
        // A failed portrait keeps the person placeholder.
        if (!signal.aborted) people.failure(cause);
      }
    })();
  }
  function syncPortraits(rows: PersonSummary[]) {
    const owners = new Map(rows.map((person) => [person.person_id, person]));
    const wanted = new Map([...owners.values()].map((person) => [portraitKey(person), person]));
    for (const key of portraits.keys()) if (!wanted.has(key)) releasePortrait(key);
    for (const [key, person] of wanted) if (key && !portraits.has(key)) loadPortrait(key, person);
  }
  // Selected people keep their portraits after a search replaces the cards.
  $effect(() => {
    const rows = [...people.selected, ...people.items];
    untrack(() => syncPortraits(rows));
  });
  $effect(() => {
    void people.access?.scope;
    untrack(() => syncPortraits([...people.selected, ...people.items]));
    return () => {
      for (const key of portraits.keys()) releasePortrait(key);
      images = {};
    };
  });
</script>

{#snippet portrait(person: PersonSummary)}
  {@const image = images[person.person_id]}
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
{/snippet}
{#snippet counts(person: PersonSummary)}
  {@const summary = person.photo_summary}
  <span class="count"
    >{summary?.library_count ?? 0} Library {summary?.library_count === 1 ? "photo" : "photos"}</span
  >
{/snippet}

<main class="people-workspace" aria-label="People">
  <div class="browser-toolbar people-toolbar">
    <div>
      <h1>People</h1>
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
      {people.error}<Button onclick={() => void people.retry()}>Retry</Button>
    </p>{/if}
  {#if people.loading && !people.items.length}<p role="status"><Spinner />Loading people…</p>
  {:else if !people.items.length && !people.error}<p>
      {people.query ? "No matching people" : "No people yet"}
    </p>{/if}
  {#if people.writeError && !merge && !editing}<p role="alert">{people.writeError}</p>{/if}
  {#if people.selected.length}<div class="selection" role="region" aria-label="Selected people">
      <span
        >{people.selected.map((person) => distinctLabel(person, people.selected)).join(" and ")} selected</span
      >
      <Button
        disabled={people.busy || people.selected.length !== 2}
        onclick={() => people.startMerge()}>Merge</Button
      ><Button disabled={people.busy} onclick={() => people.clearSelection()}>Clear</Button>
    </div>{/if}
  <ul class="people-grid">
    {#each people.items as person (person.person_id)}
      <li class="person-card" class:selected={selected(person)}>
        <button
          type="button"
          class="person-open"
          onclick={() =>
            onperson(person.person_id, person.display_name, !person.photo_summary?.library_count)}
        >
          {@render portrait(person)}
          <strong
            >{people.items.some(
              (peer) =>
                peer.person_id !== person.person_id && peer.display_name === person.display_name,
            )
              ? distinctLabel(person, people.items)
              : person.display_name}</strong
          >
          {@render counts(person)}
        </button>
        {#if unlocked && person.photo_summary && (person.photo_summary.hidden_count ?? 0) > 0}<button
            class="hidden-count"
            type="button"
            onclick={() => onperson(person.person_id, person.display_name, true)}
            >{person.photo_summary.hidden_count} Hidden {person.photo_summary.hidden_count === 1
              ? "photo"
              : "photos"}</button
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
          ><Checkbox
            label="Select"
            ariaLabel={`Select ${distinctLabel(person, people.items)}`}
            checked={selected(person)}
            disabled={!!merge || (!selected(person) && people.selected.length >= 2)}
            onchange={() => people.toggle(person)}
          />
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
  {@const label = (person: PersonSummary) => distinctLabel(person, merge.pair)}
  <Modal title="Merge people" onclose={close}>
    <div class="merge-pair" role="radiogroup" aria-label="Person to keep">
      {#each merge.pair as person (person.person_id)}
        <label class="person-card merge-card">
          {@render portrait(person)}
          <strong>{label(person)}</strong>
          {@render counts(person)}
          <span class="keep"
            ><input
              type="radio"
              name="keep"
              aria-label={`Keep ${label(person)}`}
              checked={merge.keep === person.person_id}
              disabled={people.busy}
              onchange={() => people.keep(person.person_id)}
            />Keep this one</span
          >
        </label>
      {/each}
    </div>
    {#if roles}<p>
        Keep {label(roles.survivor)}. Move {label(roles.source)}'s records into this person.
      </p>
      <Checkbox
        label="Confirm merge"
        checked={merge.confirmed}
        onchange={(checked) => people.confirmMerge(checked)}
        disabled={people.busy}
      />{/if}
    {#if people.writeError}<p role="alert">{people.writeError}</p>{/if}
    <div class="modal-actions">
      <Button disabled={people.busy} onclick={close}>Cancel</Button><Button
        disabled={people.busy || !roles || !merge.confirmed}
        onclick={() =>
          void people.submitMerge().then((saved) => {
            if (saved) close();
          })}>Merge</Button
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
  .hidden-count {
    margin: 0 12px;
    color: var(--text-muted);
    background: none;
    border: 0;
    cursor: pointer;
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
  .person-card.selected {
    border-color: var(--accent, var(--border-strong, currentColor));
  }
  .selection {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-3) var(--space-5) 0;
  }
  .merge-pair {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(180px, 100%), 1fr));
    gap: 16px;
  }
  .merge-card {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 12px;
  }
  .keep {
    display: flex;
    align-items: center;
    gap: var(--space-2);
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
