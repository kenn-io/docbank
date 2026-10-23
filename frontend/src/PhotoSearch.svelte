<script lang="ts">
  import { reportAction } from "./action-events.js";
  import { untrack } from "svelte";
  import { SearchInput, SelectDropdown } from "@kenn-io/kit-ui";
  import type { Photos } from "./photos.svelte.js";
  import { photoSearchSort } from "./photoSearch.js";

  let { photos }: { photos: Photos } = $props();
  let search = $state(untrack(() => photos.baseQuery.text));
  let syntax = $state(untrack(() => photos.baseQuery.syntax));
  let mode = $state(untrack(() => photos.baseQuery.mode));
  const disabled = $derived(photos.loading || photos.hiding || photos.trashing);
  const applyDraft = $derived(Boolean(search.trim()) || search !== photos.baseQuery.text);
  const appliedText = $derived(photos.baseQuery.text);
  const appliedSyntax = $derived(photos.baseQuery.syntax);
  const appliedMode = $derived(photos.baseQuery.mode);
  $effect(() => {
    search = appliedText;
  });
  $effect(() => {
    syntax = appliedSyntax;
  });
  $effect(() => {
    mode = appliedMode;
  });
  function submit() {
    const text = search.trim();
    const pending = photos.setQuery({
      ...photos.baseQuery,
      text: search,
      syntax,
      mode,
      sort: photoSearchSort({ text: search, syntax, mode }),
    });
    void pending?.then((accepted) => {
      if (accepted && text) reportAction(photos.session, "search_run", mode);
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
  title="Find by"
  {disabled}
  value={mode}
  options={[
    { value: "lexical", label: "Metadata" },
    { value: "semantic", label: "Meaning" },
    { value: "hybrid", label: "Hybrid" },
  ]}
  onchange={(value) => {
    mode = value as "lexical" | "semantic" | "hybrid";
    if (applyDraft && !submit()) mode = photos.baseQuery.mode;
  }}
/>
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
