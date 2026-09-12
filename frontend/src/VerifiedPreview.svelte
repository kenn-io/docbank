<script lang="ts">
  import { tick } from "svelte";
  import { Button } from "@kenn-io/kit-ui";
  import { APIError } from "./api.js";
  import AttachmentsTab from "./AttachmentsTab.svelte";
  import { readRelatedSource, type AttachmentIdentity } from "./attachments.js";
  import DownloadButton from "./DownloadButton.svelte";
	import DuplicatesTab from "./DuplicatesTab.svelte";
  import OriginalPreview from "./OriginalPreview.svelte";
  import PageViewer from "./PageViewer.svelte";
  import type { HighlightTerm } from "./query.js";
  import type { RenditionObservation } from "./renditionText.js";
  import type { LiveSelectedSource, RelatedSelectedSource, SelectedSource } from "./selectedSource.js";
  import VerifiedText from "./VerifiedText.svelte";

  type HighlightChoice = { id: string; name: string; terms: HighlightTerm[] };
  let { session, source, authorizationRevision, profileName = "", observed, activeTab = $bindable("preview"),
    queryTerms = [], queryHighlightError = "", highlightSets = [], snapshotPosition,
    snapshotTotal, canPrevious = false, canNext = false, snapshotExpired = false,
    onnavigate = () => undefined, onreturnfocus = () => undefined, onauthfailure }: {
    session: string; source: SelectedSource; authorizationRevision: number; profileName?: string;
		activeTab?: "preview" | "text" | "duplicates" | "attachments";
    observed?: RenditionObservation; queryTerms?: string[]; queryHighlightError?: string;
    highlightSets?: HighlightChoice[]; snapshotPosition?: number; snapshotTotal?: number;
    canPrevious?: boolean; canNext?: boolean; snapshotExpired?: boolean;
    onnavigate?: (direction: "previous" | "next") => void | Promise<void>;
    onreturnfocus?: () => void;
    onauthfailure: (cause: unknown) => void;
  } = $props();
	let duplicateSource = $state<LiveSelectedSource | undefined>();
	let relatedSources = $state<RelatedSelectedSource[]>([]);
	let navigationPending = $state(false);
	let navigationError = $state("");
	let navigationEpoch = 0;
	let navigationController: AbortController | undefined;
	let frozenKey = "";
	const relatedSource = $derived(relatedSources.at(-1));
	const displayedSource = $derived(relatedSource ?? duplicateSource ?? source);
	const displayedRevision = $derived(relatedSource?.mutationRevision ?? duplicateSource?.mutationRevision ?? authorizationRevision);

	$effect(() => {
		const key = `${session}:${source.key}`;
		if (key !== frozenKey) {
			frozenKey = key;
			duplicateSource = undefined;
			relatedSources = [];
			navigationError = "";
		}
		return cancelNavigation;
	});

  function cancelNavigation(): void {
    ++navigationEpoch;
    navigationController?.abort();
    navigationController = undefined;
    navigationPending = false;
  }
  function chooseTab(tab: typeof activeTab): void {
    cancelNavigation();
    navigationError = "";
    activeTab = tab;
  }
  async function openRelated(target: AttachmentIdentity): Promise<void> {
    cancelNavigation();
    if (relatedSources.length >= 32) { navigationError = "Navigation depth limit reached. Return to an earlier document to continue."; return; }
    const pending = new AbortController(); navigationController = pending;
    const request = navigationEpoch;
    navigationPending = true; navigationError = "";
    try {
      const next = await readRelatedSource(session, target, pending.signal);
      if (request !== navigationEpoch || pending.signal.aborted) return;
      relatedSources = [...relatedSources, next];
      activeTab = next.mimeType === "message/rfc822" ? "attachments" : "preview";
    } catch (cause) {
      if (request !== navigationEpoch || pending.signal.aborted) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else navigationError = cause instanceof Error ? cause.message : String(cause);
    } finally { if (request === navigationEpoch) navigationPending = false; }
  }
  function backRelated(): void {
    cancelNavigation();
    relatedSources = relatedSources.slice(0, -1);
    navigationError = "";
    activeTab = "attachments";
  }
  async function returnToOrigin(): Promise<void> {
    cancelNavigation();
    relatedSources = []; duplicateSource = undefined; navigationError = "";
    activeTab = "attachments";
    await tick();
    onreturnfocus();
  }

	function openDuplicate(next: LiveSelectedSource): void {
		cancelNavigation();
		relatedSources = [];
		duplicateSource = next;
		activeTab = "text";
	}

	function closeDuplicate(): void {
		cancelNavigation();
		duplicateSource = undefined;
		activeTab = "duplicates";
	}
</script>

<section class="verified-preview" aria-label={`Verified content of ${displayedSource.name}`} data-source-version={displayedSource.versionID}>
  {#if relatedSource}
    <div class="separate-context related-context" role="status">
      <span>Related document · separate inspector context · current path {relatedSource.path}</span>
      <span>Exact related version {relatedSource.versionID}</span>
      <div class="related-actions">
        <Button size="sm" surface="soft" onclick={backRelated}>Back to previous document</Button>
        <Button size="sm" surface="soft" onclick={() => void returnToOrigin()}>Return to {source.kind === "snapshot" ? "frozen" : "selected"} document</Button>
      </div>
    </div>
	{:else if duplicateSource}
		<div class="separate-context" role="status"><span>Outside duplicate · {duplicateSource.path}</span>
			<Button size="sm" surface="soft" onclick={closeDuplicate}>Return to {source.kind === "snapshot" ? "frozen" : "selected"} document</Button></div>
	{/if}
  {#if !duplicateSource && !relatedSource && snapshotPosition !== undefined && snapshotTotal !== undefined}
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
      onclick={() => chooseTab("preview")}>Preview</button>
    <button type="button" role="tab" aria-selected={activeTab === "text"}
      onclick={() => chooseTab("text")}>Text</button>
		<button type="button" role="tab" aria-selected={activeTab === "duplicates"}
			onclick={() => chooseTab("duplicates")}>Duplicates</button>
    <button type="button" role="tab" aria-selected={activeTab === "attachments"}
      onclick={() => chooseTab("attachments")}>Attachments</button>
  </div>
  {#if navigationPending}<p role="status">Checking related document access and exact version…</p>{/if}
  {#if navigationError}<p role="status">Related document unavailable: {navigationError}</p>{/if}
  {#if activeTab === "preview"}
		{#if displayedSource.mimeType === "application/pdf" || displayedSource.mimeType === "image/png"}
			<PageViewer {session} source={displayedSource} authorizationRevision={displayedRevision} {onauthfailure} />
		{:else}
		<OriginalPreview {session} source={displayedSource} authorizationRevision={displayedRevision} {onauthfailure} />
		{/if}
  {:else}
		{#if activeTab === "text"}
			<VerifiedText {session} source={displayedSource} authorizationRevision={displayedRevision} {profileName}
				observed={duplicateSource || relatedSource ? undefined : observed} {queryTerms} {queryHighlightError} {highlightSets} {onauthfailure} />
		{:else if activeTab === "attachments"}
      <AttachmentsTab {session} source={displayedSource} onopen={(target) => void openRelated(target)} {onauthfailure} />
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
  .related-context { flex-direction: column; align-items: stretch; overflow-wrap: anywhere; }
  .related-actions { display: flex; gap: var(--space-2); flex-wrap: wrap; }
	.separate-context { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-inset); color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
