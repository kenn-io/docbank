<script lang="ts">
  import { Button, Typeahead, Notice } from "@kenn-io/kit-ui";
  import {
    listPeople,
    createPerson,
    type PhotoFace,
    type PersonSummary,
  } from "./generated/docbank.js";
  import type { PhotoFaces } from "./photoFaces.svelte.js";
  let {
    faces,
    session,
    onperson,
  }: { faces: PhotoFaces; session: string; onperson?: (id: string, name: string) => void } =
    $props();
  let people = $state.raw<PersonSummary[]>([]);
  let query = $state("");
  let activeFace = $state("");
  let searching = $state(false);
  let error = $state("");
  let controller: AbortController | undefined;
  let writer: AbortController | undefined;
  let choosing = $state(false);
  const created = new Map<string, PersonSummary>();
  const key = (id: string) => `\0${id}`;
  let exact = $state(false);
  $effect(() => {
    void faces.version;
    people = [];
    query = "";
    activeFace = "";
    error = "";
    return () => {
      controller?.abort();
      writer?.abort();
      created.clear();
    };
  });
  async function search(faceID: string, value: string) {
    activeFace = faceID;
    query = value;
    controller?.abort();
    const request = (controller = new AbortController());
    people = [];
    searching = true;
    exact = false;
    error = "";
    try {
      const page = await listPeople(
        { query: value, limit: 100 },
        { session, signal: request.signal },
      );
      if (!request.signal.aborted) {
        people = page.items;
        exact = page.exact_match ?? false;
      }
    } catch (cause) {
      if (!request.signal.aborted) error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!request.signal.aborted) searching = false;
    }
  }
  async function choose(face: PhotoFace, value: string) {
    if (choosing) return false;
    choosing = true;
    error = "";
    const version = faces.version;
    const request = (writer = new AbortController());
    try {
      if (!value) return await faces.clear(face);
      let person =
        people.find((person) => key(person.person_id) === value) ?? created.get(value.trim());
      if (!person) {
        person = await createPerson(
          { display_name: value.trim() },
          { session, signal: request.signal },
        );
        created.set(value.trim(), person);
      }
      if (request.signal.aborted || faces.version !== version) return false;
      return await faces.name(face, person.person_id);
    } catch (cause) {
      if (!request.signal.aborted) error = cause instanceof Error ? cause.message : String(cause);
      return false;
    } finally {
      choosing = false;
    }
  }
</script>

<section aria-label="People" class="people">
  <h2>People</h2>
  {#if faces.loading}<span role="status">Loading people…</span>{/if}
  {#each faces.faces as face (face.face_id)}
    {@const person = faces.person(face)}
    <div class="person-row">
      <Typeahead
        remote
        title="Name face"
        placeholder="Find or create a person"
        fallbackLabel={faces.label(face)}
        value={person ? key(person.person_id) : ""}
        options={[
          ...(person
            ? [{ name: key(person.person_id), label: faces.personLabel(person.person_id) }]
            : []),
          ...(activeFace === face.face_id ? people : [])
            .filter((candidate) => candidate.person_id !== person?.person_id)
            .map((candidate) => ({
              name: key(candidate.person_id),
              label: candidate.display_name,
            })),
        ]}
        allowCustom={activeFace === face.face_id &&
          !!query.trim() &&
          !exact &&
          !searching &&
          !error}
        customLabel={'Create person "{query}"'}
        loading={activeFace === face.face_id && searching}
        allowClear={!!person}
        clearLabel="Clear name"
        disabled={choosing || faces.busy || faces.loading}
        onquery={(value) => search(face.face_id, value)}
        onselect={(value) => choose(face, value)}
      />
      {#if person && onperson}<Button
          size="sm"
          onclick={() => onperson?.(person.person_id, faces.personLabel(person.person_id))}
          >{faces.personLabel(person.person_id)}</Button
        >{/if}
    </div>
  {/each}
  {#each faces.assertions.filter((row) => row.photo_mark && row.action === "assert") as person (person.assertion_id)}
    <Button
      size="sm"
      onclick={() => onperson?.(person.person_id, faces.personLabel(person.person_id))}
      >{faces.personLabel(person.person_id)}</Button
    >
  {/each}
  {#if faces.error || error}<Notice tone="error" message={faces.error || error} />{/if}
</section>

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
