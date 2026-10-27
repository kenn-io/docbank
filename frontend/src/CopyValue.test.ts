import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import CopyValue from "./CopyValue.svelte";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("copies the whole value and clears feedback when the selected value changes", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  const view = render(CopyValue, { label: "Path", value: "/records/report.txt" });
  await fireEvent.click(screen.getByRole("button", { name: "Copy Path" }));
  expect(writeText).toHaveBeenCalledWith("/records/report.txt");
  expect(await screen.findByText("Copied")).toBeTruthy();
  await view.rerender({ label: "Path", value: "/records/other.txt" });
  expect(screen.queryByText("Copied")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Copy Path" }));
  expect(writeText).toHaveBeenLastCalledWith("/records/other.txt");
});

it("reports a clipboard failure without claiming a successful copy", async () => {
  vi.stubGlobal("navigator", {
    clipboard: {
      writeText: vi.fn().mockRejectedValue(new Error("Clipboard denied")),
    },
  });
  render(CopyValue, { label: "ID", value: "42" });
  await fireEvent.click(screen.getByRole("button", { name: "Copy ID" }));
  expect((await screen.findByRole("status")).textContent).toBe("Couldn't copy");
});
