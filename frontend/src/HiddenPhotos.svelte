<script lang="ts">
  import { onMount, tick } from "svelte";
  import { Button, FormField, Spinner } from "@kenn-io/kit-ui";
  import { getPhotoHiddenState, setupPhotoHidden, unlockPhotoHidden, changePhotoHidden, disablePhotoHidden, type PhotoHiddenState } from "./generated/docbank.js";
  import { Photos } from "./photos.svelte.js";
  import { PhotoPreviewCache } from "./photoPreviewCache.js";
  import { concealPhotoWorkspace, revalidatePhotoWorkspace, type PhotoWorkspace } from "./photoWorkspace.js";
  import PhotosWorkspace from "./PhotosWorkspace.svelte";
  import { APIError } from "./api-transport.js";
  import { HiddenAccess } from "./hiddenAccess.svelte.js";

  let { photoID = "", onphotochange = () => {}, onconceal, access, photoActionError = "", ontrashed, onunhidden, onactionerror, onstate, onaccessfailure }: { photoID?: string; onphotochange?: (id: string, mode: "open" | "step" | "close") => void; onconceal?: () => void; access: HiddenAccess; photoActionError?: string; ontrashed?: () => void; onunhidden?: () => void; onactionerror?: (error: string) => void; onstate?: (state: PhotoHiddenState) => void; onaccessfailure?: () => void } = $props();
  let hiddenState = $state<PhotoHiddenState>({ configured: false });
  let workspace = $state<PhotoWorkspace>();
  let photoWorkspace = $state<PhotosWorkspace>();
  export function activePhotos() { return workspace?.photos; }
  export function captureReturn() { photoWorkspace?.captureReturn(); }
  export async function refresh() { if (workspaceVisible) await photoWorkspace?.refresh(); else await checkAccess(); }
  export function lock() { return action("lock"); }
  let workspaceVisible = $state(false);
  let concealment = 0;
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
  let remaining = $state(0);
  // Deadlines come from server-relative seconds so a skewed browser clock cannot distort them.
  let deadline = 0;
  let lockDeadline = $state(0);
  const secondsUntil = (at: number) => Math.max(0, Math.ceil((at - Date.now()) / 1000));
  const lockoutDescription = $derived(lockDeadline || lockoutError ? "hidden-lockout" : undefined);
  let refreshController = new AbortController();
  let disposed = false;
  const options = () => ({ session: access.session, signal: AbortSignal.timeout(30_000) });

  function conceal() {
    if (workspaceVisible && !disposed) onconceal?.();
    workspaceVisible = false;
    concealment++;
    return concealPhotoWorkspace(workspace);
  }
  function clear() {
    void conceal();
    workspace?.photos.dispose();
    workspace = undefined;
    remaining = 0;
    deadline = 0;
  }
  function authorizationLost(cause: unknown) {
    clear();
    readError = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) access.onauthfailure(cause);
    else void checkAccess();
  }
  async function applyState(result: PhotoHiddenState, signal: AbortSignal, requestedAt: number) {
    onstate?.(result);
    if (workspace && (result.configured !== hiddenState.configured || result.expires_at !== hiddenState.expires_at)) clear();
    else if (workspace && result.change_id !== hiddenState.change_id) void conceal();
    readError = "";
    lockoutError = "";
    hiddenState = result;
    resolved = true;
    lockDeadline = result.locked_for ? requestedAt + result.locked_for * 1000 : 0;
    deadline = result.expires_in ? requestedAt + result.expires_in * 1000 : 0;
    remaining = secondsUntil(deadline);
    if (!remaining) access.lockError = "";
    if (!remaining || concealingAction || access.lockError) clear();
    else if (!workspace) { workspace = { photos: new Photos(access.session, authorizationLost, true), cache: new PhotoPreviewCache(access.session, authorizationLost, true) }; workspaceVisible = true; }
    else if (!workspaceVisible && !workspace.photos.hiding && !workspace.photos.trashing && !workspace.photos.loading) {
      const current = workspace;
      const version = concealment;
      if (!await revalidatePhotoWorkspace(current, access.session, authorizationLost, () => version === concealment && !signal.aborted && !disposed)) return;
      workspaceVisible = true;
    }
  }
  async function checkAccess() {
    if (document.visibilityState !== "visible") return;
    refreshController.abort();
    refreshController = new AbortController();
    const controller = refreshController;
    reading = true;
    try {
      await access.settled();
      if (controller.signal.aborted || disposed) return;
      // Anchoring at send time can only end access early, never late.
      const requestedAt = Date.now();
      const result = await getPhotoHiddenState({ session: access.session, signal: AbortSignal.any([controller.signal, AbortSignal.timeout(2000)]) });
      if (controller.signal.aborted || disposed) return;
      await applyState(result, controller.signal, requestedAt);
    } catch (cause) {
      if (!controller.signal.aborted && !disposed) {
        readError = cause instanceof Error ? cause.message : String(cause);
        onaccessfailure?.();
        if (cause instanceof APIError && (cause.status === 401 || cause.status === 403)) { clear(); hiddenState.expires_at = undefined; if (cause.status === 401) access.onauthfailure(cause); }
        else void conceal();
      }
    }
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
    const submitted = { passcode, currentPasscode, nextPasscode, configured: hiddenState.configured };
    actionPending = true;
    concealingAction = kind !== "enter";
    let disableDispatched = false;
    let succeeded = false;
    if (kind === "lock" || kind === "disable" || kind === "change") clear();
    try {
      if (kind === "lock") succeeded = await access.lock();
      else await access.run(async () => {
        if (kind === "enter") {
          if (!submitted.configured) await setupPhotoHidden({ passcode: submitted.passcode }, options());
          await unlockPhotoHidden({ passcode: submitted.passcode }, options());
        } else if (kind === "change") await changePhotoHidden({ passcode: submitted.currentPasscode, new_passcode: submitted.nextPasscode }, options());
        else { disableDispatched = true; await disablePhotoHidden({ passcode: submitted.currentPasscode }, options()); }
      });
      passcode = "";
      currentPasscode = "";
      nextPasscode = "";
      passcodeError = currentPasscodeError = nextPasscodeError = "";
      lockoutError = "";
      if (kind !== "lock") succeeded = true;
    } catch (cause) {
      if (disposed) return;
      if (cause instanceof APIError && cause.status === 401) { access.onauthfailure(cause); return; }
      if (cause instanceof APIError && cause.code === "hidden_passcode") {
        const message = "Incorrect passcode.";
        if (kind === "enter") { passcodeError = message; invalidField = "hidden-passcode"; }
        else { currentPasscodeError = message; invalidField = "hidden-current-passcode"; }
      } else if (cause instanceof APIError && cause.code === "hidden_lockout") lockoutError = cause.message;
      else actionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      actionPending = false;
      concealingAction = false;
      if (disableDispatched) onunhidden?.();
      if (!disposed) await checkAccess();
      // Unlock, lock and disable replace the focused control; move focus to its successor.
      const successor = invalidField ?? (!succeeded ? undefined : kind === "enter" ? "hidden-controls" : kind === "change" ? undefined : "hidden-passcode");
      if (successor && !disposed) { await tick(); document.getElementById(successor)?.focus(); }
    }
  }

  onMount(() => {
    void checkAccess();
    const poll = setInterval(() => { if (!reading) void checkAccess(); }, 2000);
    const foreground = () => { if (document.visibilityState === "visible") void checkAccess(); else { refreshController.abort(); void conceal(); } };
    document.addEventListener("visibilitychange", foreground);
    const timer = setInterval(() => {
      if (lockDeadline && !secondsUntil(lockDeadline)) { lockDeadline = 0; hiddenState.locked_until = undefined; }
      if (!deadline) return;
      remaining = secondsUntil(deadline);
      if (!remaining) { clear(); hiddenState.expires_at = undefined; }
    }, 250);
    return () => { disposed = true; refreshController.abort(); clear(); clearInterval(timer); clearInterval(poll); document.removeEventListener("visibilitychange", foreground); passcode = ""; currentPasscode = ""; nextPasscode = ""; };
  });

</script>

<section class="hidden-photos" aria-label="Hidden photos">
  <div class="hidden-boundary">Hidden photos stay out of Photos. Documents and document tools can still read the underlying files.</div>
  {#if photoActionError}<p role="alert">{photoActionError}</p>{/if}
  {#if actionError}<p role="alert">{actionError}</p>{/if}
  {#if readError}<p role="alert">{readError}</p><Button size="sm" onclick={() => void checkAccess()}>Retry</Button>{/if}
  {#if access.lockError}
    <div class="hidden-gate"><h1>Hidden</h1></div>
  {:else if !resolved}
    <div class="hidden-gate"><h1>Hidden</h1>{#if reading}<Spinner />{/if}</div>
  {:else if workspace && remaining && workspaceVisible}
    <div id="hidden-controls" class="hidden-controls" tabindex="-1"><span>Locks in {Math.floor(remaining / 60)}:{String(remaining % 60).padStart(2, "0")}</span><Button size="sm" disabled={actionPending} onclick={() => void action("lock")}>Lock</Button></div>
    {#each [workspace] as current (current)}<PhotosWorkspace bind:this={photoWorkspace} photos={current.photos} cache={current.cache} title="Hidden" {photoID} {onphotochange} {ontrashed} onhidden={onunhidden} oninvalidate={conceal} {onactionerror} />{/each}
  {:else if workspace && remaining}
    <Spinner />
  {:else}
    <div class="hidden-gate">
      <h1>Hidden</h1>
      {#if hiddenState.configured}<p>Forgotten passcode? Run <code>docbank photos hidden reset</code>.</p>{:else}<p>Unlocks for five minutes.</p>{/if}
      {#if lockoutDescription}<p id="hidden-lockout" class="lockout" role="status">{lockDeadline ? `Too many attempts. Try again after ${new Date(lockDeadline).toLocaleTimeString()}.` : lockoutError}</p>{/if}
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
