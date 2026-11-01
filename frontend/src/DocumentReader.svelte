<script lang="ts">
  import { tick } from "svelte";
  import { Button, Modal } from "@kenn-io/kit-ui";
  import AppTooltip from "./AppTooltip.svelte";
  import DownloadButton from "./DownloadButton.svelte";
  import OriginalPreview from "./OriginalPreview.svelte";
  import RedactionDrawer from "./RedactionDrawer.svelte";
  import { parseMediaType } from "./query.js";
  import type { SelectedSource } from "./selectedSource.js";
  let {
    session,
    source,
    authorizationRevision,
    redactionAvailable = false,
    onauthfailure,
    onclose,
  }: {
    session: string;
    source: SelectedSource;
    authorizationRevision: number;
    redactionAvailable?: boolean;
    onauthfailure: (cause: unknown) => void;
    onclose: () => void;
  } = $props();
  let redacting = $state(false);
  let review = $state<ReturnType<typeof RedactionDrawer>>();
  let reviewTrigger: HTMLElement | undefined;
  const canRedact = $derived(
    redactionAvailable &&
      source.kind === "live" &&
      parseMediaType(source.mimeType) === "application/pdf",
  );
  const redactionDisabled = $derived(
    source.size === 0
      ? "An empty PDF cannot be redacted"
      : source.size > 64 * 1024 * 1024
        ? "PDF redaction supports files up to 64 MiB"
        : "",
  );
</script>

<div inert={redacting}>
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
      {#if canRedact}
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
  .reader-content {
    height: calc(100svh - 190px);
    overflow: auto;
    min-height: 200px;
  }
</style>
