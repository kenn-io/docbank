<script lang="ts">
  import { onMount } from "svelte";
  import { Button, TextInput, Spinner } from "@kenn-io/kit-ui";
  import { getPhotoHiddenState, setupPhotoHidden, unlockPhotoHidden, lockPhotoHidden, changePhotoHidden, disablePhotoHidden, type PhotoHiddenState } from "./generated/docbank.js";
  import { Photos, notifyPhotoPrivacy, photoPrivacyEvent, photoRevalidationErrorEvent } from "./photos.svelte.js";
  import { PhotoPreviewCache } from "./photoPreviewCache.js";
  import PhotosWorkspace from "./PhotosWorkspace.svelte";
  import { APIError } from "./api-transport.js";

  let { session, onauthfailure }: { session: string; onauthfailure: (cause: unknown) => void } = $props();
  let hiddenState = $state<PhotoHiddenState>({ change_id: "", configured: false });
  let workspace = $state<{ photos: Photos; cache: PhotoPreviewCache }>();
  let passcode = $state("");
  let nextPasscode = $state("");
  let readError = $state("");
  let actionError = $state("");
  let reading = $state(true);
  let actionPending = $state(false);
  let concealingAction = $state(false);
  const busy = $derived(reading || actionPending);
  let remaining = $state(0);
  let refreshController = new AbortController();
  const actionController = new AbortController();
  let disposed = false;
  const options = (signal = refreshController.signal) => ({ session, signal: AbortSignal.any([signal, AbortSignal.timeout(30_000)]) });

  function clear() {
    workspace?.photos.clearForPrivacy();
    workspace?.photos.dispose();
    void workspace?.cache.dispose();
    workspace = undefined;
    remaining = 0;
  }
  function authorizationLost(cause: unknown) {
    clear();
    readError = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
    else void refresh();
  }
  async function refresh() {
    refreshController.abort();
    refreshController = new AbortController();
    const controller = refreshController;
    clear();
    hiddenState.expires_at = undefined;
    try {
      const result = await getPhotoHiddenState(options());
      if (controller.signal.aborted || disposed) return;
      readError = "";
      hiddenState = result;
      remaining = result.expires_at ? Math.max(0, Math.ceil((Date.parse(result.expires_at) - Date.now()) / 1000)) : 0;
      if (remaining && !concealingAction) workspace = { photos: new Photos(session, authorizationLost, true), cache: new PhotoPreviewCache(session, authorizationLost) };
    } catch (cause) { if (!controller.signal.aborted && !disposed) { if (cause instanceof APIError && cause.status === 401) onauthfailure(cause); else readError = cause instanceof Error ? cause.message : String(cause); } }
    finally { if (!controller.signal.aborted) reading = false; }
  }
  async function action(kind: "enter" | "lock" | "change" | "disable") {
    if (actionPending) return;
    actionPending = true;
    concealingAction = kind !== "enter";
    let completed = false;
    actionError = "";
    if (kind === "lock" || kind === "disable" || kind === "change") clear();
    try {
      if (kind === "enter") {
        if (!hiddenState.configured) await setupPhotoHidden({ passcode }, options(actionController.signal));
        await unlockPhotoHidden({ passcode }, options(actionController.signal));
      } else if (kind === "lock") await lockPhotoHidden({}, options(actionController.signal));
      else if (kind === "change") await changePhotoHidden({ passcode, new_passcode: nextPasscode }, options(actionController.signal));
      else await disablePhotoHidden({ passcode }, options(actionController.signal));
      passcode = "";
      nextPasscode = "";
      completed = true;
    } catch (cause) {
      if (disposed) return;
      if (cause instanceof APIError && cause.status === 401) { onauthfailure(cause); return; }
      actionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      actionPending = false;
      concealingAction = false;
      if (!disposed) { if (completed) notifyPhotoPrivacy(); else await refresh(); }
    }
  }

  onMount(() => {
    void refresh();
    const privacy = (event: Event) => {
      const detail = (event as CustomEvent<PhotoHiddenState | string | undefined>).detail;
      if (workspace && detail && typeof detail === "object" && detail.change_id === hiddenState.change_id && detail.configured === hiddenState.configured && detail.expires_at === hiddenState.expires_at) return;
      if (typeof detail === "string") actionError = detail;
      void refresh();
    };
    const failed = (event: Event) => { refreshController.abort(); clear(); hiddenState.expires_at = undefined; reading = false; readError = (event as CustomEvent<string>).detail; };
    window.addEventListener(photoPrivacyEvent, privacy);
    window.addEventListener(photoRevalidationErrorEvent, failed);
    const timer = setInterval(() => {
      if (!hiddenState.expires_at) return;
      remaining = Math.max(0, Math.ceil((Date.parse(hiddenState.expires_at) - Date.now()) / 1000));
      if (!remaining) { clear(); hiddenState.expires_at = undefined; notifyPhotoPrivacy(); }
    }, 250);
    return () => { disposed = true; refreshController.abort(); actionController.abort(); clear(); clearInterval(timer); window.removeEventListener(photoPrivacyEvent, privacy); window.removeEventListener(photoRevalidationErrorEvent, failed); passcode = ""; nextPasscode = ""; };
  });
</script>

<section class="hidden-photos" aria-label="Hidden photos">
  <div class="hidden-boundary">Hidden photos stay out of Photos. Documents and document tools can still read the underlying files.</div>
  {#if actionError}<p role="alert">{actionError}</p>{/if}
  {#if readError}<p role="alert">{readError}</p><Button size="sm" onclick={() => void refresh()}>Retry</Button>{/if}
  {#if workspace && remaining}
    <div class="hidden-controls"><span>Locks in {Math.floor(remaining / 60)}:{String(remaining % 60).padStart(2, "0")}</span><Button size="sm" disabled={busy} onclick={() => void action("lock")}>Lock</Button></div>
    {#each [workspace] as current (current)}<PhotosWorkspace photos={current.photos} cache={current.cache} title="Hidden" />{/each}
  {:else}
    <div class="hidden-gate">
      <h1>Hidden</h1>
      <p>{hiddenState.configured ? "Enter your passcode to view hidden photos for five minutes." : "Set a passcode before hiding photos."}</p>
      {#if hiddenState.locked_until}<p>Too many attempts. Try again after {new Date(hiddenState.locked_until).toLocaleTimeString()}.</p>{/if}
      <form onsubmit={event => { event.preventDefault(); void action("enter"); }}>
        <TextInput type="password" ariaLabel="Passcode" bind:value={passcode} autocomplete="current-password" disabled={busy} />
        <Button type="submit" disabled={busy || !passcode}>{hiddenState.configured ? "Unlock" : "Set passcode"}</Button>
      </form>
      {#if busy}<Spinner />{/if}
      {#if hiddenState.configured}
        <details><summary>Manage passcode</summary>
          <p>Enter the current passcode above. Disabling Hidden returns every hidden photo to Library.</p>
          <TextInput type="password" ariaLabel="New passcode" bind:value={nextPasscode} autocomplete="new-password" disabled={busy} />
          <Button disabled={busy || !passcode || !nextPasscode} onclick={() => void action("change")}>Change passcode</Button>
          <Button disabled={busy || !passcode} onclick={() => void action("disable")}>Disable Hidden</Button>
        </details>
      {/if}
    </div>
  {/if}
</section>

<style>
  .hidden-photos { display: flex; flex-direction: column; flex: 1; min-height: 0; overflow: hidden; }
  .hidden-boundary, .hidden-controls { padding: var(--space-3) var(--space-5); font-size: var(--font-size-sm); color: var(--text-muted); border-bottom: 1px solid var(--border-default); }
  .hidden-controls { display: flex; align-items: center; justify-content: space-between; }
  .hidden-gate { max-width: 560px; padding: var(--space-5); }
  form { display: flex; gap: var(--space-2); align-items: center; }
  details { margin-top: var(--space-5); }
  [role="alert"] { padding: var(--space-3) var(--space-5); color: var(--text-primary); }
</style>
