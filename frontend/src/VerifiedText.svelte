<script lang="ts">
  import { tick, untrack } from "svelte";
  import { Chip, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "./api.js";
  import { previewEligibility, readVerifiedPreview } from "./download.js";
  import type { HighlightTerm } from "./query.js";
  import { resolveRenditionText, readVerifiedRenditionText,
    type RenditionObservation, type RenditionTextResolution } from "./renditionText.js";
  import type { SelectedSource } from "./selectedSource.js";
  import { markText } from "./textMarks.js";

  type HighlightChoice = { id: string; name: string; terms: HighlightTerm[] };
  let { session, source, authorizationRevision, profileName = "", observed, queryTerms = [],
    queryHighlightError = "", highlightSets = [], onauthfailure }: {
    session: string; source: SelectedSource; authorizationRevision: number; profileName?: string;
    observed?: RenditionObservation; queryTerms?: string[]; queryHighlightError?: string;
    highlightSets?: HighlightChoice[]; onauthfailure: (cause: unknown) => void;
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
	const loadKey = $derived([session, source.key, authorizationRevision, profileName,
		observed?.configuration ?? "", observed?.profileFingerprint ?? "", observed?.generationID ?? "",
		observed?.coverageState ?? "", observed?.attachmentID ?? "", observed?.buildID ?? ""].join(":"));
  const highlightTerms = $derived(highlightSets.find((item) => item.id === highlightSetID)?.terms ?? []);
  const marked = $derived(markText(text, { find, highlightTerms, queryTerms }));

  $effect(() => {
		const key = loadKey;
		const inputs = untrack(() => ({ session, source, authorizationRevision, profileName, observed }));
    const controller = new AbortController();
    let current = true;
    void key;
    resolution = null;
    text = "";
    error = "";
    originalFallback = false;
    loading = true;
    activeMatch = 0;
		void resolveRenditionText(inputs.session, inputs.source, inputs.authorizationRevision,
			inputs.profileName, inputs.observed, controller.signal)
      .then(async (next) => {
        if (!current) return;
        resolution = next;
        if (next.state === "ready") {
					const received = await readVerifiedRenditionText(inputs.session, inputs.source,
						inputs.authorizationRevision, next, controller.signal);
          if (current) text = received;
          return;
        }
        if (next.state === "verified_empty") return;
        let eligibility;
				try { eligibility = previewEligibility(inputs.source.mimeType, inputs.source.size); } catch { return; }
        if (eligibility.kind !== "text") return;
				const fallback = await readVerifiedPreview(inputs.session, inputs.source,
					inputs.authorizationRevision, controller.signal, () => undefined);
        if (!current) {
          if (fallback.kind === "image") URL.revokeObjectURL(fallback.url);
          return;
        }
        if (fallback.kind !== "text") {
          URL.revokeObjectURL(fallback.url);
          return;
        }
        originalFallback = true;
        text = fallback.text;
      }).catch((cause: unknown) => {
        if (!current || (cause instanceof DOMException && cause.name === "AbortError")) return;
        if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
        else error = cause instanceof Error ? cause.message : String(cause);
      }).finally(() => { if (current) loading = false; });
    return () => { current = false; controller.abort(); text = ""; resolution = null; error = ""; };
  });

  function moveMatch(delta: number): void {
    if (marked.matches.length === 0) return;
    activeMatch = (activeMatch + delta + marked.matches.length) % marked.matches.length;
    void tick().then(() => {
      textBox?.querySelector<HTMLElement>(`mark[data-match-index="${activeMatch}"]`)?.scrollIntoView({ block: "center" });
    });
  }

  function stateMessage(state: Exclude<RenditionTextResolution["state"], "ready">): string {
    if (state === "verified_empty") return "Processing verified that this document has no readable text.";
    if (state === "failed") return "Text processing failed for this selected version.";
    if (state === "unprocessed") return "This selected version has not been processed for text.";
    if (state === "unconfigured") return "Text processing is not configured for this vault.";
    if (state === "profile_required") return "Choose a processing profile before reading rendition text.";
    return "The rendition recorded for this historical selection is no longer available.";
  }
</script>

<div class="verified-text">
  <div class="text-heading">
    <div><strong>Verified text</strong><span>{originalFallback ? "Exact original-text fallback" : "Exact selected rendition"}</span></div>
    {#if text || resolution?.state === "verified_empty"}<Chip size="xs" tone="success" dot>SHA-256 verified</Chip>{/if}
  </div>
  <div class="text-controls">
    <label>Find <input aria-label="Find in verified text" maxlength="256" value={find}
      oninput={(event) => { find = event.currentTarget.value; activeMatch = 0; }} /></label>
    <button type="button" aria-label="Previous text match" disabled={marked.matches.length === 0} onclick={() => moveMatch(-1)}>↑</button>
    <button type="button" aria-label="Next text match" disabled={marked.matches.length === 0} onclick={() => moveMatch(1)}>↓</button>
    <span aria-live="polite">{marked.matches.length ? `${activeMatch + 1} of ${marked.matches.length}` : "No matches"}</span>
    {#if highlightSets.length}
      <label>Highlights <select aria-label="Saved highlight set" bind:value={highlightSetID}>
        <option value="">None</option>
        {#each highlightSets as item (item.id)}<option value={item.id}>{item.name}</option>{/each}
      </select></label>
    {/if}
  </div>
  {#if queryHighlightError}<p class="notice" role="status">Query highlights unavailable: {queryHighlightError}</p>{/if}
  {#if loading}
    <div class="loading" role="status"><Spinner size={14} /> Resolving verified text…</div>
  {:else if error}
    <p class="notice" role="alert">{error}</p>
  {:else if text}
		<pre bind:this={textBox} aria-label={`Verified text of ${source.name}`}>{#each marked.segments as segment}{#if segment.match}<mark
      class:active={segment.match.index === activeMatch}
      data-match-index={segment.match.index}
      data-source={segment.match.source}
      style={`--mark-color:${segment.match.color}`}>{segment.text}</mark>{:else}{segment.text}{/if}{/each}</pre>
    {#if marked.truncated}<p class="notice" role="status">Showing the first 5,000 non-overlapping matches.</p>{/if}
  {:else if resolution && resolution.state !== "ready"}
    <p class="notice" role="status">{stateMessage(resolution.state)}</p>
  {/if}
</div>

<style>
  .verified-text { display: grid; gap: var(--space-3); }
  .text-heading, .text-heading > div, .text-controls { display: flex; align-items: center; gap: var(--space-2); }
  .text-heading { justify-content: space-between; }
  .text-heading > div { align-items: baseline; }
  .text-heading strong { color: var(--text-primary); font-size: var(--font-size-sm); }
  .text-heading span, .text-controls, .loading, .notice { color: var(--text-muted); font-size: var(--font-size-xs); }
  .text-controls { flex-wrap: wrap; }
  .text-controls label { display: flex; align-items: center; gap: var(--space-1); }
  input, select, button { min-height: 28px; border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-surface); color: var(--text-primary); }
  input { width: 150px; padding: 0 var(--space-2); }
  select { max-width: 170px; }
  button { min-width: 30px; cursor: pointer; }
  button:disabled { cursor: default; opacity: .5; }
  .loading { display: flex; align-items: center; gap: var(--space-2); min-height: 96px; }
  pre { max-height: 420px; overflow: auto; margin: 0; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-inset); color: var(--text-primary); font-family: var(--font-mono); font-size: var(--font-size-xs); line-height: 1.55; overflow-wrap: anywhere; white-space: pre-wrap; }
  mark { border-radius: 2px; background: color-mix(in srgb, var(--mark-color) 65%, transparent); color: inherit; }
  mark.active { outline: 2px solid var(--mark-color); outline-offset: 1px; }
  .notice { margin: 0; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-inset); }
</style>
