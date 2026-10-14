import { randomUUID } from "./crypto.js";

interface PhotoNavigateEvent extends Event {
  destination: { url: string; key: string };
}
interface PhotoNavigation extends EventTarget {
  currentEntry: { key: string } | null;
  traverseTo(key: string): { finished: Promise<unknown> };
}

export function photoNavigationGuard(shouldBlock: (url: URL) => boolean, browser: Window = window) {
  const navigation = (browser as unknown as { navigation?: PhotoNavigation }).navigation;
  const markerKey = "docbankPhotoHistory";
  const initial = browser.history.state?.[markerKey];
  const validInitial = typeof initial?.owner === "string" && initial.owner.length > 0 && Number.isSafeInteger(initial.position);
  const owner = validInitial ? initial.owner as string : randomUUID();
  const ownedPosition = () => {
    const marker = browser.history.state?.[markerKey];
    return marker?.owner === owner && Number.isSafeInteger(marker.position) ? marker.position as number : undefined;
  };
  const markedState = (state: Record<string, unknown> | null, position: number | undefined) => ({ ...state, [markerKey]: position === undefined ? undefined : { owner, position } });
  if (!navigation && !validInitial) browser.history.replaceState(markedState(browser.history.state, 0), "", browser.location.href);
  let restoring = false;
  const current = () => ({ url: browser.location.href, key: navigation?.currentEntry?.key, position: ownedPosition() });
  let accepted = current();
  function accept() {
    accepted = current();
  }
  function settled() {
    if (restoring && browser.location.href === accepted.url && (navigation ? navigation.currentEntry?.key === accepted.key : ownedPosition() === accepted.position)) restoring = false;
  }
  function navigate(event: Event) {
    const next = event as PhotoNavigateEvent;
    if (next.destination.key === accepted.key && next.destination.url === accepted.url) return;
    if (event.cancelable && shouldBlock(new URL(next.destination.url))) event.preventDefault();
  }
  function currentEntryChanged() {
    settled();
    if (browser.location.href === accepted.url) accept();
  }
  return {
    accept,
    push(state: Record<string, unknown> | null, url: string) {
      const position = ownedPosition();
      browser.history.pushState(navigation ? state : markedState(state, position === undefined ? undefined : position + 1), "", url);
      accept();
    },
    replace(state: Record<string, unknown> | null, url: string) {
      browser.history.replaceState(navigation ? state : { ...state, [markerKey]: browser.history.state?.[markerKey] }, "", url);
      accept();
    },
    listen() {
      accept();
      navigation?.addEventListener("navigate", navigate);
      navigation?.addEventListener("currententrychange", currentEntryChanged);
      browser.addEventListener("popstate", settled);
      browser.addEventListener("hashchange", settled);
      return () => {
        navigation?.removeEventListener("navigate", navigate);
        navigation?.removeEventListener("currententrychange", currentEntryChanged);
        browser.removeEventListener("popstate", settled);
        browser.removeEventListener("hashchange", settled);
      };
    },
    blocked() {
      if (restoring) return true;
      if (!shouldBlock(new URL(browser.location.href))) return false;
      try {
        if (navigation && accepted.key) {
          restoring = true;
          void navigation.traverseTo(accepted.key).finished.catch(() => { restoring = false; });
        } else if (accepted.position !== undefined && ownedPosition() !== undefined) {
          const delta = accepted.position - ownedPosition()!;
          if (delta) { restoring = true; browser.history.go(delta); }
        } else if (!navigation && browser.history.state?.[markerKey] === undefined) {
          // Typed hashes and fragment links add an unmarked entry right after the accepted one; nothing owned leads back, so restore it in place.
          // The restored entry is a copy, not the accepted entry, so it carries none of that entry's app state.
          browser.history.replaceState(markedState(null, accepted.position === undefined ? undefined : accepted.position + 1), "", accepted.url);
          accept();
        }
      } catch { restoring = false; }
      return true;
    },
  };
}
