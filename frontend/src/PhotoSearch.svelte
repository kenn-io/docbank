<script lang="ts">
  import { recordMiss } from "./generated/docbank.js";
  import { reportFailure, reportSearch } from "./action-events.js";
  import { untrack } from "svelte";
  import { SearchInput, SelectDropdown } from "@kenn-io/kit-ui";
  import type { Photos } from "./photos.svelte.js";
  import { searchSyntaxOptions } from "./searchSyntax.js";
  import { photoSearchSort } from "./photoSearch.js";

  let { photos }: { photos: Photos } = $props();
  let search = $state(untrack(() => photos.query.text));
  let syntax = $state(untrack(() => photos.query.syntax));
  const disabled = $derived(photos.blocking || photos.hiding || photos.trashing);
  const applyDraft = $derived(Boolean(search.trim()) || search !== photos.query.text);
  const appliedText = $derived(photos.query.text);
  const appliedSyntax = $derived(photos.query.syntax);
  $effect(() => {
    search = appliedText;
  });
  $effect(() => {
    syntax = appliedSyntax;
  });
  function submit(explicit = false) {
    const text = search.trim();
    const session = photos.session;
    const submitted = {
      ...photos.query,
      text: search,
      syntax,
      sort: photoSearchSort({ text: search, syntax }),
    };
    const pending = photos.setQuery(
      submitted,
      explicit
        ? (cause) => {
            if (text) reportFailure(photos.session, "search", cause);
          }
        : undefined,
    );
    void pending?.then((outcome) => {
      if (!explicit || !text || !outcome) return;
      if (outcome.accepted) {
        reportSearch(session, "text", outcome.empty);
        if (outcome.empty)
          void recordMiss(
            {
              query: text,
              requested_mode: submitted.mode,
              actual_mode: "lexical",
              filters: { ...submitted.filters, syntax: submitted.syntax },
              truncated: false,
            },
            { session, signal: AbortSignal.timeout(1000) },
          ).catch(() => {});
        return;
      }
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
      if (photos.query.text && !submit()) search = photos.query.text;
    }}
  />
</form>
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
