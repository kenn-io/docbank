<script lang="ts">
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import type { BatesExport } from "./generated/docbank.js";
  import type { NumberingInputs } from "./redactionNumbering.js";
  import type { RecipientPackageController } from "./recipientPackageController.svelte.js";
  let {
    controller,
    inputs,
    artifact,
  }: { controller: RecipientPackageController; inputs: NumberingInputs; artifact: BatesExport } =
    $props();
  const active = $derived(controller.allocation === artifact.allocation_id);
</script>

<Button
  disabled={controller.phase !== "idle"}
  onclick={() => void controller.start(inputs, artifact)}>Download recipient ZIP</Button
>
{#if active}
  {#if controller.phase === "building"}
    <p role="status"><Spinner size={16} /> Checking PDF and preparing ZIP…</p>
    <Button onclick={() => void controller.cancel()}>Cancel package</Button>
  {:else if controller.phase === "releasing"}
    <p role="status"><Spinner size={16} /> Releasing package…</p>
  {:else if controller.phase === "cleanup-failed"}
    <Button onclick={() => void controller.release()}>Release package</Button>
  {/if}
  {#if controller.error}<p role="alert">{controller.error}</p>{/if}
{/if}

<style>
  p {
    font-size: 0.875rem;
  }
  [role="status"] {
    display: flex;
    align-items: center;
    gap: 0.4rem;
  }
  [role="alert"] {
    color: var(--status-error-text);
  }
</style>
