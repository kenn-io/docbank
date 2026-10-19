<script lang="ts">
  import { onMount } from "svelte";
  import { Button, FormField, Modal } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { HiddenAccess, passcodeValidation } from "./hiddenAccess.svelte.js";

  let { access }: { access: HiddenAccess } = $props();
  let passcode = $state("");
  let passcodeError = $state("");
  let error = $state("");
  let busy = $state(false);
  const focusPasscode = () => document.getElementById("hidden-setup-passcode")?.focus();
  onMount(focusPasscode);

  function cancel() {
    if (!busy) access.cancelSetup();
  }

  async function save() {
    if (busy) return;
    passcodeError = passcodeValidation(passcode, "Enter a passcode.");
    if (passcodeError) return focusPasscode();
    error = "";
    busy = true;
    const prompt = access.setupPrompt;
    try {
      await access.createPasscode(passcode);
      passcode = "";
      prompt?.done("saved");
    } catch (cause) {
      if (prompt?.signal.aborted || (cause instanceof APIError && cause.status === 401)) {
        prompt?.done("cancelled");
        return;
      }
      if (cause instanceof APIError && cause.code === "hidden_configured") prompt?.done("exists");
      else if (cause instanceof APIError && cause.code === "invalid_hidden_passcode")
        passcodeError = cause.message;
      else error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      busy = false;
    }
  }
</script>

<Modal title="Set a Hidden passcode" onclose={cancel} closable={!busy} closeOnOverlayClick={!busy}>
  <form
    onsubmit={(event) => {
      event.preventDefault();
      void save();
    }}
  >
    <p>Hidden photos leave Library. Unlock Hidden with this passcode to view them.</p>
    <FormField
      type="password"
      field={{
        id: "hidden-setup-passcode",
        label: "Passcode",
        value: passcode,
        error: passcodeError,
        disabled: busy,
      }}
      autocomplete="new-password"
      oninput={(value) => {
        passcode = value;
        passcodeError = "";
      }}
    />
  </form>
  {#if error}<p role="alert">{error}</p>{/if}
  {#snippet footer()}
    <Button disabled={busy} onclick={cancel}>Cancel</Button>
    <Button tone="info" disabled={busy} onclick={() => void save()}
      >{busy ? "Saving…" : "Set passcode and hide"}</Button
    >
  {/snippet}
</Modal>

<style>
  form {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
</style>
