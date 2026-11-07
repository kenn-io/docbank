<script lang="ts">
  import { Button, Notice, Typeahead } from "@kenn-io/kit-ui";
  import {
    type PhotoFace,
    type PersonSummary,
    type PhotoPersonAssertion,
  } from "./generated/docbank.js";
  import type { PhotoFaces } from "./photoFaces.svelte.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import { APIError } from "./api-transport.js";
  import { readNamePage } from "./people.svelte.js";
  import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";
  import PersonNamesakes, {
    groupKey,
    namesakeGroup,
    namesakeOptions,
    personKey as key,
  } from "./PersonNamesakes.svelte";
  let {
    faces,
    session,
    onperson,
    cache,
  }: {
    faces: PhotoFaces;
    session: string;
    onperson?: (id: string, name: string) => void;
    cache?: PhotoPreviewCache;
  } = $props();
  type Search = {
    face: string;
    version: string;
    scope: number;
    query: string;
    people: PersonSummary[];
    nextCursor?: string;
    incompleteName: string;
    pendingCursor?: string;
    status: "idle" | "loading" | "ready" | "error" | "invalid";
    error: string;
    controller?: AbortController;
  };
  const emptySearch = (): Search => ({
    face: "",
    version: faces.version,
    scope: faces.hiddenAccess?.scope ?? 0,
    query: "",
    people: [],
    incompleteName: "",
    status: "idle",
    error: "",
  });
  let searchState = $state.raw<Search>(emptySearch());
  let writer: AbortController | undefined;
  let choosing = $state(false);
  let confirmation = $state<PhotoCullConfirmation>();
  let duplicate = $state.raw<{
    face?: PhotoFace;
    name: string;
    newName: string;
    people: PersonSummary[];
  }>();
  let selection = $state("");
  const groups = $derived(namesakeOptions(searchState.people, searchState.incompleteName));
  function closeChooser() {
    duplicate = undefined;
    selection = "";
  }
  function previewFailure(cause: unknown) {
    if (
      cause instanceof APIError &&
      (cause.status === 401 || (faces.hidden && cause.status === 403))
    ) {
      closeChooser();
      faces.onauthfailure(cause);
      return true;
    }
    return false;
  }
  function openChooser(face: PhotoFace | undefined, name: string) {
    closeChooser();
    const group = namesakeGroup(searchState.people, name, searchState.query);
    duplicate = { face: face ? { ...face } : undefined, ...group };
    selection =
      group.people.find((person) => person.person_id === (face && faces.person(face)?.person_id))
        ?.person_id ?? "";
  }
  async function assignSelection() {
    const selected = duplicate;
    if (!selected || !selection) return;
    const id = selection;
    closeChooser();
    choosing = true;
    try {
      if (!selected.face) await faces.mark(id);
      else
        await faces.name(
          selected.face,
          id === "new" ? "" : id,
          id === "new" ? selected.newName : undefined,
        );
    } finally {
      choosing = false;
    }
  }
  async function remove(face: PhotoFace) {
    const version = faces.version;
    const captured = { ...face };
    const person = faces.person(face);
    if (
      person &&
      !(await confirmation!.confirm(
        `Remove this face and its name, ${faces.personLabel(person.person_id)}? You can't undo this here.`,
        "Remove face",
      ))
    )
      return;
    if (faces.version === version) await faces.remove(captured);
  }
  async function clearMark(assertion: PhotoPersonAssertion) {
    const version = faces.version;
    const captured = { ...assertion };
    if (
      await confirmation!.confirm(
        `Remove ${faces.personLabel(assertion.person_id)} from this photo?`,
        "Clear whole-photo mark",
      )
    ) {
      if (faces.version === version) await faces.clearMark(captured);
    }
  }
  $effect(() => {
    void faces.version;
    void faces.hiddenAccess?.scope;
    void faces.hidden;
    searchState = emptySearch();
    closeChooser();
    return () => {
      closeChooser();
      searchState.controller?.abort();
      writer?.abort();
    };
  });
  async function readSearch(state: Search, cursor?: string) {
    state.controller?.abort();
    const request = new AbortController();
    const pending = {
      ...state,
      pendingCursor: cursor,
      status: "loading" as const,
      error: "",
      controller: request,
    };
    searchState = pending;
    const owns = () =>
      searchState === pending &&
      !request.signal.aborted &&
      faces.version === pending.version &&
      (faces.hiddenAccess?.scope ?? 0) === pending.scope;
    const next = await readNamePage(pending, {
      session,
      face: pending.face,
      signal: request.signal,
      run: (read) => (faces.hiddenAccess ? faces.hiddenAccess.run(read) : read()),
      failure: (cause) => {
        if (owns()) previewFailure(cause);
        return cause instanceof Error ? cause.message : String(cause);
      },
    });
    if (owns()) searchState = { ...pending, ...next };
  }
  async function search(faceID: string, value: string) {
    searchState.controller?.abort();
    await readSearch({ ...emptySearch(), face: faceID, query: value });
  }
  async function choose(face: PhotoFace | undefined, value: string) {
    if (choosing) return false;
    if (value === "\0more") {
      if (searchState.status !== "loading")
        await readSearch(
          searchState,
          searchState.status === "error" ? searchState.pendingCursor : searchState.nextCursor,
        );
      return false;
    }
    if (value.startsWith("\0group:")) {
      void openChooser(face, value.slice(7));
      return true;
    }
    const selected = searchState;
    const captured = face ? { ...face } : undefined;
    const person = selected.people.find((person) => key(person.person_id) === value);
    if (
      value &&
      !person &&
      (value.startsWith("\0") ||
        value.trim() !== selected.query.trim() ||
        selected.status !== "ready" ||
        selected.nextCursor)
    )
      return false;
    if (faces.version !== selected.version || (faces.hiddenAccess?.scope ?? 0) !== selected.scope)
      return false;
    choosing = true;
    let saved = false;
    const request = (writer = new AbortController());
    try {
      return (saved = !captured
        ? await faces.mark(person?.person_id ?? "", person ? undefined : value.trim())
        : !value
          ? await faces.clear(captured)
          : person
            ? await faces.name(captured, person.person_id)
            : await faces.name(captured, "", value.trim()));
    } finally {
      if (!saved && !request.signal.aborted && faces.version === selected.version)
        await search(captured?.face_id ?? "mark", selected.query);
      choosing = false;
    }
  }
</script>

<section aria-label="People" class="people">
  <h2>People</h2>
  {#if faces.loading}<span role="status">Loading people…</span>{/if}
  {#each faces.ordered as face (face.face_id)}
    {@const person = faces.person(face)}
    {@const foldedName = searchState.people.find(
      (match) => match.person_id === person?.person_id,
    )?.display_name_folded}
    {@const sharedName =
      person &&
      searchState.face === face.face_id &&
      foldedName &&
      searchState.people.filter((candidate) => candidate.display_name_folded === foldedName)
        .length > 1
        ? foldedName
        : ""}
    <div class="person-row">
      <span>{faces.number(face)}</span>
      <Typeahead
        remote
        title={`Name ${faces.number(face).toLowerCase()}`}
        placeholder="Find or create a person"
        fallbackLabel={faces.label(face)}
        value={person ? (sharedName ? groupKey(sharedName) : key(person.person_id)) : ""}
        options={[
          ...(person && !sharedName
            ? [{ name: key(person.person_id), label: faces.personLabel(person.person_id) }]
            : []),
          ...(searchState.face === face.face_id ? groups : [])
            .filter((option) => option.name !== (person ? key(person.person_id) : ""))
            .map((option) => ({
              ...option,
              ...(sharedName && option.name === groupKey(sharedName)
                ? { displayLabel: faces.personLabel(person!.person_id) }
                : {}),
            })),
          ...(searchState.face === face.face_id &&
          (searchState.status === "error" || searchState.nextCursor) &&
          searchState.status !== "loading"
            ? [{ name: "\0more", label: searchState.status === "error" ? "Retry" : "Load more" }]
            : []),
        ]}
        allowCustom={searchState.face === face.face_id &&
          !!searchState.query.trim() &&
          searchState.status === "ready" &&
          !searchState.nextCursor}
        customLabel={'Create person "{query}"'}
        loading={searchState.face === face.face_id && searchState.status === "loading"}
        error={searchState.face === face.face_id ? searchState.error : ""}
        allowClear={!!person}
        clearLabel="Clear name"
        disabled={choosing || faces.busy || faces.loading}
        onquery={(value) => search(face.face_id, value)}
        onselect={(value) =>
          person && !sharedName && value === key(person.person_id) ? true : choose(face, value)}
      />
      {#if person && onperson && faces.personActive(person.person_id)}<Button
          size="sm"
          onclick={() => onperson?.(person.person_id, faces.personLabel(person.person_id))}
          >{faces.personLabel(person.person_id)}</Button
        >{/if}
      <Button size="sm" disabled={faces.busy || faces.loading} onclick={() => remove(face)}
        >Remove face</Button
      >
    </div>
  {/each}
  {#if faces.ordered.length}
    <p>
      Search omits sensitive-only people and locked Hidden people. A new person may be a duplicate;
      merge them after unlock.
    </p>
  {/if}
  {#each faces.assertions.filter((row) => row.photo_mark && row.action === "assert") as person (person.assertion_id)}
    <div class="person-row">
      {#if onperson && faces.personActive(person.person_id)}<Button
          size="sm"
          onclick={() => onperson?.(person.person_id, faces.personLabel(person.person_id))}
          >{faces.personLabel(person.person_id)}</Button
        >
      {:else}<span>{faces.personLabel(person.person_id)}</span>{/if}
      <Button size="sm" disabled={faces.busy || faces.loading} onclick={() => clearMark(person)}
        >Clear whole-photo mark</Button
      >
    </div>
  {/each}
  <div class="person-row">
    <Typeahead
      remote
      title="Mark person in photo"
      placeholder="Mark person in photo"
      fallbackLabel="Mark person in photo"
      value=""
      options={searchState.face === "mark"
        ? [
            ...groups,
            ...(searchState.status !== "loading" &&
            (searchState.status === "error" || searchState.nextCursor)
              ? [{ name: "\0more", label: searchState.status === "error" ? "Retry" : "Load more" }]
              : []),
          ]
        : []}
      loading={searchState.face === "mark" && searchState.status === "loading"}
      allowCustom={searchState.face === "mark" &&
        !!searchState.query.trim() &&
        searchState.status === "ready" &&
        !searchState.nextCursor}
      customLabel={'Create person "{query}"'}
      error={searchState.face === "mark" ? searchState.error : ""}
      disabled={choosing || faces.busy || faces.loading}
      onquery={(value) => search("mark", value)}
      onselect={(value) => choose(undefined, value)}
    />
  </div>
  {#if faces.error}<Button
      size="sm"
      disabled={faces.loading || faces.busy}
      onclick={() => faces.load(faces.version)}>Retry</Button
    >{/if}
  {#if faces.error}<Notice tone="error" message={faces.error} />{/if}
</section>

<PhotoCullConfirmation bind:this={confirmation} />
{#if duplicate}
  {#key duplicate}<PersonNamesakes
      name={duplicate.name}
      people={duplicate.people}
      newName={duplicate.face ? duplicate.newName : undefined}
      confirmLabel={duplicate.face ? "Name face" : "Mark person in photo"}
      bind:selection
      hidden={faces.hidden}
      {session}
      {cache}
      run={faces.hiddenAccess ? (load) => faces.hiddenAccess!.run(load) : undefined}
      onfailure={previewFailure}
      onclose={closeChooser}
      onconfirm={assignSelection}
    />{/key}
{/if}

<style>
  .people {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  h2 {
    font-size: var(--font-size-sm);
    margin: 0;
  }
  .person-row {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
</style>
