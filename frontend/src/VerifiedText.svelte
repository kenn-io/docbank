<script lang="ts">
  import { tick, untrack, type Snippet } from "svelte";
  import { Button, FindBar, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import AppTooltip from "./AppTooltip.svelte";
  import ShieldCheckIcon from "@lucide/svelte/icons/shield-check";
  import { APIError } from "./api-transport.js";
  import { previewEligibility, readVerifiedPreview } from "./download.js";
  import type { HighlightTerm } from "./query.js";
  import {
    resolveRenditionText,
    readVerifiedRenditionText,
    type RenditionObservation,
    type RenditionTextResolution,
  } from "./renditionText.js";
  import type { SelectedSource } from "./selectedSource.js";
  import { markText, nextTextMatch } from "./textMarks.js";

  type HighlightChoice = { id: string; name: string; terms: HighlightTerm[] };
  let {
    session,
    source,
    authorizationRevision,
    active = true,
    processingRevision = 0,
    profileName = "",
    observed,
    searchContext = false,
    fallback,
    alternative,
    queryTerms = [],
    queryHighlightError = "",
    highlightSets = [],
    onprocess,
    onauthfailure,
  }: {
    session: string;
    source: SelectedSource;
    authorizationRevision: number;
    active?: boolean;
    processingRevision?: number;
    profileName?: string;
    observed?: RenditionObservation;
    searchContext?: boolean;
    fallback?: Snippet;
    alternative?: Snippet;
    queryTerms?: string[];
    queryHighlightError?: string;
    highlightSets?: HighlightChoice[];
    onprocess?: () => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();

  let resolution = $state<RenditionTextResolution | null>(null);
  let text = $state("");
  let loading = $state(false);
  let error = $state("");
  let originalFallback = $state(false);
  let find = $state("");
  let highlightSetID = $state("");
  let activeMatch = $state(0);
  let textBox: HTMLElement | undefined = $state();
  const loadKey = $derived(
    [
      session,
      source.key,
      authorizationRevision,
      profileName,
      observed?.configuration ?? "",
      observed?.profileFingerprint ?? "",
      observed?.generationID ?? "",
      observed?.coverageState ?? "",
      observed?.attachmentID ?? "",
      observed?.buildID ?? "",
    ].join(":"),
  );
  const highlightTerms = $derived(
    highlightSets.find((item) => item.id === highlightSetID)?.terms ?? [],
  );
  const marked = $derived(markText(text, find ? { find } : { highlightTerms, queryTerms }));

  const findMatches = $derived(find ? marked.matches : []);
  const refreshKey = $derived(`${loadKey}:${active}:${processingRevision}`);
  let completedKey = "";
  let completedResolution = "";

  $effect(() => {
    const key = loadKey;
    void refreshKey;
    const inputs = untrack(() => ({
      active,
      session,
      source,
      authorizationRevision,
      profileName,
      observed,
    }));
    if (!inputs.active) return;
    const controller = new AbortController();
    let current = true;
    if (key !== completedKey) {
      resolution = null;
      text = "";
      originalFallback = false;
      loading = true;
      activeMatch = 0;
    }
    error = "";
    void resolveRenditionText(
      inputs.session,
      inputs.source,
      inputs.authorizationRevision,
      inputs.profileName,
      inputs.observed,
      controller.signal,
    )
      .then(async (next) => {
        if (!current) return;
        const fingerprint = JSON.stringify(next);
        if (key === completedKey && fingerprint === completedResolution) return;
        completedKey = "";
        loading = true;
        text = "";
        originalFallback = false;
        activeMatch = 0;
        resolution = next;
        if (next.state === "ready") {
          const received = await readVerifiedRenditionText(
            inputs.session,
            inputs.source,
            inputs.authorizationRevision,
            next,
            controller.signal,
          );
          if (current) text = received;
        } else if (next.state !== "verified_empty") {
          let eligibility;
          try {
            eligibility = previewEligibility(inputs.source.mimeType, inputs.source.size);
          } catch {
            eligibility = undefined;
          }
          if (eligibility?.kind === "text") {
            const fallback = await readVerifiedPreview(
              inputs.session,
              inputs.source,
              inputs.authorizationRevision,
              controller.signal,
              () => undefined,
            );
            if (fallback.kind !== "text") URL.revokeObjectURL(fallback.url);
            else if (current) {
              originalFallback = true;
              text = fallback.text;
            }
          }
        }
        if (current) {
          completedKey = key;
          completedResolution = fingerprint;
        }
      })
      .catch((cause: unknown) => {
        if (!current || (cause instanceof DOMException && cause.name === "AbortError")) return;
        if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
        else error = cause instanceof Error ? cause.message : String(cause);
      })
      .finally(() => {
        if (current) loading = false;
      });
    return () => {
      current = false;
      controller.abort();
    };
  });

  const searchMatches = $derived(searchContext && !find ? marked.matches : []);
  // A new query or Find input starts at the first match, even if text loads later.
  $effect(() => {
    void marked;
    activeMatch = 0;
  });
  $effect(() => {
    const index = activeMatch;
    const matches = find ? findMatches : searchMatches;
    const enabled = active;
    const box = textBox;
    void marked;
    let current = true;
    void tick().then(() => {
      if (!current || !enabled || !matches.length || !box) return;
      const match = box.querySelector<HTMLElement>(`mark[data-match-index="${index}"]`);
      if (!match) return;
      // Keep scrolling inside the text; do not move the results or the inspector.
      const top =
        box.scrollTop +
        match.getBoundingClientRect().top -
        box.getBoundingClientRect().top -
        (box.clientHeight - match.offsetHeight) / 2;
      box.scrollTo?.({ top: Math.max(0, top) });
    });
    return () => {
      current = false;
    };
  });
  function moveMatch(delta: -1 | 1): void {
    const count = find ? findMatches.length : searchMatches.length;
    activeMatch = nextTextMatch(activeMatch, delta, count);
  }

  function stateMessage(state: Exclude<RenditionTextResolution["state"], "ready">): string {
    if (state === "verified_empty")
      return "Processing verified that this document has no readable text.";
    if (state === "failed") return "Text processing failed for this selected version.";
    if (state === "unprocessed" || state === "unconfigured") return "";
    if (state === "profile_required")
      return "Choose a processing profile before reading rendition text.";
    return "The rendition recorded for this historical selection is no longer available.";
  }
</script>

<div class="verified-text">
  {#if text || resolution?.state === "verified_empty"}
    <div class="text-controls">
      {#if text}
        <FindBar
          query={find}
          matchCount={findMatches.length}
          currentIndex={Math.min(activeMatch, Math.max(0, findMatches.length - 1))}
          autofocus={false}
          placeholder="Find"
          inputAriaLabel="Find in verified text"
          previousLabel="Previous text match"
          nextLabel="Next text match"
          closeLabel="Clear text search"
          oninput={(value) => {
            find = value.slice(0, 256);
            activeMatch = 0;
          }}
          onnext={() => moveMatch(1)}
          onprev={() => moveMatch(-1)}
          onclose={() => {
            find = "";
            activeMatch = 0;
          }}
        />
        {#if highlightSets.length}
          <SelectDropdown
            value={highlightSetID}
            title="Saved highlight set"
            options={[
              { value: "", label: "Highlights" },
              ...highlightSets.map((item) => ({ value: item.id, label: item.name })),
            ]}
            onchange={(value) => {
              highlightSetID = value;
              activeMatch = 0;
            }}
          />
        {/if}
      {/if}
      <span class="verified"
        ><AppTooltip text="Matches the stored SHA-256" focusable
          ><ShieldCheckIcon size={14} aria-label="Verified" /></AppTooltip
        ></span
      >
      {#if text && alternative}{@render alternative()}{/if}
    </div>
  {/if}
  {#if queryHighlightError}<p class="notice" role="status">
      Query highlights unavailable: {queryHighlightError}
    </p>{/if}
  {#if loading}
    <div class="loading" role="status"><Spinner size={14} /> Loading text…</div>
  {:else if error}
    <p class="notice" role="alert">{error}</p>
  {:else if text}
    {#if searchContext}
      <p class="text-origin">{originalFallback ? "Original text" : "Extracted text"}</p>
      {#if !find && searchMatches.length}
        <nav class="search-matches" aria-label="Search matches">
          <Button
            size="sm"
            surface="soft"
            onclick={() => moveMatch(-1)}
            ariaLabel="Previous search match">Previous match</Button
          >
          <span aria-live="polite">Search match {activeMatch + 1} of {searchMatches.length}</span>
          <Button
            size="sm"
            surface="soft"
            onclick={() => moveMatch(1)}
            ariaLabel="Next search match">Next match</Button
          >
        </nav>
      {:else if !find && !queryHighlightError}
        <p class="notice" role="status">
          {queryTerms.length
            ? "No literal search matches in this text. The result may match metadata or use different word forms."
            : "No literal query highlights are available for this search. Read the context below."}
        </p>
      {/if}
    {/if}
    {#if originalFallback && resolution && ["failed", "historical_unavailable"].includes(resolution.state)}
      <p class="notice" role="status">
        {stateMessage(resolution.state as Exclude<RenditionTextResolution["state"], "ready">)}
      </p>
    {/if}
    <pre
      bind:this={textBox}
      aria-label={`Verified text of ${source.name}`}>{#each marked.segments as segment}{#if segment.match}<mark
            class:active={(!!find || searchContext) && segment.match.index === activeMatch}
            data-match-index={segment.match.index}
            data-source={segment.match.source}
            style={`--mark-color:${segment.match.color}`}>{segment.text}</mark
          >{:else}{segment.text}{/if}{/each}</pre>
    {#if marked.truncated}<p class="notice" role="status">
        Showing the first 5,000 non-overlapping matches.
      </p>{/if}
  {:else if resolution?.state === "unconfigured"}
    <p class="setup">
      <span>Text extraction isn't set up.</span>
      <a href="https://docbank.ai/docs/usage/document-processing/" target="_blank" rel="noreferrer"
        >Set up</a
      >
    </p>
  {:else if resolution?.state === "unprocessed" || resolution?.state === "profile_required"}
    {#if resolution.state === "profile_required"}<p class="setup">
        {stateMessage(resolution.state)}
      </p>{/if}
    {#if onprocess}<div>
        <Button size="sm" surface="soft" onclick={onprocess}>Process…</Button>
      </div>{:else if resolution.state === "unprocessed"}<p class="setup">
        This version hasn't been processed.
      </p>{/if}
  {:else if resolution && resolution.state !== "ready"}
    <p class="notice" role="status">{stateMessage(resolution.state)}</p>
  {/if}
  {#if !loading && !text && fallback}{@render fallback()}{/if}
</div>

<style>
  .verified-text {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    height: 100%;
    min-height: 0;
  }
  .text-origin {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .search-matches {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--font-size-xs);
  }
  .text-controls {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    flex-wrap: wrap;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .verified {
    display: flex;
    margin-left: auto;
    color: var(--accent-green);
  }
  .setup {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .setup a {
    margin-left: var(--space-1);
  }
  .loading {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-height: 96px;
  }
  pre {
    flex: 1;
    min-height: 100px;
    max-height: 420px;
    overflow: auto;
    margin: 0;
    padding: var(--space-5);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-primary);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
    line-height: 1.55;
    overflow-wrap: anywhere;
    white-space: pre-wrap;
  }
  mark {
    border-radius: 2px;
    background: color-mix(in srgb, var(--mark-color) 65%, transparent);
    color: inherit;
  }
  mark.active {
    outline: 2px solid var(--mark-color);
    outline-offset: 1px;
  }
  .notice {
    margin: 0;
    padding: var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }
</style>
