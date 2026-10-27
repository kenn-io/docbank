<script lang="ts">
  import { reportFailure, reportSearch } from "./action-events.js";
  import { untrack } from "svelte";
  import { SearchInput, SelectDropdown } from "@kenn-io/kit-ui";
  import type { Photos } from "./photos.svelte.js";
  import { searchSyntaxOptions } from "./searchSyntax.js";
  import { photoSearchSort } from "./photoSearch.js";

  let { photos }: { photos: Photos } = $props();
  let search = $state(untrack(() => photos.query.text));
  let syntax = $state(untrack(() => photos.query.syntax));
  let mode = $state(untrack(() => photos.query.mode));
  const disabled = $derived(photos.blocking || photos.hiding || photos.trashing);
  const applyDraft = $derived(Boolean(search.trim()) || search !== photos.query.text);
  const appliedText = $derived(photos.query.text);
  const appliedSyntax = $derived(photos.query.syntax);
  const appliedMode = $derived(photos.query.mode);
  $effect(() => {
    search = appliedText;
  });
  $effect(() => {
    syntax = appliedSyntax;
  });
  $effect(() => {
    mode = appliedMode;
  });
  function submit(explicit = false) {
    const text = search.trim();
    const pending = photos.setQuery(
      {
        ...photos.query,
        text: search,
        syntax,
        mode,
        sort: photoSearchSort({ text: search, syntax, mode }),
      },
      explicit
        ? (cause) => {
            if (text) reportFailure(photos.session, "search", cause);
          }
        : undefined,
    );
    void pending?.then((outcome) => {
      if (!explicit || !text || !outcome) return;
      if (outcome.accepted)
        return reportSearch(
          photos.session,
          photos.fallbackReason || mode === "lexical" ? "text" : mode,
          outcome.empty,
        );
      reportFailure(photos.session, "search", outcome.cause);
    });
    return pending;
  }
</script>

<form
  class="search"
  onsubmit={(event) => {
    event.preventDefault();
    submit(true);
  }}
>
  <SearchInput
    block
    {disabled}
    ariaLabel="Search photos"
    placeholder={syntax === "advanced" ? "rating:5 pick label:red" : "Search photos"}
    bind:value={search}
    onclear={() => {
      mode = "lexical";
      if (photos.query.text && !submit()) {
        search = photos.query.text;
        mode = photos.query.mode;
      }
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
    if (applyDraft && !submit()) mode = photos.query.mode;
  }}
/>
<SelectDropdown
  title="Search mode"
  {disabled}
  value={syntax}
  options={searchSyntaxOptions}
  onchange={(value) => {
    syntax = value as "simple" | "advanced";
    if (applyDraft && !submit()) syntax = photos.query.syntax;
  }}
/>
