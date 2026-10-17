import { afterEach, expect, it } from "vite-plus/test";
import { cleanup, render, screen } from "@testing-library/svelte";
import PhotoFaceCloseups from "./PhotoFaceCloseups.svelte";
import { PhotoFaces } from "./photoFaces.svelte.js";
import { photo, photoFace, photoPersonAssertion } from "./photo-test-fixtures.js";
afterEach(cleanup);
it("keeps captions without a matching preview and reuses image nodes when names change", async () => {
  const faces = new PhotoFaces("session", () => {});
  faces.version = "version-a";
  faces.faces = [
    photoFace({
      face_id: "small",
      created_at: "2024-01-01T00:00:00Z",
      box: { x: 0, y: 0, width: 0.001, height: 0.001 },
      assertion_id: undefined,
    }),
    photoFace(),
    photoFace({ face_id: "dismissed", dismissed: true }),
  ];
  faces.people = [{ person_id: "person-a", display_name: "Ada", state: "curated" }];
  faces.assertions = [
    photoPersonAssertion(),
    photoPersonAssertion({ assertion_id: "mark", photo_mark: true }),
  ];
  const selected = { ...photo(1), content_version_id: "version-a", rotation: 90 as const };
  const fit = { url: "blob:fit", content_version_id: "version-b", width: 1000, height: 800 };
  const component = render(PhotoFaceCloseups, { photo: selected, faces, fit });
  expect(screen.getAllByText("No preview")).toHaveLength(3);
  expect(screen.queryAllByRole("img")).toHaveLength(0);
  await component.rerender({
    photo: selected,
    faces,
    fit: { ...fit, content_version_id: "version-a" },
  });
  const images = screen.getAllByRole("img");
  expect(images.map((img) => img.getAttribute("alt"))).toEqual(["Ada", "Face 1", "Face 2 · Ada"]);
  expect(images.every((img) => img.getAttribute("src") === "blob:fit")).toBe(true);
  faces.people = faces.people.map((row) => ({ ...row, display_name: "Grace" }));
  await component.rerender({
    photo: selected,
    faces,
    fit: { ...fit, content_version_id: "version-a" },
  });
  expect(screen.getAllByRole("img")[2]).toBe(images[2]);
  expect(images[2]!.getAttribute("alt")).toBe("Face 2 · Grace");
});
