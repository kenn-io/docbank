<script lang="ts">
  import { reportAction, reportFailure } from "./action-events.js";
  import "./inspector.css";
  import { tick } from "svelte";
  import { Button } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import AttachmentsTab from "./AttachmentsTab.svelte";
  import EmailViewer from "./EmailViewer.svelte";
  import { readRelatedSource, type AttachmentIdentity } from "./attachments.js";
  import DownloadButton from "./DownloadButton.svelte";
  import InspectorDetails from "./InspectorDetails.svelte";
  import DuplicatesTab from "./DuplicatesTab.svelte";
  import OriginalPreview from "./OriginalPreview.svelte";
  import PageViewer from "./PageViewer.svelte";
  import { parseMediaType, type HighlightTerm } from "./query.js";
  import type { RenditionObservation } from "./renditionText.js";
  import type {
    LiveSelectedSource,
    RelatedSelectedSource,
    SelectedSource,
  } from "./selectedSource.js";
  import TabStrip from "./TabStrip.svelte";
  import VerifiedText from "./VerifiedText.svelte";

  type HighlightChoice = { id: string; name: string; terms: HighlightTerm[] };
  let {
    session,
    source,
    authorizationRevision,
    processingRevision = 0,
    profileName = "",
    observed,
    activeTab = $bindable("preview"),
    queryTerms = [],
    queryHighlightError = "",
    highlightSets = [],
    snapshotPosition,
    snapshotTotal,
    canPrevious = false,
    canNext = false,
    snapshotExpired = false,
    onnavigate = () => undefined,
    onreturnfocus = () => undefined,
    onprocess,
    onauthfailure,
    ondisplay,
  }: {
    session: string;
    source: SelectedSource;
    authorizationRevision: number;
    processingRevision?: number;
    profileName?: string;
    activeTab?: "preview" | "text" | "duplicates" | "attachments" | "email";
    observed?: RenditionObservation;
    queryTerms?: string[];
    queryHighlightError?: string;
    highlightSets?: HighlightChoice[];
    snapshotPosition?: number;
    snapshotTotal?: number;
    canPrevious?: boolean;
    canNext?: boolean;
    snapshotExpired?: boolean;
    onnavigate?: (direction: "previous" | "next") => void | Promise<void>;
    onreturnfocus?: () => void;
    onprocess?: (source: SelectedSource) => void;
    onauthfailure: (cause: unknown) => void;
    ondisplay?: (source: SelectedSource, revision: number) => void;
  } = $props();
  let visited = $state<{ key: string; tabs: string[] }>({ key: "", tabs: [] });
  let duplicateSource = $state<LiveSelectedSource | undefined>();
  let relatedSources = $state<RelatedSelectedSource[]>([]);
  let navigationPending = $state(false);
  let navigationError = $state("");
  let navigationEpoch = 0;
  let navigationController: AbortController | undefined;
  let frozenKey = "";
  const relatedSource = $derived(relatedSources.at(-1));
  const displayedSource = $derived(relatedSource ?? duplicateSource ?? source);
  const displayedObservation = $derived(duplicateSource || relatedSource ? undefined : observed);
  const isEmail = $derived(parseMediaType(displayedSource.mimeType) === "message/rfc822");
  // An email has its own tab; its Preview would only say it cannot render.
  const showPreview = $derived(!isEmail);
  const fallbackTab = $derived(showPreview ? "preview" : "email");
  const displayedTab = $derived(
    activeTab === "email" && !isEmail
      ? fallbackTab
      : activeTab === "preview" && !showPreview
        ? fallbackTab
        : activeTab,
  );
  const tabList = $derived([
    ...(showPreview ? [{ value: "preview", label: "Preview" }] : []),
    { value: "text", label: "Text" },
    { value: "duplicates", label: "Duplicates" },
    { value: "attachments", label: "Attachments" },
    ...(isEmail ? [{ value: "email", label: "Email" }] : []),
  ]);
  const displayedRevision = $derived(
    relatedSource?.mutationRevision ?? duplicateSource?.mutationRevision ?? authorizationRevision,
  );
  const contentKey = $derived(
    `${session}:${displayedSource.key}:${displayedRevision}:${processingRevision}`,
  );
  const visitedTabs = $derived(visited.key === contentKey ? visited.tabs : []);

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

  $effect(() => {
    ondisplay?.(displayedSource, displayedRevision);
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
    visited = { key: contentKey, tabs: [...new Set([...visitedTabs, displayedTab])] };
    activeTab = tab;
  }
  async function openRelated(target: AttachmentIdentity): Promise<void> {
    cancelNavigation();
    const pending = new AbortController();
    navigationController = pending;
    const request = navigationEpoch;
    navigationPending = true;
    navigationError = "";
    try {
      if (relatedSources.length >= 32)
        throw new Error(
          "Navigation depth limit reached. Return to an earlier document to continue.",
        );
      const next = await readRelatedSource(session, target, pending.signal);
      if (request !== navigationEpoch || pending.signal.aborted) return;
      relatedSources = [...relatedSources, next];
      reportAction(session, "document_opened");
      activeTab = parseMediaType(next.mimeType) === "message/rfc822" ? "attachments" : "preview";
    } catch (cause) {
      if (request !== navigationEpoch || pending.signal.aborted) return;
      reportFailure(session, "open", cause);
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      else navigationError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === navigationEpoch) navigationPending = false;
    }
  }
  function backRelated(): void {
    cancelNavigation();
    relatedSources = relatedSources.slice(0, -1);
    navigationError = "";
    activeTab = "attachments";
  }
  async function returnToOrigin(): Promise<void> {
    cancelNavigation();
    relatedSources = [];
    duplicateSource = undefined;
    navigationError = "";
    activeTab = "attachments";
    await tick();
    onreturnfocus();
  }

  function openDuplicate(next: LiveSelectedSource): void {
    cancelNavigation();
    relatedSources = [];
    duplicateSource = next;
    reportAction(session, "document_opened");
    activeTab = "text";
  }

  function closeDuplicate(): void {
    cancelNavigation();
    duplicateSource = undefined;
    activeTab = "duplicates";
  }
</script>

<section
  class="verified-preview"
  aria-label={`Verified content of ${displayedSource.name}`}
  data-source-version={displayedSource.versionID}
>
  {#if !ondisplay}<div class="preview-actions">
      <DownloadButton
        {session}
        source={displayedSource}
        authorizationRevision={displayedRevision}
        {onauthfailure}
      />
    </div>{/if}
  {#if relatedSource}
    <div class="separate-context related-context" role="status">
      <strong>Attachment</strong>
      <InspectorDetails
        label="Preview file"
        facts={[
          { label: "Path", value: relatedSource.path, copy: true },
          { label: "Version", value: relatedSource.versionID, copy: true },
          { label: "SHA-256", value: relatedSource.blobHash, copy: true },
        ]}
      />
      <div class="related-actions">
        <Button size="sm" surface="soft" onclick={backRelated}>Back to previous document</Button>
        <Button size="sm" surface="soft" onclick={() => void returnToOrigin()}
          >Return to selected document</Button
        >
      </div>
    </div>
  {:else if duplicateSource}
    <div class="separate-context" role="status">
      <strong>Duplicate</strong>
      <InspectorDetails
        label="Preview file"
        facts={[
          { label: "Path", value: duplicateSource.path, copy: true },
          { label: "Version", value: duplicateSource.versionID, copy: true },
          { label: "SHA-256", value: duplicateSource.blobHash, copy: true },
        ]}
      />
      <Button size="sm" surface="soft" onclick={closeDuplicate}>Return to selected document</Button>
    </div>
  {/if}
  {#if !duplicateSource && !relatedSource && snapshotPosition !== undefined && snapshotTotal !== undefined}
    <nav class="document-navigation" aria-label="Document navigation">
      <Button
        size="sm"
        surface="soft"
        disabled={!canPrevious || snapshotExpired}
        onclick={() => void onnavigate("previous")}>Previous</Button
      >
      <span aria-live="polite">Document {snapshotPosition + 1} of {snapshotTotal}</span>
      <Button
        size="sm"
        surface="soft"
        disabled={!canNext || snapshotExpired}
        onclick={() => void onnavigate("next")}>Next</Button
      >
    </nav>
    {#if snapshotExpired}<p class="expired" role="status">
        These results expired. Run the query again to continue.
      </p>{/if}
  {/if}
  <TabStrip
    tabs={tabList}
    value={displayedTab}
    ariaLabel="Verified content view"
    onchange={(tab) => chooseTab(tab as typeof activeTab)}
  />
  {#if navigationPending}<p role="status">Loading document…</p>{/if}
  {#if navigationError}<p role="status">Related document unavailable: {navigationError}</p>{/if}
  <div class="tab-content inspector-content">
    {#key contentKey}
      {#each tabList as tab (tab.value)}
        {#if tab.value === displayedTab || (tab.value !== "text" && visitedTabs.includes(tab.value))}
          <div hidden={tab.value !== displayedTab}>
            {#if tab.value === "preview"}
              {#if displayedSource.mimeType === "application/pdf" || displayedSource.mimeType === "image/png"}
                <PageViewer
                  {session}
                  source={displayedSource}
                  authorizationRevision={displayedRevision}
                  {onauthfailure}
                />
              {:else}
                <OriginalPreview
                  {session}
                  source={displayedSource}
                  authorizationRevision={displayedRevision}
                  {onauthfailure}
                />
              {/if}
            {:else}
              {#if tab.value === "email"}
                {#key `${session}:${displayedSource.key}:${displayedRevision}`}
                  <EmailViewer
                    {session}
                    source={displayedSource}
                    authorizationRevision={displayedRevision}
                    onattachments={() => chooseTab("attachments")}
                    {onauthfailure}
                  />
                {/key}
              {:else if tab.value === "text"}
                <VerifiedText
                  {session}
                  source={displayedSource}
                  authorizationRevision={displayedRevision}
                  {profileName}
                  observed={displayedObservation}
                  {queryTerms}
                  {queryHighlightError}
                  {highlightSets}
                  onprocess={onprocess ? () => onprocess?.(displayedSource) : undefined}
                  {onauthfailure}
                />
              {:else if tab.value === "attachments"}
                <AttachmentsTab
                  {session}
                  source={displayedSource}
                  onopen={(target) => void openRelated(target)}
                  {onauthfailure}
                />
              {:else}
                <DuplicatesTab
                  {session}
                  source={displayedSource}
                  onopen={openDuplicate}
                  {onauthfailure}
                />
              {/if}
            {/if}
          </div>
        {/if}
      {/each}
    {/key}
  </div>
</section>

<style>
  .verified-preview {
    display: grid;
    gap: var(--space-5);
    min-width: 0;
  }
  .preview-actions {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }
  .tab-content :global(.original-preview pre),
  .tab-content :global(.original-preview .image-frame),
  .tab-content :global(.verified-text pre) {
    border: 0;
    padding: 0;
    border-radius: 0;
  }
  .tab-content :global(.original-preview pre) {
    padding-right: calc(14px + var(--space-3));
  }
  .tab-content :global(.original-preview .verified) {
    top: 2px;
    right: 0;
  }
  .document-navigation {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .expired {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .related-context {
    flex-direction: column;
    align-items: stretch;
    overflow-wrap: anywhere;
  }
  .related-actions {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .separate-context {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-2);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
