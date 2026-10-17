import { afterEach, expect, it, vi } from "vite-plus/test";
import { inspectionReporter } from "./action-events.js";

afterEach(() => vi.restoreAllMocks());

it("sends a later inspection after delivery fails", async () => {
  const fetcher = vi
    .spyOn(globalThis, "fetch")
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValue(new Response(null, { status: 202 }));
  const inspect = inspectionReporter("document_opened", () => "synthetic-session");
  inspect();
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  inspect();
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
});
