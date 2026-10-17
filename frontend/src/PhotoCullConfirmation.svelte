<script lang="ts">
  import { onDestroy } from "svelte";
  import { Button, Modal } from "@kenn-io/kit-ui";

  let request = $state<{ message: string; resolve: (confirmed: boolean) => void }>();
  export function confirm(message: string): Promise<boolean> {
    return new Promise((resolve) => (request = { message, resolve }));
  }
  function answer(confirmed: boolean) {
    request?.resolve(confirmed);
    request = undefined;
  }
  onDestroy(() => answer(false));
</script>

{#if request}
  <Modal title={request.message} ariaLabel={request.message} onclose={() => answer(false)}>
    {#snippet footer()}
      <Button onclick={() => answer(false)}>Cancel</Button>
      <Button tone="danger" surface="solid" onclick={() => answer(true)}>Replace</Button>
    {/snippet}
  </Modal>
{/if}
