<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import DownloadButton from "./DownloadButton.svelte";
	import DuplicatesTab from "./DuplicatesTab.svelte";
  import OriginalPreview from "./OriginalPreview.svelte";
  import type { HighlightTerm } from "./query.js";
  import type { RenditionObservation } from "./renditionText.js";
	import { closeDuplicateSource, inspectorSource, openDuplicateSource,
		type LiveSelectedSource, type SelectedSource } from "./selectedSource.js";
  import VerifiedText from "./VerifiedText.svelte";

  type HighlightChoice = { id: string; name: string; terms: HighlightTerm[] };
  let { session, source, authorizationRevision, profileName = "", observed, activeTab = $bindable("preview"),
    queryTerms = [], queryHighlightError = "", highlightSets = [], snapshotPosition,
    snapshotTotal, canPrevious = false, canNext = false, snapshotExpired = false,
    onnavigate = () => undefined, onauthfailure }: {
    session: string; source: SelectedSource; authorizationRevision: number; profileName?: string;
		activeTab?: "preview" | "text" | "duplicates";
    observed?: RenditionObservation; queryTerms?: string[]; queryHighlightError?: string;
    highlightSets?: HighlightChoice[]; snapshotPosition?: number; snapshotTotal?: number;
    canPrevious?: boolean; canNext?: boolean; snapshotExpired?: boolean;
    onnavigate?: (direction: "previous" | "next") => void | Promise<void>;
    onauthfailure: (cause: unknown) => void;
  } = $props();
	let duplicateSource = $state<LiveSelectedSource | undefined>();
	let frozenKey = "";
	const displayedSource = $derived(inspectorSource({ frozen: source, ...(duplicateSource ? { duplicate: duplicateSource } : {}) }));
	const displayedRevision = $derived(duplicateSource?.mutationRevision ?? authorizationRevision);

	$effect(() => {
		const key = source.key;
		if (key !== frozenKey) {
			frozenKey = key;
			duplicateSource = undefined;
		}
	});

	function openDuplicate(next: LiveSelectedSource): void {
		duplicateSource = openDuplicateSource({ frozen: source }, next).duplicate as LiveSelectedSource;
		activeTab = "text";
	}

	function closeDuplicate(): void {
		duplicateSource = closeDuplicateSource({ frozen: source, duplicate: duplicateSource }).duplicate as undefined;
		activeTab = "duplicates";
	}
</script>

<section class="verified-preview" aria-label={`Verified content of ${displayedSource.name}`}>
	{#if duplicateSource}
		<div class="separate-context" role="status"><span>Outside duplicate · {duplicateSource.path}</span>
			<Button size="sm" surface="soft" onclick={closeDuplicate}>Return to {source.kind === "snapshot" ? "frozen" : "selected"} document</Button></div>
	{/if}
  {#if !duplicateSource && snapshotPosition !== undefined && snapshotTotal !== undefined}
    <nav class="document-navigation" aria-label="Snapshot document navigation">
      <Button size="sm" surface="soft" disabled={!canPrevious || snapshotExpired}
        onclick={() => void onnavigate("previous")}>Previous</Button>
      <span aria-live="polite">Document {snapshotPosition + 1} of {snapshotTotal}</span>
      <Button size="sm" surface="soft" disabled={!canNext || snapshotExpired}
        onclick={() => void onnavigate("next")}>Next</Button>
    </nav>
    {#if snapshotExpired}<p class="expired" role="status">This frozen snapshot expired. Run the query again to continue navigation.</p>{/if}
  {/if}
  <div class="tabs" role="tablist" aria-label="Verified content view">
    <button type="button" role="tab" aria-selected={activeTab === "preview"}
      onclick={() => (activeTab = "preview")}>Preview</button>
    <button type="button" role="tab" aria-selected={activeTab === "text"}
      onclick={() => (activeTab = "text")}>Text</button>
		<button type="button" role="tab" aria-selected={activeTab === "duplicates"}
			onclick={() => (activeTab = "duplicates")}>Duplicates</button>
  </div>
  {#if activeTab === "preview"}
		<OriginalPreview {session} source={displayedSource} authorizationRevision={displayedRevision} {onauthfailure} />
  {:else}
		{#if activeTab === "text"}
			<VerifiedText {session} source={displayedSource} authorizationRevision={displayedRevision} {profileName}
				observed={duplicateSource ? undefined : observed} {queryTerms} {queryHighlightError} {highlightSets} {onauthfailure} />
		{:else}
			<DuplicatesTab {session} source={displayedSource} onopen={openDuplicate} {onauthfailure} />
		{/if}
  {/if}
	<DownloadButton {session} source={displayedSource} authorizationRevision={displayedRevision}
		label="Download verified original" {onauthfailure} />
</section>

<style>
  .verified-preview { display: grid; gap: var(--space-3); padding-top: var(--space-3); border-top: 1px solid var(--border-default); }
  .tabs { display: flex; gap: var(--space-1); border-bottom: 1px solid var(--border-default); }
  .tabs button { padding: var(--space-2) var(--space-3); border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--text-muted); cursor: pointer; font: inherit; font-size: var(--font-size-sm); }
  .tabs button[aria-selected="true"] { border-bottom-color: var(--color-info); color: var(--text-primary); }
  .document-navigation { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); color: var(--text-muted); font-size: var(--font-size-xs); }
  .expired { margin: 0; color: var(--text-muted); font-size: var(--font-size-xs); }
	.separate-context { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-inset); color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
