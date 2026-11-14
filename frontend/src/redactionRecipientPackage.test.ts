import { afterEach, expect, it, vi } from "vite-plus/test";
import type { BatesExport } from "./generated/docbank.js";
import type { NumberingInputs } from "./redactionNumbering.js";
import { decodeRecipientPackage, recipientText } from "./redactionRecipientPackage.js";
import { RecipientPackageController } from "./recipientPackageController.svelte.js";
import { mutationsInFlight } from "./api-transport.js";

const allocation = "11111111-1111-4111-8111-111111111111";
const namespace = "22222222-2222-4222-8222-222222222222";
const inputs = {
  preparation: {},
  masks: [],
  review: {
    sha256: "a".repeat(64),
    resolved: {
      page_count: 2,
      runs: [
        { page: 1, text: "[REDACTED] " },
        { page: 1, text: "café" },
      ],
    },
  },
} as unknown as NumberingInputs;
const text = {
  size: 18,
  sha256: "9c6c0c72a0d9c40bc29138ca3fb086b361cdff82d7f7b32f22be6050aa96e797",
};
const artifact = {
  allocation_id: allocation,
  namespace: { namespace_id: namespace },
  page_count: 2,
  pages: [{ label: "CASE000001" }, { label: "CASE000002" }],
  manifest_sha256: "b".repeat(64),
  recipe_sha256: "c".repeat(64),
  blob_sha256: "d".repeat(64),
  size: 100,
} as BatesExport;
function response() {
  const file = (path: string) => ({ path, size: 10, sha256: "f".repeat(64) });
  const profile = { encoding: "utf-8", date_format: "", declared_timezone: "" };
  return {
    contract: "native-pdf-recipient-download/v1",
    id: "e".repeat(48),
    allocation_id: allocation,
    expires_at: "2026-01-01T00:02:00Z",
    name: "recipient.zip",
    verification: "masks_text_and_numbers",
    url: "/api/daemon/web-download/file?ticket=" + "A".repeat(43),
    size: 1234,
    sha256: "e".repeat(64),
    receipt: {
      contract: "native-pdf-recipient-package/v1",
      document_count: 1,
      page_count: 2,
      dat_profile: {
        ...profile,
        id: "dat-concordance-v1",
        field: 20,
        qualifier: 254,
        newline_in_field: 174,
        header_row: true,
        columns: ["DOCID", "LABELSET", "BEGBATES", "ENDBATES", "PDF", "TEXT", "PAGECOUNT"],
      },
      opt_profile: {
        ...profile,
        id: "opt-standard-v1",
        field: 44,
        qualifier: 0,
        newline_in_field: 0,
        header_row: false,
        columns: [
          "ImageKey",
          "VolumeName",
          "ImagePath",
          "DocumentBreak",
          "FolderBreak",
          "BoxBreak",
          "PageCount",
        ],
      },
      load_file: file("VOL001/DATA.DAT"),
      page_map: file("VOL001/DATA.OPT"),
      mapping: file("mapping.json"),
      documents: [
        {
          ordinal: 1,
          document_id: "CASE000001",
          allocation_id: allocation,
          namespace_id: namespace,
          artifact_manifest_sha256: "b".repeat(64),
          recipe_sha256: "c".repeat(64),
          review_sha256: "a".repeat(64),
          page_count: 2,
          labels: ["CASE000001", "CASE000002"],
          pdf: { path: "VOL001/PDF/000001.pdf", size: 100, sha256: "d".repeat(64) },
          text: { path: "VOL001/TEXT/000001.txt", ...text },
        },
      ],
    },
  };
}
afterEach(() => vi.restoreAllMocks());
it("pins label-free UTF-8 text, including empty pages, independently of the returned receipt", () => {
  expect(recipientText(inputs)).toEqual(text);
  expect(decodeRecipientPackage(response(), inputs, artifact).receipt.documents[0]!.text).toEqual({
    path: "VOL001/TEXT/000001.txt",
    ...text,
  });
  for (const change of [
    (r: ReturnType<typeof response>) => {
      r.receipt.documents[0]!.text.sha256 = "0".repeat(64);
    },
    (r: ReturnType<typeof response>) => {
      r.receipt.documents[0]!.pdf.sha256 = "0".repeat(64);
    },
    (r: ReturnType<typeof response>) => {
      r.receipt.documents[0]!.review_sha256 = "0".repeat(64);
    },
    (r: ReturnType<typeof response>) => {
      r.receipt.documents[0]!.allocation_id = namespace;
      r.allocation_id = namespace;
    },
    (r: ReturnType<typeof response>) => {
      r.receipt.documents[0]!.labels = ["CASE000003", "CASE000004"];
      r.receipt.documents[0]!.document_id = "CASE000003";
    },
    (r: ReturnType<typeof response>) => {
      r.receipt.dat_profile.columns.reverse();
    },
    (r: ReturnType<typeof response>) => {
      r.receipt.page_count = 3;
      r.receipt.documents[0]!.page_count = 3;
    },
    (r: ReturnType<typeof response>) => {
      r.verification = "recorded_bytes_and_numbers";
    },
    (r: ReturnType<typeof response>) => {
      r.url = "https://example.org" + r.url;
    },
    (r: ReturnType<typeof response>) => {
      r.url += "&ticket=" + "B".repeat(43);
    },
    (r: ReturnType<typeof response>) => {
      r.receipt.mapping.path = "../mapping.json";
    },
  ]) {
    const r = response();
    change(r);
    expect(() => decodeRecipientPackage(r, inputs, artifact)).toThrow();
  }
});

it("offers a native ticket download bound to the completed delivery without counting the build as a vault write", async () => {
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    expect(this.getAttribute("href")).toBe(response().url);
    expect(this.download).toBe("recipient.zip");
  });
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, options) => {
    expect(mutationsInFlight()).toBe(0);
    expect(JSON.parse(options!.body as string)).toEqual({
      allocation_id: allocation,
      preparation: {},
      masks: [],
      review_sha256: "a".repeat(64),
    });
    return new Response(JSON.stringify(response()));
  });
  const controller = new RecipientPackageController(() => "tab", vi.fn());
  await controller.start(inputs, artifact);
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(click).toHaveBeenCalledTimes(1);
  expect(controller.phase).toBe("idle");
  expect(controller.message).toBe("Download started. Keep this review open until it finishes.");
  fetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
  await controller.stop();
});

it("keeps retries blocked after a lost creation reply until fresh owner cleanup succeeds", async () => {
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockRejectedValueOnce(new TypeError("Lost reply"))
    .mockResolvedValueOnce(
      new Response(JSON.stringify({ detail: "Removal failed" }), { status: 500 }),
    );
  const controller = new RecipientPackageController(() => "tab", vi.fn());
  await controller.start(inputs, artifact);
  expect(controller.phase).toBe("cleanup-failed");
  expect(controller.error).toBe(
    "The recipient ZIP could not be released. Release it before trying again.",
  );
  await controller.start(inputs, artifact);
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(fetch.mock.calls[1]![1]!.method).toBe("DELETE");
  expect(fetch.mock.calls[1]![1]!.signal?.aborted).toBe(false);
  fetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
  await controller.release();
  expect(controller.phase).toBe("idle");
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
  fetch.mockResolvedValueOnce(new Response(JSON.stringify(response())));
  await controller.start(inputs, artifact);
  expect(click).toHaveBeenCalledTimes(1);
  fetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
  await controller.stop();
});

it("serializes cancellation cleanup across deliveries and ignores a late successful reply", async () => {
  let created!: (r: Response) => void, released!: (r: Response) => void;
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          created = resolve;
        }),
    )
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          released = resolve;
        }),
    );
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
  const controller = new RecipientPackageController(() => "tab", vi.fn());
  const build = controller.start(inputs, artifact);
  const cleanup = controller.cancel();
  expect(controller.phase).toBe("releasing");
  expect(fetch.mock.calls[0]![1]!.signal?.aborted).toBe(true);
  expect(fetch.mock.calls[1]![1]!.signal?.aborted).toBe(false);
  await controller.start(inputs, { ...artifact, allocation_id: namespace });
  expect(fetch).toHaveBeenCalledTimes(2);
  created(new Response(JSON.stringify(response())));
  await build;
  expect(click).not.toHaveBeenCalled();
  released(new Response(null, { status: 204 }));
  await cleanup;
  expect(controller.phase).toBe("idle");
});

it("does not cancel an existing transfer when another package reports busy", async () => {
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(
      new Response(
        JSON.stringify({ code: "recipient_package_busy", detail: "Another package is active" }),
        { status: 503 },
      ),
    );
  const controller = new RecipientPackageController(() => "tab", vi.fn());
  await controller.start(inputs, artifact);
  expect(controller.phase).toBe("idle");
  expect(controller.error).toBe(
    "Another recipient ZIP is being prepared or downloaded. Try again when it finishes.",
  );
  expect(fetch).toHaveBeenCalledTimes(1);
});

it("keeps one download status line without hiding a package that is still building", async () => {
  let created!: (r: Response) => void;
  vi.spyOn(globalThis, "fetch").mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        created = resolve;
      }),
  );
  const controller = new RecipientPackageController(() => "tab", vi.fn());
  expect(controller.note("first", "PDF started")).toBe(true);
  expect([controller.allocation, controller.message]).toEqual(["first", "PDF started"]);
  const build = controller.start(inputs, artifact);
  expect(controller.note("first", "PDF started")).toBe(false);
  expect(controller.allocation).toBe(artifact.allocation_id);
  created(new Response(JSON.stringify({ code: "recipient_package_busy" }), { status: 503 }));
  await build;
  expect(controller.note("first", "PDF started")).toBe(true);
  expect(controller.error).toBe("");
});

it("keeps a failed release recoverable after the review that owned it closes", async () => {
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(new Response(JSON.stringify(response())))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Busy disk" }), { status: 500 }));
  const controller = new RecipientPackageController(() => "tab", vi.fn());
  await controller.start(inputs, artifact);
  expect(click).toHaveBeenCalledOnce();
  await controller.stop();
  // No delivery owns the failure now, so the review panel shows the release action.
  expect([controller.phase, controller.allocation]).toEqual(["cleanup-failed", ""]);
  expect(controller.error).toBe(
    "The recipient ZIP could not be released. Release it before trying again.",
  );
  fetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
  await controller.release();
  expect(controller.phase).toBe("idle");
  expect(fetch.mock.calls.at(-1)![1]!.method).toBe("DELETE");
});
