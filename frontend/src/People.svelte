<script lang="ts">
  import {
    Button,
    Checkbox,
    Modal,
    PageHeader,
    RadioGroup,
    Spinner,
    TextInput,
    Typeahead,
  } from "@kenn-io/kit-ui";
  import XIcon from "@lucide/svelte/icons/x";
  import UserIcon from "@lucide/svelte/icons/user";
  import { untrack } from "svelte";
  import type {
    FaceSuggestionFace,
    PersonFacePreview,
    PersonSummary,
  } from "./generated/docbank.js";
  import { distinctLabel } from "./personLabel.js";
  import { APIError } from "./api-transport.js";
  import PersonNamesakes, {
    groupKey,
    namesakeGroup,
    namesakeOptions,
    personKey,
  } from "./PersonNamesakes.svelte";
  import {
    mergeRoles,
    reviewFaces,
    suggestionCounts,
    suggestionTitle,
    type People,
  } from "./people.svelte.js";
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
  // pending holds the review sheet's crops still loading, which show a spinner.
  let pending = $state.raw<ReadonlySet<string>>(new Set());
  // pick is the "Who is this?" name: an existing person, or a new one by name.
  // A person chosen among namesakes keeps their shared name and a distinct label.
  let pick = $state.raw<{ id?: string; name: string; label?: string; group?: string }>();
  // namesakes is the open chooser for a name several people share.
  let namesakes = $state.raw<ReturnType<typeof namesakeGroup>>();
  let namesake = $state("");
  const unlocked = $derived(!!people.access?.deadline);
  const merge = $derived(people.merge);
  const roles = $derived(merge && mergeRoles(merge));
  const suggestions = $derived(people.suggestions);
  const review = $derived(suggestions.review);
  const reviewing = $derived(!!review);
  const faces = $derived(review ? reviewFaces(review) : []);
  const names = $derived(suggestions.names);
  const nameOptions = $derived(namesakeOptions(names.people, names.incompleteName));
  const sharedPick = $derived(
    pick?.group && nameOptions.some((option) => option.name === groupKey(pick!.group!))
      ? pick.group
      : "",
  );
  const selected = (person: PersonSummary) =>
    people.selected.some((row) => row.person_id === person.person_id);
  function close() {
    if (people.busy) return;
    editing = undefined;
    people.closeMerge();
  }
  function closeReview() {
    if (suggestions.busy) return;
    pick = undefined;
    namesakes = undefined;
    suggestions.close();
  }
  $effect(() => {
    void people.access?.scope;
    untrack(() => {
      editing = undefined;
      people.closeMerge();
      pick = undefined;
      namesakes = undefined;
      suggestions.close();
    });
  });
  // Keep the chosen name while a background rebuild replaces the open group.
  $effect(() => {
    void reviewing;
    untrack(() => {
      pick = undefined;
      namesakes = undefined;
    });
  });
  // Crops load per face, so in-place card updates and Load more keep the
  // images already shown. An access-scope change starts over.
  type Crop = {
    face: PersonFacePreview;
    id?: string;
    faceID?: string;
    hidden: boolean;
    size: number;
    review?: boolean;
  };
  type Loading = { abort: AbortController; releases: (() => void)[] };
  const loading = new Map<string, Loading>();
  const portraitKey = (person: PersonSummary) => {
    const face = person.photo_summary?.face;
    return face ? `${person.person_id}\0${JSON.stringify(face)}` : "";
  };
  const faceKey = (face: FaceSuggestionFace, size: number) =>
    face.preview
      ? `face\0${size}\0${face.face_id}\0${face.person_id ?? ""}\0${JSON.stringify(face.preview)}`
      : "";
  function settle(key: string) {
    if (!pending.has(key)) return;
    const next = new Set(pending);
    next.delete(key);
    pending = next;
  }
  function releaseCrop(key: string) {
    const crop = loading.get(key);
    crop?.abort.abort();
    for (const release of crop?.releases ?? []) release();
    loading.delete(key);
    settle(key);
    if (images[key]) {
      const next = { ...images };
      delete next[key];
      images = next;
    }
  }
  function loadCrop(key: string, crop: Crop) {
    const state: Loading = { abort: new AbortController(), releases: [] };
    const { signal } = state.abort;
    loading.set(key, state);
    if (crop.review) pending = new Set(pending).add(key);
    void (async () => {
      try {
        signal.throwIfAborted();
        const image = await faceCrop(
          cache,
          crop.face,
          {
            ...(crop.id ? { id: crop.id } : { faceID: crop.faceID }),
            hidden: crop.hidden,
            size: crop.size,
          },
          signal,
          (release) => state.releases.push(release),
        );
        if (image && !signal.aborted) images = { ...images, [key]: image };
      } catch (cause) {
        // A failed crop keeps the placeholder.
        if (!signal.aborted) people.failure(cause);
      } finally {
        if (!signal.aborted) settle(key);
      }
    })();
  }
  function wantedCrops() {
    const wanted = new Map<string, Crop>();
    for (const person of [...people.selected, ...people.items]) {
      const face = person.photo_summary?.face;
      if (face)
        wanted.set(portraitKey(person), {
          face,
          id: person.person_id,
          hidden: person.photo_summary?.face_hidden ?? false,
          size: 128,
        });
    }
    const suggested = (face: FaceSuggestionFace | undefined, size: number, sheet = false) => {
      if (face?.preview)
        wanted.set(faceKey(face, size), {
          face: face.preview,
          faceID: face.face_id,
          hidden: face.hidden,
          size,
          review: sheet,
        });
    };
    for (const candidate of suggestions.items)
      suggested(
        (review?.candidate?.candidate_id === candidate.candidate_id ? faces : candidate.faces)[0],
        128,
      );
    for (const face of faces) suggested(face, 120, true);
    return wanted;
  }
  function syncCrops(wanted: Map<string, Crop>) {
    for (const key of loading.keys()) if (!wanted.has(key)) releaseCrop(key);
    for (const [key, crop] of wanted) if (!loading.has(key)) loadCrop(key, crop);
  }
  // Selected people keep their portraits after a search replaces the cards.
  $effect(() => {
    const wanted = wantedCrops();
    untrack(() => syncCrops(wanted));
  });
  $effect(() => {
    void people.access?.scope;
    untrack(() => syncCrops(wantedCrops()));
    return () => {
      for (const key of loading.keys()) releaseCrop(key);
      images = {};
    };
  });
  function openNamesakes(name: string) {
    namesakes = namesakeGroup(names.people, name, names.query);
    namesake = namesakes.people.find((row) => row.person_id === pick?.id)?.person_id ?? "";
  }
  function chooseNamesake() {
    if (!namesakes || !namesake) return;
    const row = namesakes.people.find((row) => row.person_id === namesake);
    pick = row
      ? {
          id: row.person_id,
          name: row.display_name,
          label: distinctLabel(row, namesakes.people),
          group: row.display_name_folded,
        }
      : { name: namesakes.newName };
    namesakes = undefined;
  }
  function namesakeFailure(cause: unknown) {
    people.failure(cause);
    if (!(cause instanceof APIError && cause.status === 401)) return false;
    namesakes = undefined;
    return true;
  }
  function confirmName() {
    if (!pick) return;
    const decision = pick.id
      ? { action: "link" as const, person_id: pick.id }
      : { action: "new_person" as const, display_name: pick.name };
    void suggestions.decide(decision);
  }
</script>

{#snippet crop(key: string, label: string, icon: number)}
  {@const image = images[key]}
  {#if image}<img
      src={image.url}
      alt={label}
      style={image.style}
      onerror={() => {
        const next = { ...images };
        delete next[key];
        images = next;
      }}
    />{:else if pending.has(key)}<Spinner label="Loading face" />{:else}<UserIcon
      size={icon}
      aria-hidden="true"
    />{/if}
{/snippet}
{#snippet portrait(person: PersonSummary)}
  <span class="face-tile">
    <span class="face-crop"
      >{@render crop(portraitKey(person), `Face of ${person.display_name}`, 48)}</span
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
  <div class="people-header">
    <PageHeader title="People" size="medium">
      {#snippet actions()}
        <form
          onsubmit={(event) => {
            event.preventDefault();
            void people.load(people.draft.trim());
          }}
        >
          <TextInput ariaLabel="Find people" placeholder="Find people" bind:value={people.draft} />
          <Button type="submit" disabled={people.loading}>Search</Button>
        </form>
      {/snippet}
    </PageHeader>
  </div>
  {#if suggestions.loading || suggestions.error || suggestions.items.length}<section
      class="suggestions"
      aria-labelledby="suggestions-title"
    >
      <h2 id="suggestions-title">
        Suggestions{#if suggestions.total}<span class="count">{suggestions.total}</span>{/if}
      </h2>
      {#if suggestions.error}<p role="alert">
          {suggestions.error}<Button size="sm" onclick={() => void suggestions.retry()}
            >Retry</Button
          >
        </p>{/if}
      {#if suggestions.items.length}<ul class="suggestion-row">
          {#each suggestions.items as candidate (candidate.candidate_id)}
            {@const cardFaces =
              review?.candidate?.candidate_id === candidate.candidate_id ? faces : candidate.faces}
            {@const first = cardFaces[0]}
            <li>
              <button
                type="button"
                class="suggestion-card"
                disabled={suggestions.busy}
                onclick={() => suggestions.open(candidate)}
              >
                <span class="face-tile"
                  ><span class="suggestion-crop"
                    >{#if first}{@render crop(faceKey(first, 128), "Suggested face", 36)}{/if}</span
                  ></span
                >
                <strong class="suggestion-title">{suggestionTitle(candidate)}</strong>
                <span class="count">{suggestionCounts(cardFaces)}</span>
              </button>
            </li>
          {/each}
          {#if suggestions.items.length < suggestions.total}<li class="suggestion-more">
              <Button disabled={suggestions.loading} onclick={() => void suggestions.load(true)}
                >{suggestions.loading ? "Loading…" : "Show more"}</Button
              >
            </li>{/if}
        </ul>{/if}
      {#if suggestions.loading && !suggestions.items.length}<p role="status">
          <Spinner />Loading suggestions…
        </p>{/if}
    </section>{/if}
  {#if suggestions.writeError && !review}<p role="alert">{suggestions.writeError}</p>{/if}
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
  {#if suggestions.items.length && people.items.length}<h2 class="saved-title">
      Named people
    </h2>{/if}
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
        void people.rename(editing!, name).then((saved) => {
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
    <div class="merge-pair">
      {#each merge.pair as person (person.person_id)}
        <div class="person-card merge-card">
          {@render portrait(person)}
          <strong>{label(person)}</strong>
          {@render counts(person)}
        </div>
      {/each}
    </div>
    <RadioGroup
      legend="Person to keep"
      name="keep"
      value={merge.keep}
      disabled={people.busy}
      options={merge.pair.map((person) => ({
        value: person.person_id,
        label: `Keep ${label(person)}`,
      }))}
      onchange={(value) => {
        if (value) people.keep(value);
      }}
    />
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

{#if review && namesakes}
  <PersonNamesakes
    name={namesakes.name}
    people={namesakes.people}
    newName={namesakes.newName}
    confirmLabel="Choose"
    bind:selection={namesake}
    hidden={faces[0]?.hidden ?? false}
    session={people.session}
    {cache}
    run={(load) => people.run(load)}
    onfailure={namesakeFailure}
    onclose={() => (namesakes = undefined)}
    onconfirm={chooseNamesake}
  />
{:else if review}
  {@const named = review.candidate?.suggested_person_id}
  {@const person = review.candidate?.display_name ?? ""}
  <Modal title={review.title} onclose={closeReview} width="406px">
    <div class="review-content">
      {#if review.candidate}
        <p class="count">{suggestionCounts(faces)}</p>
        <ul class="review-grid">
          {#each faces as face, index (face.face_id)}
            <li class="review-face">
              {@render crop(faceKey(face, 120), "Suggested face", 44)}
              <button
                type="button"
                class="remove-face"
                aria-label={named ? `Not ${person}: face ${index + 1}` : `Skip face ${index + 1}`}
                title={named ? `Not ${person}` : "Skip"}
                disabled={suggestions.busy}
                onclick={() => void suggestions.removeFace(face)}
                ><XIcon size="14" aria-hidden="true" /></button
              >
            </li>
          {/each}
        </ul>
        {#if suggestions.writeError}<p role="alert">{suggestions.writeError}</p>{/if}
        {#if named}<div class="modal-actions">
            <Button
              disabled={suggestions.busy}
              onclick={() => void suggestions.decide({ action: "reject" })}>Not {person}</Button
            ><Button
              tone="info"
              surface="solid"
              disabled={suggestions.busy}
              onclick={() => void suggestions.decide({ action: "link", person_id: named })}
              >Confirm</Button
            >
          </div>
        {:else}<span class="name-picker"
            ><Typeahead
              remote
              placement="bottom"
              title="Name this person"
              placeholder="Name this person"
              fallbackLabel="Name this person"
              value={pick
                ? sharedPick
                  ? groupKey(sharedPick)
                  : pick.id
                    ? personKey(pick.id)
                    : pick.name
                : ""}
              options={[
                ...(pick &&
                !sharedPick &&
                !nameOptions.some((option) => pick?.id && option.name === personKey(pick.id))
                  ? [
                      pick.id
                        ? { name: personKey(pick.id), label: pick.label ?? pick.name }
                        : { name: pick.name, label: `New person "${pick.name}"` },
                    ]
                  : []),
                ...nameOptions.map((option) =>
                  sharedPick && option.name === groupKey(sharedPick)
                    ? { ...option, displayLabel: pick!.label }
                    : option,
                ),
                ...(names.status !== "loading" && (names.status === "error" || names.nextCursor)
                  ? [{ name: "\0more", label: names.status === "error" ? "Retry" : "Load more" }]
                  : []),
              ]}
              allowCustom={!!names.query.trim() && names.status === "ready" && !names.nextCursor}
              customLabel={'New person "{query}"'}
              loading={names.status === "loading"}
              error={names.error}
              disabled={suggestions.busy}
              onquery={(value) => void suggestions.searchNames(value)}
              onselect={(value) => {
                if (value === "\0more") {
                  if (names.status !== "loading") void suggestions.searchNames(names.query, true);
                  return false;
                }
                if (value.startsWith("\0group:")) {
                  openNamesakes(value.slice(7));
                  return true;
                }
                const row = names.people.find((row) => personKey(row.person_id) === value);
                if (row) pick = { id: row.person_id, name: row.display_name };
                else if (value && !value.startsWith("\0")) pick = { name: value.trim() };
                else return false;
                return true;
              }}
            /></span
          >
          <div class="modal-actions">
            <Button
              disabled={suggestions.busy}
              onclick={() => void suggestions.decide({ action: "reject" })}>Dismiss</Button
            >
            <Button
              tone={pick ? "info" : "neutral"}
              surface={pick ? "solid" : "outline"}
              disabled={suggestions.busy || !pick}
              onclick={confirmName}>Confirm</Button
            >
          </div>{/if}
      {:else}
        <p>Nothing left to review in this group.</p>
        {#if suggestions.writeError}<p role="alert">{suggestions.writeError}</p>{/if}
        <div class="modal-actions">
          <Button disabled={suggestions.busy} onclick={closeReview}>Done</Button>
        </div>
      {/if}
    </div>
  </Modal>
{/if}

<style>
  .suggestions {
    padding: 0 var(--space-7);
  }
  .suggestions h2 {
    display: flex;
    gap: var(--space-2);
    align-items: baseline;
    margin: 0 0 var(--space-3);
    font-size: var(--font-size-sm);
  }
  .suggestion-row {
    list-style: none;
    margin: 0;
    padding: 0 0 var(--space-2);
    display: flex;
    gap: 16px;
    overflow-x: auto;
  }
  .suggestion-row > li {
    flex: 0 0 184px;
  }
  .suggestion-more {
    display: flex;
    align-items: center;
  }
  .suggestion-card {
    width: 100%;
    height: 100%;
    font: inherit;
    color: var(--text-primary);
    text-align: left;
    background: none;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    cursor: pointer;
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: 8px;
    padding: 12px;
  }
  .suggestion-crop {
    width: 128px;
    height: 128px;
    flex-shrink: 0;
    position: relative;
    border-radius: 50%;
    overflow: hidden;
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--text-muted);
    background: var(--bg-inset);
  }
  .suggestion-title {
    display: -webkit-box;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    overflow: hidden;
  }
  .review-grid {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    grid-template-columns: repeat(auto-fit, 120px);
    justify-content: center;
    gap: var(--space-3);
    max-height: 55vh;
    overflow-y: auto;
  }
  .review-face {
    position: relative;
    width: 120px;
    height: 120px;
    overflow: hidden;
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-muted);
    display: flex;
    align-items: center;
    justify-content: center;
  }
  .remove-face {
    position: absolute;
    top: var(--space-1);
    right: var(--space-1);
    z-index: 1;
    width: 24px;
    height: 24px;
    padding: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    border: 0;
    border-radius: 50%;
    color: var(--text-primary);
    background: var(--bg-surface);
    cursor: pointer;
  }
  .remove-face:disabled {
    cursor: default;
    opacity: var(--opacity-disabled);
  }
  .name-picker {
    --typeahead-max-width: none;
    flex: 1;
    min-width: 0;
    --typeahead-control-height: 31px;
    --typeahead-control-font-size: var(--font-size-sm);
  }
  /* The list floats over Dismiss and Confirm, like the inspector's picker,
     so opening it never resizes or re-centres the dialog. It ends at the
     buttons' bottom edge (24px padding + 16px gap + 31px row - 2px offset),
     keeping the sheet's inset, and the buttons fade out under it rather than
     showing between short lists and the sheet's edge. Opacity, unlike
     visibility, leaves them in the Tab order the closing list hands off to. */
  .name-picker :global(.kit-typeahead__panel) {
    max-height: 69px;
  }
  .review-content .name-picker + .modal-actions {
    padding-top: var(--space-7);
  }
  .name-picker:has(:global(.kit-typeahead__panel)) + .modal-actions {
    opacity: 0;
    pointer-events: none;
  }
  .review-content {
    display: flex;
    flex-direction: column;
    gap: 16px;
    min-height: min(240px, 65vh);
  }
  .review-content p {
    margin: 0;
  }
  .saved-title {
    font-size: var(--font-size-sm);
    margin: 24px var(--space-7) 0;
  }
  .review-content .modal-actions {
    justify-content: space-between;
    margin-top: auto;
    padding-top: 0;
  }
  .people-workspace {
    flex: 1;
    min-width: 0;
    overflow: auto;
    background: var(--bg-surface);
  }
  /* The header draws no rule: the person cards below supply their own top
     edges, and a rule a few pixels above them would read as a second line. */
  .people-header {
    padding: var(--space-6) var(--space-7) var(--space-5);
  }
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
    padding: var(--space-5) var(--space-7);
    margin: 0;
    display: grid;
    grid-template-columns: repeat(auto-fill, 184px);
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
  .people-workspace > p {
    padding: var(--space-4);
  }
  @media (max-width: 640px) {
    .people-grid {
      padding: var(--space-3);
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .people-header {
      padding-inline: var(--space-6);
    }
  }
</style>
