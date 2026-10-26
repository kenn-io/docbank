import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import App from "./App.svelte";
import { root } from "./version-test-fixtures.js";
import { savePanelWidth } from "./panelWidth.js";

beforeEach(() => localStorage.clear());
afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function start() {
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    const value = url.includes("/path?")
      ? root
      : url.includes("/children?")
        ? { directory: root, items: [], total: 0, limit: 1000, offset: 0 }
        : { items: [], total: 0, limit: 1000, offset: 0 };
    return new Response(JSON.stringify(value));
  });
  render(App);
}

it("names the import list the same in the sidebar and its drawer", async () => {
  start();
  await fireEvent.click(await screen.findByRole("button", { name: "Import history" }));
  expect(await screen.findByRole("dialog", { name: "Import history" })).toBeTruthy();
});

it("tells the user the back button has nowhere to go instead of claiming a top folder", async () => {
  start();
  const back = await screen.findByRole("button", { name: "Back to previous directory" });
  await fireEvent.mouseEnter(back.parentElement!);
  expect((await screen.findByRole("tooltip")).textContent).toBe("No previous folder");
});

it("applies the saved inspector width", async () => {
  savePanelWidth("documents", 380);
  start();
  const workspace = await screen.findByRole("main");
  expect(workspace.style.getPropertyValue("--inspector-width")).toBe("min(380px, 50vw)");
});
