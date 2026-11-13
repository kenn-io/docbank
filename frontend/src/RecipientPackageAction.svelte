<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
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
  const building = $derived(active && controller.phase === "building");
</script>

{#if active && controller.phase === "cleanup-failed"}<Button
    size="sm"
    onclick={() => void controller.release()}>Release recipient ZIP</Button
  >
{:else}<Button
    size="sm"
    busy={building || (active && controller.phase === "releasing")}
    busyLabel={building ? "Preparing recipient ZIP" : "Releasing recipient ZIP"}
    ariaLabel="Download recipient ZIP"
    label="Recipient ZIP"
    disabled={controller.phase !== "idle"}
    onclick={() => void controller.start(inputs, artifact)}
  />{/if}
{#if building}<Button
    size="sm"
    ariaLabel="Cancel recipient ZIP"
    onclick={() => void controller.cancel()}>Cancel</Button
  >{/if}
{#if active}
  {#if building}<p class="status" role="status">Checking the PDF…</p>
  {:else if controller.message}<p class="status" role="status">{controller.message}</p>{/if}
  {#if controller.error}<p class="error" role="alert">{controller.error}</p>{/if}
{/if}

<style>
  p {
    margin: 0;
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }
  .status {
    color: var(--text-muted);
  }
  .error {
    color: var(--status-error-text);
  }
</style>
