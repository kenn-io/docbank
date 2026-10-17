<script lang="ts">
  import { Button, Typeahead, Notice, Modal } from "@kenn-io/kit-ui";
  import {
    listPeople,
    listPhotoAssets,
    type PhotoFace,
    type PersonSummary,
    type PhotoPersonAssertion,
  } from "./generated/docbank.js";
  import type { PhotoFaces } from "./photoFaces.svelte.js";
  import { previewObjectURL, type PhotoPreviewCache } from "./photoPreviewCache.js";
  import { APIError } from "./api-transport.js";
  import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";
  let {
    faces,
    session,
    onperson,
    cache,
    assetID,
  }: {
    faces: PhotoFaces;
    session: string;
    onperson?: (id: string, name: string) => void;
    cache?: PhotoPreviewCache;
    assetID?: string;
  } = $props();
  let people = $state.raw<PersonSummary[]>([]);
  let query = $state("");
  let activeFace = $state("");
  let searching = $state(false);
  let error = $state("");
  let controller: AbortController | undefined;
  let writer: AbortController | undefined;
  let choosing = $state(false);
  let confirmation = $state<PhotoCullConfirmation>();
  let duplicate = $state.raw<{ face: PhotoFace; name: string; people: PersonSummary[] }>();
  let selection = $state("");
  let previews = $state.raw<Record<string, { urls: string[]; loading: boolean; failed: boolean }>>(
    {},
  );
  let previewController: AbortController | undefined;
  let releases: (() => void)[] = [];
  const groupKey = (name: string) => `\0group:${name}`;
  const groups = $derived.by(() => {
    const grouped = new Map<string, PersonSummary[]>();
    for (const person of people)
      grouped.set(person.display_name_folded, [
        ...(grouped.get(person.display_name_folded) ?? []),
        person,
      ]);
    return [...grouped].map(([name, members]) => ({
      name: members.length > 1 ? groupKey(name) : key(members[0]!.person_id),
      label:
        members.length > 1
          ? `${members[0]!.display_name} · ${members.length} people`
          : members[0]!.display_name,
    }));
  });
  function closeChooser() {
    previewController?.abort();
    for (const release of releases) release();
    releases = [];
    previews = {};
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
  async function openChooser(face: PhotoFace, name: string) {
    closeChooser();
    const members = people.filter((person) => person.display_name_folded === name);
    duplicate = { face: { ...face }, name: members[0]!.display_name, people: members };
    selection =
      members.find((person) => person.person_id === faces.person(face)?.person_id)?.person_id ?? "";
    const request = (previewController = new AbortController());
    previews = Object.fromEntries(
      members.map((person) => [person.person_id, { urls: [], loading: true, failed: false }]),
    );
    for (const person of members) {
      try {
        const read = () => {
          request.signal.throwIfAborted();
          return listPhotoAssets(
            {
              query: {
                text: "",
                syntax: "simple",
                filters: { person_ids: [person.person_id] },
                sort: { field: "capture_time", direction: "desc" },
              },
              hidden: faces.hidden,
              page_size: 4,
              stacks: false,
              facets: [],
            },
            { session, signal: request.signal },
          );
        };
        const page = await (faces.hiddenAccess ? faces.hiddenAccess.run(read) : read());
        if (request.signal.aborted) return;
        const rows = page.items
          .filter((row) => row.asset_id !== assetID && row.content_version_id !== faces.version)
          .slice(0, 3);
        previews = {
          ...previews,
          [person.person_id]: {
            urls: [],
            loading: rows.length > 0 && !!cache,
            failed: rows.length > 0 && !cache,
          },
        };
        for (const row of rows) {
          if (!cache) continue;
          if (!row.previews.grid.generation_id) {
            previews = {
              ...previews,
              [person.person_id]: { ...previews[person.person_id]!, loading: false, failed: true },
            };
            continue;
          }
          releases.push(
            previewObjectURL(
              cache,
              row.asset_id,
              row.previews.grid.generation_id,
              (url) => {
                if (request.signal.aborted) return;
                const state = previews[person.person_id]!;
                previews = {
                  ...previews,
                  [person.person_id]: { ...state, loading: false, urls: [...state.urls, url] },
                };
              },
              (cause) => {
                if (request.signal.aborted) return;
                previewFailure(cause);
                if (!request.signal.aborted)
                  previews = {
                    ...previews,
                    [person.person_id]: {
                      ...previews[person.person_id]!,
                      loading: false,
                      failed: true,
                    },
                  };
              },
              false,
              faces.hiddenAccess,
            ),
          );
        }
      } catch (cause) {
        if (request.signal.aborted) return;
        previewFailure(cause);
        if (!request.signal.aborted)
          previews = {
            ...previews,
            [person.person_id]: { urls: [], loading: false, failed: true },
          };
      }
    }
  }
  async function assignSelection() {
    const selected = duplicate;
    if (!selected || !selection) return;
    const id = selection;
    closeChooser();
    choosing = true;
    try {
      await faces.name(
        selected.face,
        id === "new" ? "" : id,
        id === "new" ? selected.name : undefined,
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
        `Remove ${faces.personLabel(assertion.person_id)} from this photo? You can't add a whole-photo mark back from the browser.`,
        "Clear whole-photo mark",
      )
    ) {
      if (faces.version === version) await faces.clearMark(captured);
    }
  }
  const key = (id: string) => `\0${id}`;
  $effect(() => {
    void faces.version;
    people = [];
    query = "";
    activeFace = "";
    error = "";
    return () => {
      closeChooser();
      controller?.abort();
      writer?.abort();
    };
  });
  async function search(faceID: string, value: string) {
    activeFace = faceID;
    query = value;
    const normalized = value.trim();
    controller?.abort();
    const request = (controller = new AbortController());
    people = [];
    searching = true;
    error = "";
    if (new TextEncoder().encode(normalized).length > 200) {
      error = "Name must be 200 bytes or fewer.";
      searching = false;
      return;
    }
    try {
      const read = (cursor?: string) => {
        const fetch = () => {
          request.signal.throwIfAborted();
          return listPeople(
            { query: normalized, limit: 100, ...(cursor ? { cursor } : {}) },
            { session, signal: request.signal },
          );
        };
        return faces.hiddenAccess ? faces.hiddenAccess.run(fetch) : fetch();
      };
      let page = await read();
      const matches = [...page.items];
      const tail = matches.at(-1)?.display_name_folded;
      while (
        !request.signal.aborted &&
        page.next_cursor &&
        page.same_name_continues &&
        tail !== undefined
      ) {
        page = await read(page.next_cursor);
        const boundary = page.items.findIndex((person) => person.display_name_folded !== tail);
        matches.push(...(boundary < 0 ? page.items : page.items.slice(0, boundary)));
        if (boundary >= 0 || !page.items.length) break;
      }
      if (!request.signal.aborted) people = matches;
    } catch (cause) {
      if (!request.signal.aborted) {
        previewFailure(cause);
        error = cause instanceof Error ? cause.message : String(cause);
      }
    } finally {
      if (!request.signal.aborted) searching = false;
    }
  }
  async function choose(face: PhotoFace, value: string) {
    if (choosing) return false;
    if (value.startsWith("\0group:")) {
      void openChooser(face, value.slice(7));
      return true;
    }
    choosing = true;
    error = "";
    const version = faces.version;
    const selectedQuery = query;
    let saved = false;
    const request = (writer = new AbortController());
    try {
      if (!value) return (saved = await faces.clear(face));
      const person = people.find((person) => key(person.person_id) === value);
      if (!person && (value.startsWith("\0") || value.trim() !== query.trim())) return false;
      return (saved = person
        ? await faces.name(face, person.person_id)
        : await faces.name(face, "", value.trim()));
    } finally {
      if (!saved && !request.signal.aborted && faces.version === version)
        await search(face.face_id, selectedQuery);
      choosing = false;
    }
  }
</script>

<section aria-label="People" class="people">
  <h2>People</h2>
  {#if faces.loading}<span role="status">Loading people…</span>{/if}
  {#each faces.ordered as face (face.face_id)}
    {@const person = faces.person(face)}
    {@const foldedName = people.find(
      (match) => match.person_id === person?.person_id,
    )?.display_name_folded}
    {@const sharedName =
      person &&
      activeFace === face.face_id &&
      foldedName &&
      people.filter((candidate) => candidate.display_name_folded === foldedName).length > 1
        ? foldedName
        : ""}
    <div class="person-row">
      <span>{faces.number(face)}</span>
      <Typeahead
        remote
        title="Name face"
        placeholder="Find or create a person"
        fallbackLabel={faces.label(face)}
        value={person ? (sharedName ? groupKey(sharedName) : key(person.person_id)) : ""}
        options={[
          ...(person && !sharedName
            ? [{ name: key(person.person_id), label: faces.personLabel(person.person_id) }]
            : []),
          ...(activeFace === face.face_id ? groups : [])
            .filter((option) => option.name !== (person ? key(person.person_id) : ""))
            .map((option) => ({
              ...option,
              ...(sharedName && option.name === groupKey(sharedName)
                ? { displayLabel: faces.personLabel(person!.person_id) }
                : {}),
            })),
        ]}
        allowCustom={activeFace === face.face_id && !!query.trim() && !searching && !error}
        customLabel={'Create person "{query}"'}
        loading={activeFace === face.face_id && searching}
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
  {#if faces.error}<Button
      size="sm"
      disabled={faces.loading || faces.busy}
      onclick={() => faces.load(faces.version)}>Retry</Button
    >{/if}
  {#if faces.error || error}<Notice tone="error" message={faces.error || error} />{/if}
</section>

<PhotoCullConfirmation bind:this={confirmation} />
{#if duplicate}
  <Modal title={`Choose ${duplicate.name}`} onclose={closeChooser}>
    {#each duplicate.people as person (person.person_id)}
      {@const preview = previews[person.person_id]}
      <label class="duplicate-person">
        <input type="radio" name="person" value={person.person_id} bind:group={selection} />
        <span
          >{person.display_name}<small
            >{person.identity_label || new Date(person.created_at).toLocaleDateString()}</small
          ></span
        >
        {#each preview?.urls ?? [] as url}<img
            src={url}
            alt={`Photos of ${person.display_name}`}
          />{/each}
        {#if preview?.loading}<span>Loading photos…</span>
        {:else if preview?.failed}<span>Preview unavailable</span>
        {:else if !preview?.urls.length}<span>No photos you can see.</span>{/if}
      </label>
    {/each}
    <label
      ><input type="radio" name="person" value="new" bind:group={selection} />New person named {duplicate.name}</label
    >
    {#snippet footer()}
      <Button onclick={closeChooser}>Cancel</Button>
      <Button disabled={!selection} onclick={assignSelection}>Name face</Button>
    {/snippet}
  </Modal>
{/if}

<style>
  .duplicate-person {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-bottom: var(--space-3);
  }
  .duplicate-person small {
    display: block;
  }
  .duplicate-person img {
    width: 64px;
    height: 64px;
    object-fit: cover;
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
