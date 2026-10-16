<script lang="ts">
  import { onDestroy } from "svelte";
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
  }: { faces: PhotoFaces; session: string; onperson?: (id: string) => void } = $props();
  let people = $state.raw<PersonSummary[]>([]);
  let query = $state("");
  let searching = $state(false);
  let error = $state("");
  let controller: AbortController | undefined;
  const key = (id: string) => `\0${id}`;
  const exact = $derived(people.some((person) => person.display_name === query.trim()));
  $effect(() => {
    void faces.version;
    people = [];
    query = "";
    error = "";
    return () => controller?.abort();
  });
  onDestroy(() => controller?.abort());
  async function search(value: string) {
    query = value;
    controller?.abort();
    const request = (controller = new AbortController());
    people = [];
    searching = true;
    error = "";
    try {
      const page = await listPeople(
        { query: value, limit: 100 },
        { session, signal: request.signal },
      );
      if (!request.signal.aborted) people = page.items;
    } catch (cause) {
      if (!request.signal.aborted) error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!request.signal.aborted) searching = false;
    }
  }
  async function choose(face: PhotoFace, value: string) {
    const version = faces.version;
    const request = (controller ??= new AbortController());
    let person = people.find((person) => key(person.person_id) === value);
    try {
      if (!person)
        person = await createPerson(
          { display_name: value.trim() },
          { session, signal: request.signal },
        );
      if (request.signal.aborted || faces.version !== version) return false;
      return await faces.name(face, person.person_id);
    } catch (cause) {
      if (!request.signal.aborted) error = cause instanceof Error ? cause.message : String(cause);
      return false;
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
        options={people.map((person) => ({
          name: key(person.person_id),
          label: person.display_name,
        }))}
        allowCustom={!exact && !searching && !error}
        customLabel={'Create person "{query}"'}
        loading={searching}
        disabled={faces.busy || faces.loading}
        onquery={search}
        onselect={(value) => choose(face, value)}
      />
      {#if person && onperson}<Button size="sm" onclick={() => onperson?.(person.person_id)}
          >{person.display_name}</Button
        >{/if}
    </div>
  {/each}
  {#each faces.assertions.filter((row) => row.photo_mark && row.action === "assert") as person (person.assertion_id)}
    <Button size="sm" onclick={() => onperson?.(person.person_id)}>{person.display_name}</Button>
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
