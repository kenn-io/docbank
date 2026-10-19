<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Modal, escapeCloses } from "@kenn-io/kit-ui";

  let request = $state<{
    message: string;
    label: string;
    detail?: string;
    resolve: (confirmed: boolean) => void;
  }>();
  export function confirm(message: string, label: string, detail?: string): Promise<boolean> {
    return new Promise((resolve) => (request = { message, label, detail, resolve }));
  }
  function answer(confirmed: boolean) {
    request?.resolve(confirmed);
    request = undefined;
  }
  onDestroy(() => answer(false));
</script>

{#if request}
  {@const pending = request}
  <div role="presentation" onkeydown={escapeCloses(() => answer(false))}>
    <Modal title={pending.message} ariaLabel={pending.message} onclose={() => answer(false)}>
      {#if pending.detail}<p>{pending.detail}</p>{/if}
      {#snippet footer()}
        <Button onclick={() => answer(false)}>Cancel</Button>
        <Button tone="danger" surface="solid" onclick={() => answer(true)}>{pending.label}</Button>
      {/snippet}
    </Modal>
  </div>
{/if}
