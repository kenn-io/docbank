import { expect, it, vi } from "vite-plus/test";
import { APIError } from "./api-transport.js";
import { readHistory } from "./historyRead.js";

it("cancels a pending retry when the source changes", async () => {
  const controller = new AbortController();
  const read = vi.fn().mockRejectedValue(new APIError("Unavailable", 503, "unavailable"));
  const pending = readHistory(read, controller.signal);
  await Promise.resolve();
  controller.abort();
  await expect(pending).rejects.toBeDefined();
  await new Promise((resolve) => setTimeout(resolve, 550));
  expect(read).toHaveBeenCalledTimes(1);
});
