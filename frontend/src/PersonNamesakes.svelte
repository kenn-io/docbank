<script module lang="ts">
  import type { PersonSummary } from "./generated/docbank.js";
  export const personKey = (id: string) => `\0${id}`;
  export const groupKey = (name: string) => `\0group:${name}`;
  // namesakeOptions lists a person search as Typeahead options. People who
  // share a name become one option that opens the chooser; a name the next
  // page continues is left out until it is whole.
  export const namesakeOptions = (people: readonly PersonSummary[], incompleteName = "") => {
    const grouped = new Map<string, PersonSummary[]>();
    for (const person of people)
      grouped.set(person.display_name_folded, [
        ...(grouped.get(person.display_name_folded) ?? []),
        person,
      ]);
    return [...grouped]
      .filter(([name]) => name !== incompleteName)
      .map(([name, members]) => ({
        name: members.length > 1 ? groupKey(name) : personKey(members[0]!.person_id),
        label:
          members.length > 1
            ? `${members[0]!.display_name} · ${members.length} people`
            : members[0]!.display_name,
      }));
  };
  // namesakeGroup opens the chooser on the people a search found under one
  // folded name. Search matches word prefixes, so a new person keeps the
  // typed spelling only when it is the whole name.
  export const namesakeGroup = (people: readonly PersonSummary[], name: string, query: string) => {
    const members = people.filter((person) => person.display_name_folded === name);
    const typed = query.trim();
    const folded = typed.normalize("NFKC").toLowerCase().split(/\s+/).join(" ");
    return {
      name: members[0]!.display_name,
      newName: typed && folded === name ? typed : members[0]!.display_name,
      people: members,
    };
  };
</script>

<script lang="ts">
  import { Button, Modal, RadioGroup } from "@kenn-io/kit-ui";
  import { listPersonFacePreviews } from "./generated/docbank.js";
  import { faceCrop, type PhotoPreviewCache } from "./photoPreviewCache.js";
  import { personDetail as detail } from "./personLabel.js";
  // The namesake chooser tells people who share a name apart by detail, ID,
  // and up to three of their photos. onfailure returns true when the failure
  // closes the chooser, which stops its preview reads.
  let {
    name,
    people,
    newName,
    confirmLabel,
    selection = $bindable(""),
    hidden,
    session,
    cache,
    run = (request) => request(),
    onfailure,
    onclose,
    onconfirm,
  }: {
    name: string;
    people: PersonSummary[];
    newName?: string;
    confirmLabel: string;
    selection: string;
    hidden: boolean;
    session: string;
    cache?: PhotoPreviewCache;
    run?: (request: () => Promise<void>) => Promise<void>;
    onfailure: (cause: unknown) => boolean;
    onclose: () => void;
    onconfirm: () => void;
  } = $props();
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
  function closePreviews() {
    previewController?.abort();
    for (const release of releases) release();
    releases = [];
    previews = {};
    expanded = "";
  }
  $effect(() => closePreviews);
  function previewFailure(cause: unknown) {
    if (onfailure(cause)) closePreviews();
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
        { hidden },
        { session, signal: request.signal },
      );
      request.signal.throwIfAborted();
      for (const row of page.items) {
        if ((tied && !row.box) || !cache) {
          failed = true;
          continue;
        }
        try {
          const image = await faceCrop(
            cache,
            row,
            { id: person.person_id, hidden, size: 64 },
            request.signal,
            (release) => releases.push(release),
          );
          if (!image) {
            failed = true;
            continue;
          }
          const state = previews[person.person_id]!;
          previews = {
            ...previews,
            [person.person_id]: {
              ...state,
              images: [...state.images, { ...image, wholePhoto: !row.box }],
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
  async function expandPreviews(person: PersonSummary) {
    const previous = expanded;
    closePreviews();
    if (previous === person.person_id) return;
    expanded = person.person_id;
    previews = { [person.person_id]: { images: [], loading: true, failed: false } };
    const request = (previewController = new AbortController());
    const members = people;
    await run(() => loadPreviews(person, request, members));
  }
</script>

<Modal title={`Choose ${name}`} {onclose}>
  <RadioGroup
    legend="Matching people"
    name="person"
    value={selection || undefined}
    options={[
      ...people.map((person) => ({
        value: person.person_id,
        label: `${person.display_name} ${detail(person)} ${person.person_id}`,
      })),
      ...(newName !== undefined ? [{ value: "new", label: `New person named ${newName}` }] : []),
    ]}
    onchange={(value) => {
      selection = value ?? "";
    }}
  />
  <div class="duplicate-list">
    {#each people as person (person.person_id)}
      {@const preview = previews[person.person_id]}
      <div class="duplicate-person" data-person={person.person_id}>
        <span>{person.display_name}</span>
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
  {#snippet footer()}
    <Button onclick={onclose}>Cancel</Button>
    <Button disabled={!selection} onclick={onconfirm}>{confirmLabel}</Button>
  {/snippet}
</Modal>

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
</style>
