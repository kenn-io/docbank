<script lang="ts">
  import { onMount } from "svelte";
  import { Button, TextInput, Spinner } from "@kenn-io/kit-ui";
  import { getPhotoHiddenState, setupPhotoHidden, unlockPhotoHidden, lockPhotoHidden, changePhotoHidden, disablePhotoHidden, type PhotoHiddenState } from "./generated/docbank.js";
  import { Photos, notifyPhotoPrivacy, photoPrivacyEvent } from "./photos.svelte.js";
  import { PhotoPreviewCache } from "./photoPreviewCache.js";
  import PhotosWorkspace from "./PhotosWorkspace.svelte";
  import { APIError } from "./api-transport.js";

  let { session, onauthfailure }: { session: string; onauthfailure: (cause: unknown) => void } = $props();
  let hiddenState = $state<PhotoHiddenState>({ configured: false });
  let workspace = $state<{ photos: Photos; cache: PhotoPreviewCache }>();
  let passcode = $state("");
  let nextPasscode = $state("");
  let error = $state("");
  let busy = $state(true);
  let remaining = $state(0);
  let refreshController = new AbortController();
  let disposed = false;
  const options = () => ({ session, signal: AbortSignal.any([refreshController.signal, AbortSignal.timeout(30_000)]) });

  function clear() {
    workspace?.photos.clearForPrivacy();
    workspace?.photos.dispose();
    void workspace?.cache.dispose();
    workspace = undefined;
    remaining = 0;
  }
  function authorizationLost(cause: unknown) {
    clear();
    error = cause instanceof Error ? cause.message : String(cause);
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
      hiddenState = result;
      remaining = result.expires_at ? Math.max(0, Math.ceil((Date.parse(result.expires_at) - Date.now()) / 1000)) : 0;
      if (remaining) workspace = { photos: new Photos(session, authorizationLost, true), cache: new PhotoPreviewCache(session, authorizationLost) };
    } catch (cause) { if (!controller.signal.aborted) error = cause instanceof Error ? cause.message : String(cause); }
    finally { if (!controller.signal.aborted) busy = false; }
  }
  async function action(kind: "enter" | "lock" | "change" | "disable") {
    busy = true;
    error = "";
    if (kind === "lock" || kind === "disable" || kind === "change") clear();
    try {
      if (kind === "enter") {
        if (!hiddenState.configured) await setupPhotoHidden({ passcode }, options());
        await unlockPhotoHidden({ passcode }, options());
      } else if (kind === "lock") await lockPhotoHidden({}, options());
      else if (kind === "change") await changePhotoHidden({ passcode, new_passcode: nextPasscode }, options());
      else await disablePhotoHidden({ passcode }, options());
      passcode = "";
      nextPasscode = "";
      notifyPhotoPrivacy();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
      await refresh();
    } finally { busy = false; }
  }

  onMount(() => {
    void refresh();
    const privacy = (event: Event) => { error = (event as CustomEvent<string>).detail ?? ""; void refresh(); };
    const foreground = () => { if (document.visibilityState === "visible") void refresh(); };
    window.addEventListener(photoPrivacyEvent, privacy);
    document.addEventListener("visibilitychange", foreground);
    const timer = setInterval(() => {
      if (!hiddenState.expires_at) return;
      remaining = Math.max(0, Math.ceil((Date.parse(hiddenState.expires_at) - Date.now()) / 1000));
      if (!remaining) { clear(); hiddenState.expires_at = undefined; notifyPhotoPrivacy(); }
    }, 250);
    return () => { disposed = true; refreshController.abort(); clear(); clearInterval(timer); window.removeEventListener(photoPrivacyEvent, privacy); document.removeEventListener("visibilitychange", foreground); passcode = ""; nextPasscode = ""; };
  });
</script>

<section class="hidden-photos" aria-label="Hidden photos">
  <div class="hidden-boundary">Hidden photos stay out of Photos. Documents and document tools can still read the underlying files.</div>
  {#if error}<p role="alert">{error}</p>{/if}
  {#if workspace && remaining}
    <div class="hidden-controls"><span>Locks in {Math.floor(remaining / 60)}:{String(remaining % 60).padStart(2, "0")}</span><Button size="sm" disabled={busy} onclick={() => void action("lock")}>Lock</Button></div>
    {#key workspace}<PhotosWorkspace photos={workspace.photos} cache={workspace.cache} title="Hidden" />{/key}
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
