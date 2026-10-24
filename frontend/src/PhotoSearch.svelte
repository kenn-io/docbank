<script lang="ts">
  import { reportFailure, reportSearch } from "./action-events.js";
  import { untrack } from "svelte";
  import { SearchInput, SelectDropdown } from "@kenn-io/kit-ui";
  import type { Photos } from "./photos.svelte.js";
  import { photoSearchSort } from "./photoSearch.js";

  let { photos }: { photos: Photos } = $props();
  let search = $state(untrack(() => photos.baseQuery.text));
  let syntax = $state(untrack(() => photos.baseQuery.syntax));
  const disabled = $derived(photos.loading || photos.hiding || photos.trashing);
  const applyDraft = $derived(Boolean(search.trim()) || search !== photos.baseQuery.text);
  const appliedText = $derived(photos.baseQuery.text);
  const appliedSyntax = $derived(photos.baseQuery.syntax);
  $effect(() => {
    search = appliedText;
  });
  $effect(() => {
    syntax = appliedSyntax;
  });
  function submit() {
    const text = search.trim();
    const pending = photos.setQuery(
      {
        ...photos.baseQuery,
        text: search,
        syntax,
        sort: photoSearchSort({ text: search, syntax }),
      },
      (cause) => {
        if (text) reportFailure(photos.session, "search", cause);
      },
    );
    void pending?.then((outcome) => {
      if (!text || !outcome) return;
      if (outcome.accepted) return reportSearch(photos.session, "text", outcome.empty);
      reportFailure(photos.session, "search", outcome.cause);
    });
    return pending;
  }
</script>

<form
  class="search"
  onsubmit={(event) => {
    event.preventDefault();
    submit();
  }}
>
  <SearchInput
    block
    {disabled}
    ariaLabel="Search photos"
    placeholder="Search photos"
    bind:value={search}
    onclear={() => {
      if (photos.baseQuery.text && !submit()) search = photos.baseQuery.text;
    }}
  />
</form>
<SelectDropdown
  title="Search mode"
  {disabled}
  value={syntax}
  options={[
    { value: "simple", label: "Plain words" },
    { value: "advanced", label: "Query syntax" },
  ]}
  onchange={(value) => {
    syntax = value as "simple" | "advanced";
    if (applyDraft && !submit()) syntax = photos.baseQuery.syntax;
  }}
/>
