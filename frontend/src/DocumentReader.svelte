<script lang="ts">
  import { tick } from "svelte";
  import { Button, Modal } from "@kenn-io/kit-ui";
  import AppTooltip from "./AppTooltip.svelte";
  import DownloadButton from "./DownloadButton.svelte";
  import OriginalPreview from "./OriginalPreview.svelte";
  import RedactionDrawer from "./RedactionDrawer.svelte";
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
  }: {
    session: string;
    source: SelectedSource;
    authorizationRevision: number;
    node?: Node;
    actionsDisabled?: boolean;
    onauthfailure: (cause: unknown) => void;
    onclose: () => void;
  } = $props();
  let redacting = $state(false);
  let review = $state<ReturnType<typeof RedactionDrawer>>();
  let reviewTrigger: HTMLElement | undefined;
  const redactionAction = $derived(pdfRedactionAction(source, node, actionsDisabled));
  const redactionDisabled = $derived(redactionAction?.disabledReason ?? "");
</script>

<div class="reader-shell" inert={redacting}>
  <Modal
    title={source.name}
    ariaLabel={`Read ${source.name}`}
    width="calc(100vw - 32px)"
    maxWidth="none"
    maxHeight="calc(100svh - 32px)"
    onclose={() => {
      if (redacting) review?.requestClose();
      else onclose();
    }}
  >
    <div class="reader-content">
      <OriginalPreview {session} {source} {authorizationRevision} {onauthfailure} expanded />
    </div>
    {#snippet footer()}
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
    {/snippet}
  </Modal>
</div>

{#if redacting}
  <RedactionDrawer
    bind:this={review}
    {session}
    {source}
    {authorizationRevision}
    {onauthfailure}
    onclose={() => {
      redacting = false;
      // Restore focus after the reader is no longer inert.
      void tick().then(() => reviewTrigger?.focus());
    }}
  />
{/if}

<style>
  .reader-shell :global(.kit-modal-panel) {
    height: calc(100svh - 32px);
  }
  .reader-shell :global(.kit-modal-body) {
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
</style>
