import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import LocalFilePicker from "./LocalFilePicker.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it.each(["files", "folder"] as const)("chooses %s and preserves folder paths", async (kind) => {
  const onchoose = vi.fn();
  render(LocalFilePicker, { onchoose });
  const input = screen.getByLabelText(
    kind === "files" ? "Choose local files" : "Choose local folder",
  );
  const click = vi.spyOn(input as HTMLInputElement, "click");
  await fireEvent.click(
    screen.getByRole("button", { name: kind === "files" ? "Choose files" : "Choose folder" }),
  );
  expect(click).toHaveBeenCalledOnce();
  const file = new File(["report"], "report.txt");
  Object.defineProperty(file, "webkitRelativePath", {
    value: kind === "folder" ? "Reports/2026/report.txt" : "",
  });
  await fireEvent.change(input, { target: { files: [file] } });
  expect(onchoose).toHaveBeenCalledWith([file], kind);
});

it("ignores selections while disabled", async () => {
  const onchoose = vi.fn();
  render(LocalFilePicker, { onchoose, disabled: true });
  await fireEvent.change(screen.getByLabelText("Choose local files"), {
    target: { files: [new File(["x"], "x.txt")] },
  });
  expect(onchoose).not.toHaveBeenCalled();
});
