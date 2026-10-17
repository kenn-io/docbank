<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Modal } from "@kenn-io/kit-ui";

  let request = $state<{ message: string; label: string; resolve: (confirmed: boolean) => void }>();
  export function confirm(message: string, label: string): Promise<boolean> {
    return new Promise((resolve) => (request = { message, label, resolve }));
  }
  function answer(confirmed: boolean) {
    request?.resolve(confirmed);
    request = undefined;
  }
  onDestroy(() => answer(false));
</script>

{#if request}
  {@const pending = request}
  <Modal title={pending.message} ariaLabel={pending.message} onclose={() => answer(false)}>
    {#snippet footer()}
      <Button onclick={() => answer(false)}>Cancel</Button>
      <Button tone="danger" surface="solid" onclick={() => answer(true)}>{pending.label}</Button>
    {/snippet}
  </Modal>
{/if}
