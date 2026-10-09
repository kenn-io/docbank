<script lang="ts">
  import { onMount, tick } from "svelte";
  import { Button, FormField, Spinner } from "@kenn-io/kit-ui";
  import { getPhotoHiddenState, setupPhotoHidden, unlockPhotoHidden, lockPhotoHidden, changePhotoHidden, disablePhotoHidden, type PhotoHiddenState } from "./generated/docbank.js";
  import { Photos } from "./photos.svelte.js";
  import { PhotoPreviewCache } from "./photoPreviewCache.js";
  import PhotosWorkspace from "./PhotosWorkspace.svelte";
  import { APIError } from "./api-transport.js";

  let { session, onauthfailure, photoActionError = "", ontrashed, onunhidden, onactionerror }: { session: string; onauthfailure: (cause: unknown) => void; photoActionError?: string; ontrashed?: () => void; onunhidden?: () => void; onactionerror?: (error: string) => void } = $props();
  let hiddenState = $state<PhotoHiddenState>({ configured: false });
  let workspace = $state<{ photos: Photos; cache: PhotoPreviewCache }>();
  let photoWorkspace = $state<{ refresh: () => Promise<void> }>();
  export async function refresh() { await photoWorkspace?.refresh(); }
  let passcode = $state("");
  let currentPasscode = $state("");
  let nextPasscode = $state("");
  let passcodeError = $state("");
  let currentPasscodeError = $state("");
  let nextPasscodeError = $state("");
  let resolved = $state(false);
  let readError = $state("");
  let actionError = $state("");
  let lockoutError = $state("");
  let reading = $state(true);
  let actionPending = $state(false);
  let concealingAction = $state(false);
  const busy = $derived(actionPending);
  const lockoutDescription = $derived(hiddenState.locked_until || lockoutError ? "hidden-lockout" : undefined);
  let remaining = $state(0);
  let refreshController = new AbortController();
  const actionController = new AbortController();
  let disposed = false;
  const options = (signal = refreshController.signal) => ({ session, signal: AbortSignal.any([signal, AbortSignal.timeout(signal === refreshController.signal ? 2000 : 30_000)]) });

  function clear() {
    workspace?.photos.clearHidden();
    workspace?.photos.dispose();
    void workspace?.cache.dispose();
    workspace = undefined;
    remaining = 0;
  }
  function authorizationLost(cause: unknown) {
    clear();
    readError = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
    else void checkAccess();
  }
  function applyState(result: PhotoHiddenState) {
    if (workspace && (result.configured !== hiddenState.configured || result.expires_at !== hiddenState.expires_at)) clear();
    readError = "";
    lockoutError = "";
    hiddenState = result;
    resolved = true;
    reading = false;
    remaining = result.expires_at ? Math.max(0, Math.ceil((Date.parse(result.expires_at) - Date.now()) / 1000)) : 0;
    if (!remaining || concealingAction) clear();
    else if (!workspace) workspace = { photos: new Photos(session, authorizationLost, true), cache: new PhotoPreviewCache(session, authorizationLost, true) };
  }
  async function checkAccess() {
    refreshController.abort();
    refreshController = new AbortController();
    const controller = refreshController;
    reading = true;
    try {
      const result = await getPhotoHiddenState(options());
      if (controller.signal.aborted || disposed) return;
      applyState(result);
    } catch (cause) { if (!controller.signal.aborted && !disposed) { if (cause instanceof APIError && cause.status === 401) onauthfailure(cause); else readError = cause instanceof Error ? cause.message : String(cause); clear(); hiddenState.expires_at = undefined; } }
    finally { if (!controller.signal.aborted) reading = false; }
  }
  function passcodeValidation(value: string, empty: string) {
    return !value ? empty : new TextEncoder().encode(value).length > 1024 ? "Use 1 to 1,024 bytes." : "";
  }
  async function action(kind: "enter" | "lock" | "change" | "disable") {
    if (actionPending) return;
    actionError = "";
    let invalidField: string | undefined;
    if (kind === "enter") {
      passcodeError = passcodeValidation(passcode, "Enter a passcode.");
      if (passcodeError) invalidField = "hidden-passcode";
    } else if (kind !== "lock") {
      currentPasscodeError = passcodeValidation(currentPasscode, "Enter your current passcode.");
      if (currentPasscodeError) invalidField = "hidden-current-passcode";
      if (kind === "change") {
        nextPasscodeError = passcodeValidation(nextPasscode, "Enter a new passcode.");
        if (nextPasscodeError) invalidField ??= "hidden-new-passcode";
      }
    }
    if (invalidField) { await tick(); document.getElementById(invalidField)?.focus(); return; }
    actionPending = true;
    concealingAction = kind !== "enter";
    let completed = false;
    if (kind === "lock" || kind === "disable" || kind === "change") clear();
    try {
      if (kind === "enter") {
        if (!hiddenState.configured) await setupPhotoHidden({ passcode }, options(actionController.signal));
        await unlockPhotoHidden({ passcode }, options(actionController.signal));
      } else if (kind === "lock") await lockPhotoHidden({}, options(actionController.signal));
      else if (kind === "change") await changePhotoHidden({ passcode: currentPasscode, new_passcode: nextPasscode }, options(actionController.signal));
      else await disablePhotoHidden({ passcode: currentPasscode }, options(actionController.signal));
      passcode = "";
      currentPasscode = "";
      nextPasscode = "";
      passcodeError = currentPasscodeError = nextPasscodeError = "";
      completed = true;
      lockoutError = "";
    } catch (cause) {
      if (disposed) return;
      if (cause instanceof APIError && cause.status === 401) { onauthfailure(cause); return; }
      if (cause instanceof APIError && cause.code === "hidden_passcode") {
        const message = "Incorrect passcode.";
        if (kind === "enter") { passcodeError = message; invalidField = "hidden-passcode"; }
        else { currentPasscodeError = message; invalidField = "hidden-current-passcode"; }
      } else if (cause instanceof APIError && cause.code === "hidden_lockout") lockoutError = cause.message;
      else actionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      actionPending = false;
      concealingAction = false;
      if (!disposed) { if (completed && kind === "disable") onunhidden?.(); await checkAccess(); }
      if (invalidField && !disposed) { await tick(); document.getElementById(invalidField)?.focus(); }
    }
  }

  onMount(() => {
    void checkAccess();
    const poll = setInterval(() => { if (!reading) void checkAccess(); }, 2000);
    const foreground = () => { if (document.visibilityState === "visible") void checkAccess(); };
    document.addEventListener("visibilitychange", foreground);
    const timer = setInterval(() => {
      if (hiddenState.locked_until && Date.parse(hiddenState.locked_until) <= Date.now()) hiddenState.locked_until = undefined;
      if (!hiddenState.expires_at) return;
      remaining = Math.max(0, Math.ceil((Date.parse(hiddenState.expires_at) - Date.now()) / 1000));
      if (!remaining) { clear(); hiddenState.expires_at = undefined; }
    }, 250);
    return () => { disposed = true; refreshController.abort(); actionController.abort(); clear(); clearInterval(timer); clearInterval(poll); document.removeEventListener("visibilitychange", foreground); passcode = ""; currentPasscode = ""; nextPasscode = ""; };
  });

</script>

<section class="hidden-photos" aria-label="Hidden photos">
  <div class="hidden-boundary">Hidden photos stay out of Photos. Documents and document tools can still read the underlying files.</div>
  {#if photoActionError}<p role="alert">{photoActionError}</p>{/if}
  {#if actionError}<p role="alert">{actionError}</p>{/if}
  {#if readError}<p role="alert">{readError}</p><Button size="sm" onclick={() => void checkAccess()}>Retry</Button>{/if}
  {#if !resolved}
    <div class="hidden-gate"><h1>Hidden</h1>{#if reading}<Spinner />{/if}</div>
  {:else if workspace && remaining}
    <div class="hidden-controls"><span>Locks in {Math.floor(remaining / 60)}:{String(remaining % 60).padStart(2, "0")}</span><Button size="sm" disabled={actionPending} onclick={() => void action("lock")}>Lock</Button></div>
    {#each [workspace] as current (current)}<PhotosWorkspace bind:this={photoWorkspace} photos={current.photos} cache={current.cache} title="Hidden" {ontrashed} onhidden={onunhidden} {onactionerror} />{/each}
  {:else}
    <div class="hidden-gate">
      <h1>Hidden</h1>
      {#if hiddenState.configured}<p>Unlocks for five minutes.</p>{:else}<p>Forgotten passcode? Run <code>docbank photos hidden reset</code>.</p>{/if}
      {#if lockoutDescription}<p id="hidden-lockout" class="lockout" role="status">{hiddenState.locked_until ? `Too many attempts. Try again after ${new Date(hiddenState.locked_until).toLocaleTimeString()}.` : lockoutError}</p>{/if}
      <form aria-describedby={lockoutDescription} onsubmit={event => { event.preventDefault(); void action("enter"); }}>
        <FormField type="password" field={{ id: "hidden-passcode", label: "Passcode", value: passcode, error: passcodeError, disabled: busy }} autocomplete={hiddenState.configured ? "current-password" : "new-password"} oninput={value => { passcode = value; passcodeError = ""; }} />
        <Button type="submit" disabled={busy} ariaDescribedby={lockoutDescription}>{hiddenState.configured ? "Unlock" : "Set passcode"}</Button>
      </form>
      {#if busy}<Spinner />{/if}
      {#if hiddenState.configured}
        <details><summary>Manage passcode</summary>
          <form aria-describedby={lockoutDescription} onsubmit={event => { event.preventDefault(); void action("change"); }}>
            <FormField type="password" field={{ id: "hidden-current-passcode", label: "Current passcode", value: currentPasscode, error: currentPasscodeError, disabled: busy }} autocomplete="current-password" oninput={value => { currentPasscode = value; currentPasscodeError = ""; }} />
            <FormField type="password" field={{ id: "hidden-new-passcode", label: "New passcode", value: nextPasscode, error: nextPasscodeError, disabled: busy }} autocomplete="new-password" oninput={value => { nextPasscode = value; nextPasscodeError = ""; }} />
            <div class="manage-actions"><Button type="submit" disabled={busy} ariaDescribedby={lockoutDescription}>Change passcode</Button><Button tone="danger" disabled={busy} ariaDescribedby={lockoutDescription} onclick={() => void action("disable")}>Disable Hidden</Button></div>
            <p class="disable-consequence">Disabling Hidden returns every hidden photo to Library.</p>
          </form>
        </details>
      {/if}
    </div>
  {/if}
</section>

<style>
  .hidden-photos { display: flex; flex-direction: column; flex: 1; min-height: 0; overflow: hidden; }
  .hidden-boundary, .hidden-controls { padding: var(--space-3) var(--space-5); font-size: var(--font-size-sm); color: var(--text-muted); border-bottom: 1px solid var(--border-default); }
  .hidden-controls { display: flex; align-items: center; justify-content: space-between; }
  .hidden-gate { width: 100%; max-width: 440px; box-sizing: border-box; padding: var(--space-5); overflow-y: auto; }
  form { display: flex; flex-direction: column; gap: var(--space-3); align-items: flex-start; }
  details { margin-top: var(--space-5); }
  summary { cursor: pointer; border-radius: var(--radius-sm); }
  summary:focus-visible { outline: 2px solid var(--accent-blue); outline-offset: 4px; }
  details form { margin-top: var(--space-3); }
  .manage-actions { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .disable-consequence, .lockout { margin: 0; color: var(--text-muted); font-size: var(--font-size-sm); }
  [role="alert"] { padding: var(--space-3) var(--space-5); color: var(--text-primary); }
</style>
