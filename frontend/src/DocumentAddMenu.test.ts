import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import DocumentAddMenu from "./DocumentAddMenu.svelte";

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it.each(["Upload files", "Upload folder", "Import mailbox", "Import load files"])(
  "opens %s from Add",
  async (label) => {
    const onselect = vi.fn();
    render(DocumentAddMenu, { onselect });
    await fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await fireEvent.click(screen.getByRole("menuitem", { name: label }));
    expect(onselect).toHaveBeenCalledWith(
      {
        "Upload files": "files",
        "Upload folder": "folder",
        "Import mailbox": "mailbox",
        "Import load files": "loadFile",
      }[label],
    );
    expect(screen.queryByRole("menu")).toBeNull();
  },
);

it("keeps disabled choices discoverable with their reason", async () => {
  const onselect = vi.fn();
  render(DocumentAddMenu, { onselect, disabledReason: "Return to a folder before adding files" });
  await fireEvent.click(screen.getByRole("button", { name: "Add" }));
  const folder = screen.getByRole("menuitem", { name: "Upload folder" });
  expect(folder.getAttribute("aria-disabled")).toBe("true");
  expect(folder.querySelector("[title]")?.getAttribute("title")).toBe(
    "Return to a folder before adding files",
  );
  await fireEvent.click(folder);
  expect(onselect).not.toHaveBeenCalled();
});
