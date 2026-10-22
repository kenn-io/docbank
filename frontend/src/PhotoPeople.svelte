<script lang="ts">
  import { Button, Typeahead, Notice, Modal } from "@kenn-io/kit-ui";
  import {
    listPeople,
    listPersonFacePreviews,
    preparePhotoPreview,
    type PhotoFace,
    type PersonSummary,
    type PhotoPersonAssertion,
  } from "./generated/docbank.js";
  import type { PhotoFaces } from "./photoFaces.svelte.js";
  import { previewObjectURL, type PhotoPreviewCache } from "./photoPreviewCache.js";
  import { recentPeople } from "./people.svelte.js";
  import { APIError } from "./api-transport.js";
  import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";
  import { photoFaceCropStyle } from "./photoRotation.js";
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
    recentIDs: string[];
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
    recentIDs: recentPeople(session),
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
  const detail = (person: PersonSummary) =>
    person.identity_label || new Date(person.created_at).toLocaleDateString();
  let selection = $state("");
  let expanded = $state("");
  let previews = $state.raw<
    Record<
      string,
      {
        images: { url: string; style: string; wholePhoto: boolean }[];
        loading: boolean;
        failed: boolean;
      }
    >
  >({});
  let previewController: AbortController | undefined;
  let releases: (() => void)[] = [];
  const groupKey = (name: string) => `\0group:${name}`;
  const groups = $derived.by(() => {
    const grouped = new Map<string, PersonSummary[]>();
    for (const person of searchState.people)
      grouped.set(person.display_name_folded, [
        ...(grouped.get(person.display_name_folded) ?? []),
        person,
      ]);
    return [...grouped]
      .filter(([name]) => name !== searchState.incompleteName)
      .map(([name, members]) => ({
        name: members.length > 1 ? groupKey(name) : key(members[0]!.person_id),
        label:
          members.length > 1
            ? `${members[0]!.display_name} · ${members.length} people`
            : members[0]!.display_name,
      }));
  });
  function closePreviews() {
    previewController?.abort();
    for (const release of releases) release();
    releases = [];
    previews = {};
    expanded = "";
  }
  function closeChooser() {
    closePreviews();
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
    }
  }
  async function loadPreviews(
    person: PersonSummary,
    request: AbortController,
    members: PersonSummary[],
  ) {
    try {
      request.signal.throwIfAborted();
      const tied = members.some(
        (other) => other.person_id !== person.person_id && detail(other) === detail(person),
      );
      let failed = false;
      const page = await listPersonFacePreviews(
        person.person_id,
        { hidden: faces.hidden },
        { session, signal: request.signal },
      );
      request.signal.throwIfAborted();
      for (const row of page.items) {
        if ((tied && !row.box) || !cache) {
          failed = true;
          continue;
        }
        try {
          let generation = row.generation_id;
          if (!generation) {
            const slot = await preparePhotoPreview(
              row.asset_id,
              {
                content_version_id: row.content_version_id,
                person_id: person.person_id,
                size: "grid",
                hidden: faces.hidden,
              },
              { session, signal: request.signal },
            );
            if (slot.state !== "ready" || !slot.generation_id) {
              failed = true;
              continue;
            }
            generation = slot.generation_id;
          }
          request.signal.throwIfAborted();
          const url = await new Promise<string>((resolve, reject) => {
            const abort = () => reject(request.signal.reason);
            request.signal.addEventListener("abort", abort, { once: true });
            const finish = (action: () => void) => {
              request.signal.removeEventListener("abort", abort);
              action();
            };
            releases.push(
              previewObjectURL(
                cache,
                row.asset_id,
                generation,
                (url) => finish(() => resolve(url)),
                (cause) => finish(() => reject(cause)),
                false,
                person.person_id,
              ),
            );
          });
          const image = new Image();
          image.src = url;
          await image.decode();
          request.signal.throwIfAborted();
          const state = previews[person.person_id]!;
          previews = {
            ...previews,
            [person.person_id]: {
              ...state,
              images: [
                ...state.images,
                {
                  url,
                  style: photoFaceCropStyle(
                    row.box,
                    image.naturalWidth,
                    image.naturalHeight,
                    row.rotation,
                    64,
                  ),
                  wholePhoto: !row.box,
                },
              ],
            },
          };
        } catch (cause) {
          if (request.signal.aborted) return;
          previewFailure(cause);
          if (request.signal.aborted) return;
          failed = true;
        }
      }
      if (!request.signal.aborted)
        previews = {
          ...previews,
          [person.person_id]: {
            ...previews[person.person_id]!,
            loading: false,
            failed: !previews[person.person_id]!.images.length && (failed || tied),
          },
        };
    } catch (cause) {
      if (request.signal.aborted) return;
      previewFailure(cause);
      if (!request.signal.aborted)
        previews = {
          ...previews,
          [person.person_id]: { ...previews[person.person_id]!, loading: false, failed: true },
        };
    }
  }
  function openChooser(face: PhotoFace | undefined, name: string) {
    closeChooser();
    const members = searchState.people.filter((person) => person.display_name_folded === name);
    duplicate = {
      face: face ? { ...face } : undefined,
      name: members[0]!.display_name,
      newName: searchState.query.trim() || members[0]!.display_name,
      people: members,
    };
    selection =
      members.find((person) => person.person_id === (face && faces.person(face)?.person_id))
        ?.person_id ?? "";
  }
  async function expandPreviews(person: PersonSummary) {
    const previous = expanded;
    closePreviews();
    if (previous === person.person_id || !duplicate) return;
    expanded = person.person_id;
    previews = { [person.person_id]: { images: [], loading: true, failed: false } };
    const request = (previewController = new AbortController());
    const members = duplicate.people;
    const load = () => loadPreviews(person, request, members);
    await (faces.hiddenAccess ? faces.hiddenAccess.run(load) : load());
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
  const key = (id: string) => `\0${id}`;
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
    if (new TextEncoder().encode(pending.query.trim()).length > 200) {
      searchState = { ...pending, status: "invalid", error: "Name must be 200 bytes or fewer." };
      return;
    }
    try {
      const fetch = () => {
        request.signal.throwIfAborted();
        return listPeople(
          {
            query: pending.query.trim(),
            limit: 100,
            ...(pending.recentIDs.length ? { recent_ids: pending.recentIDs.join(",") } : {}),
            ...(cursor ? { cursor } : {}),
          },
          { session, signal: request.signal },
        );
      };
      const page = await (faces.hiddenAccess ? faces.hiddenAccess.run(fetch) : fetch());
      if (owns())
        searchState = {
          ...pending,
          people: [...pending.people, ...page.items],
          nextCursor: page.next_cursor,
          incompleteName:
            page.next_cursor && page.same_name_continues
              ? page.items.at(-1)!.display_name_folded
              : "",
          status: "ready",
        };
    } catch (cause) {
      if (owns()) {
        previewFailure(cause);
        searchState = {
          ...pending,
          status: "error",
          error: cause instanceof Error ? cause.message : String(cause),
        };
      }
    }
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
  <Modal title={`Choose ${duplicate.name}`} onclose={closeChooser}>
    <div class="duplicate-list">
      {#each duplicate.people as person (person.person_id)}
        {@const preview = previews[person.person_id]}
        <div class="duplicate-person">
          <label>
            <input type="radio" name="person" value={person.person_id} bind:group={selection} />
            <span
              >{person.display_name}<small>{detail(person)}</small>
              <small>{person.person_id}</small></span
            >
          </label>
          <Button
            size="sm"
            ariaExpanded={expanded === person.person_id}
            onclick={() => expandPreviews(person)}>Photos</Button
          >
          {#if expanded === person.person_id}
            <div class="candidate-previews">
              {#each preview?.images ?? [] as image}<span class="person-preview"
                  ><img
                    src={image.url}
                    alt={`Photos of ${person.display_name}`}
                    style={image.style}
                  />{#if image.wholePhoto}<small class="whole-photo">Whole photo</small>{/if}</span
                >{/each}
              {#if preview?.loading}<span>Loading photos…</span>
              {:else if preview?.failed}<span>Preview unavailable</span>
              {:else if !preview?.images.length}<span>No photos you can see.</span>{/if}
            </div>
          {/if}
        </div>
      {/each}
    </div>
    {#if duplicate.face}<label
        ><input type="radio" name="person" value="new" bind:group={selection} />New person named {duplicate.newName}</label
      >{/if}
    {#snippet footer()}
      <Button onclick={closeChooser}>Cancel</Button>
      <Button disabled={!selection} onclick={assignSelection}
        >{duplicate?.face ? "Name face" : "Mark person in photo"}</Button
      >
    {/snippet}
  </Modal>
{/if}

<style>
  .duplicate-list {
    max-height: 55vh;
    overflow-y: auto;
  }
  .candidate-previews {
    display: flex;
    flex-basis: 100%;
    gap: var(--space-2);
  }
  .duplicate-person {
    flex-wrap: wrap;
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-bottom: var(--space-3);
  }
  /* Keep the action outside the radio label so it stays out of the radio's name. */
  .duplicate-person label {
    display: contents;
  }
  .duplicate-person small {
    display: block;
    overflow-wrap: anywhere;
  }
  .duplicate-person label > span:first-of-type {
    min-width: 0;
  }
  .person-preview {
    position: relative;
    width: 64px;
    height: 64px;
    flex-shrink: 0;
    overflow: hidden;
  }
  .person-preview img {
    position: absolute;
    top: 50%;
    left: 50%;
    max-width: none;
  }
  .whole-photo {
    position: absolute;
    bottom: 0;
    background: var(--bg-inset);
  }
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
