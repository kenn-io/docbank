<script lang="ts">
  import { tick, untrack } from "svelte";
  import { Button, Modal } from "@kenn-io/kit-ui";
  import AppTooltip from "./AppTooltip.svelte";
  import DownloadButton from "./DownloadButton.svelte";
  import OriginalPreview from "./OriginalPreview.svelte";
  import RedactionReview from "./RedactionReview.svelte";
  import { pdfRedactionAction } from "./documentReader.js";
  import type { Node } from "./generated/docbank.js";
  import type { SelectedSource } from "./selectedSource.js";
  let {
    session,
    source,
    authorizationRevision,
    node,
    actionsDisabled = false,
    onauthfailure,
    onclose,
    startReview = false,
  }: {
    session: string;
    source: SelectedSource;
    authorizationRevision: number;
    node?: Node;
    actionsDisabled?: boolean;
    onauthfailure: (cause: unknown) => void;
    onclose: () => void;
    startReview?: boolean;
  } = $props();
  let redacting = $state(untrack(() => startReview));
  let review = $state<ReturnType<typeof RedactionReview>>();
  let reviewTrigger: HTMLElement | undefined;
  const redactionAction = $derived(pdfRedactionAction(source, node, actionsDisabled));
  const redactionDisabled = $derived(redactionAction?.disabledReason ?? "");
  const pdf = $derived(source.mimeType.split(";")[0]?.trim().toLowerCase() === "application/pdf");
</script>

<div class="reader-shell" class:reviewing={redacting}>
  <Modal
    title={redacting ? `Redact ${source.name}` : source.name}
    ariaLabel={`${redacting ? "Redact" : "Read"} ${source.name}`}
    width="calc(100vw - 32px)"
    maxWidth="none"
    maxHeight="calc(100svh - 32px)"
    onclose={() => {
      if (redacting) review?.requestClose(true);
      else onclose();
    }}
  >
    <div class="reader-content" class:pdf>
      {#if pdf}
        <RedactionReview
          bind:this={review}
          active={redacting}
          {session}
          {source}
          {authorizationRevision}
          {onauthfailure}
          onworkspaceclose={onclose}
          onclose={() => {
            redacting = false;
            void tick().then(() => reviewTrigger?.focus());
          }}
        />
      {:else}<OriginalPreview
          {session}
          {source}
          {authorizationRevision}
          {onauthfailure}
          expanded
        />{/if}
    </div>
    {#snippet footer()}
      <div class="reading-actions" hidden={redacting}>
        <DownloadButton {session} {source} {authorizationRevision} {onauthfailure} />
        {#if redactionAction}
          <AppTooltip text={redactionDisabled}>
            <Button
              disabled={!!redactionDisabled}
              onclick={(event) => {
                reviewTrigger = event.currentTarget as HTMLElement;
                redacting = true;
              }}>Redact PDF</Button
            >
          </AppTooltip>
        {/if}
      </div>
    {/snippet}
  </Modal>
</div>

<style>
  .reader-shell.reviewing > :global(.kit-modal-overlay > .kit-modal-panel > .kit-modal-footer) {
    display: none;
  }
  .reading-actions {
    display: flex;
    gap: var(--space-2);
  }
  .reading-actions[hidden] {
    display: none;
  }
  .reader-shell > :global(.kit-modal-overlay > .kit-modal-panel) {
    height: calc(100svh - 32px);
  }
  .reader-shell > :global(.kit-modal-overlay > .kit-modal-panel > .kit-modal-body) {
    display: flex;
    flex: 1;
    min-height: 0;
    overflow: hidden;
  }
  .reader-content {
    flex: 1;
    min-width: 0;
    min-height: 0;
    overflow: auto;
  }
  .reader-content.pdf {
    overflow: hidden;
  }
</style>
