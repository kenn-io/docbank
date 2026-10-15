import { afterEach, expect, it, onTestFinished, vi } from "vite-plus/test";
import { waitFor } from "@testing-library/svelte";
import { photoNavigationGuard } from "./photoNavigation.js";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  history.replaceState(null, "", "/");
});

function classicGuard(sync = false) {
  vi.stubGlobal("navigation", undefined);
  history.replaceState(null, "", "/");
  let dirty = false,
    acceptedURL = location.href;
  const guard = photoNavigationGuard((url) => dirty && url.href !== acceptedURL),
    dispose = guard.listen();
  const accept = () => {
    if (!guard.blocked()) {
      acceptedURL = location.href;
      guard.accept();
    }
  };
  if (sync) {
    window.addEventListener("popstate", accept);
    window.addEventListener("hashchange", accept);
  }
  onTestFinished(() => {
    window.removeEventListener("popstate", accept);
    window.removeEventListener("hashchange", accept);
    dispose();
  });
  return {
    guard,
    setDirty(value: boolean) {
      dirty = value;
    },
    setAcceptedURL(value: string) {
      acceptedURL = value;
    },
  };
}

it("guards native target changes and restores noncancelable traversal by entry key", () => {
  const navigation = Object.assign(new EventTarget(), {
    currentEntry: { key: "first" },
    traverseTo: vi.fn((_key: string) => ({ finished: Promise.resolve() })),
  });
  vi.stubGlobal("navigation", navigation);
  history.replaceState(null, "", "/photos#photo=first");
  let dirty = true;
  const guard = photoNavigationGuard(() => dirty),
    dispose = guard.listen();
  expect(history.state).toBeNull();
  const dispatch = (url: string, key: string) => {
    const event = Object.assign(new Event("navigate", { cancelable: true }), {
      destination: { url: new URL(url, location.href).href, key },
    });
    navigation.dispatchEvent(event);
    return event;
  };
  expect(dispatch("/photos#photo=second", "second").defaultPrevented).toBe(true);
  expect(dispatch("/photos#photo=first", "first").defaultPrevented).toBe(false);
  navigation.currentEntry = { key: "same-url" };
  navigation.dispatchEvent(new Event("currententrychange"));
  expect(dispatch("/photos#photo=first", "same-url").defaultPrevented).toBe(false);
  expect(dispatch("/photos#photo=second", "second").defaultPrevented).toBe(true);
  dirty = false;
  expect(dispatch("/photos#photo=second", "second").defaultPrevented).toBe(false);
  dirty = true;
  navigation.currentEntry = { key: "accepted" };
  navigation.dispatchEvent(new Event("currententrychange"));
  history.pushState(null, "", "/photos#photo=second");
  navigation.currentEntry = { key: "rejected" };
  expect(guard.blocked()).toBe(true);
  expect(navigation.traverseTo).toHaveBeenCalledWith("accepted");
  dispose();
});

it("restores classic traversal without losing accepted forward entries", async () => {
  const { guard, setDirty, setAcceptedURL } = classicGuard(true);
  const initial = history.state.docbankPhotoHistory;
  guard.push(null, "/photos");
  const library = history.state.docbankPhotoHistory;
  guard.push({ photoReturn: { contextID: "library" } }, "/photos#photo=first");
  setAcceptedURL(location.href);
  const viewer = history.state.docbankPhotoHistory;
  expect(library).toEqual({ owner: initial.owner, position: initial.position + 1 });
  expect(viewer.position).toBe(library.position + 1);
  guard.replace({ photoReturn: { contextID: "library", scrollTop: 200 } }, location.href);
  expect(history.state.docbankPhotoHistory).toEqual(viewer);
  const go = vi.spyOn(history, "go");
  setDirty(true);
  history.back();
  await waitFor(() => expect(go).toHaveBeenCalledWith(1));
  await waitFor(() => expect(history.state.docbankPhotoHistory).toEqual(viewer));
  expect(history.state.photoReturn).toEqual({ contextID: "library", scrollTop: 200 });
  expect(go).toHaveBeenCalledOnce();
  setDirty(false);
  history.back();
  await waitFor(() => expect(history.state.docbankPhotoHistory).toEqual(library));
  history.forward();
  await waitFor(() => expect(history.state.docbankPhotoHistory).toEqual(viewer));
  history.back();
  await waitFor(() => expect(history.state.docbankPhotoHistory).toEqual(library));
  guard.push(null, "/photos#photo=second");
  setAcceptedURL(location.href);
  const second = history.state.docbankPhotoHistory;
  expect(second.position).toBe(viewer.position);
  setDirty(true);
  history.back();
  await waitFor(() => expect(go).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(history.state.docbankPhotoHistory).toEqual(second));
  setDirty(false);
  history.back();
  await waitFor(() => expect(location.hash).toBe(""));
  setDirty(true);
  history.forward();
  await waitFor(() => expect(go).toHaveBeenLastCalledWith(-1));
  await waitFor(() => expect(history.state.docbankPhotoHistory).toEqual(library));
  setDirty(false);
  history.forward();
  await waitFor(() => expect(location.hash).toBe("#photo=second"));
});

it("checks dirty classic traversal synchronously and tolerates a failed restore", () => {
  const { guard, setDirty, setAcceptedURL } = classicGuard();
  setDirty(true);
  setAcceptedURL("");
  const go = vi.spyOn(history, "go").mockImplementation(() => {});
  const acceptedState = history.state;
  guard.push({ photoReturn: "retained" }, "/photos");
  const firstState = history.state;
  guard.push(null, "/photos");
  const secondState = history.state;
  history.replaceState(acceptedState, "", "/");
  expect(guard.blocked()).toBe(true);
  expect(go).toHaveBeenLastCalledWith(2);
  history.replaceState(secondState, "", "/photos");
  window.dispatchEvent(new PopStateEvent("popstate"));
  history.replaceState(firstState, "", "/photos");
  go.mockImplementationOnce(() => {
    throw new DOMException("Inactive document", "SecurityError");
  });
  expect(guard.blocked()).toBe(true);
  expect(guard.blocked()).toBe(true);
  expect(go).toHaveBeenCalledTimes(3);
});

it("leaves foreign history entries untouched", () => {
  const { guard, setDirty, setAcceptedURL } = classicGuard();
  setDirty(true);
  setAcceptedURL("");
  const go = vi.spyOn(history, "go"),
    replace = vi.spyOn(history, "replaceState");
  const state = {
    docbankPhotoHistory: { owner: "another-document", position: 0 },
    photoReturn: "foreign",
  };
  history.pushState(state, "", "/");
  expect(guard.blocked()).toBe(true);
  expect(history.state).toEqual(state);
  expect(go).not.toHaveBeenCalled();
  expect(replace).not.toHaveBeenCalled();
});

it("restores a typed hash in place so the address matches the open photo", async () => {
  const { guard, setDirty, setAcceptedURL } = classicGuard(true);
  guard.push({ photoReturn: "viewer" }, "/photos#photo=first");
  setAcceptedURL(location.href);
  const viewer = history.state.docbankPhotoHistory;
  setDirty(true);
  history.pushState(null, "", "/photos#photo=second");
  window.dispatchEvent(new PopStateEvent("popstate"));
  expect(location.hash).toBe("#photo=first");
  expect(history.state).toEqual({
    docbankPhotoHistory: { owner: viewer.owner, position: viewer.position + 1 },
  });
  const go = vi.spyOn(history, "go");
  history.pushState(null, "", "/photos#photo=third");
  window.dispatchEvent(new PopStateEvent("popstate"));
  expect(location.hash).toBe("#photo=first");
  expect(history.state.docbankPhotoHistory.position).toBe(viewer.position + 2);
  history.back();
  await waitFor(() => expect(history.state.docbankPhotoHistory.position).toBe(viewer.position + 1));
  expect(go).not.toHaveBeenCalled();
});
