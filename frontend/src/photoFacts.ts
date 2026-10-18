import type { PhotoBrowseRow } from "./generated/docbank.js";

export function photoFacts(photo?: PhotoBrowseRow) {
  const orientations = [
    "",
    "Normal",
    "Mirrored horizontally",
    "Rotated 180°",
    "Mirrored vertically",
    "Mirrored horizontally, rotated 270°",
    "Rotated 90° clockwise",
    "Mirrored horizontally, rotated 90°",
    "Rotated 270° clockwise",
  ];
  return photo
    ? [
        ["Captured", photo.file_modified_time ? undefined : photo.capture_time],
        [
          "File modified",
          photo.file_modified_time
            ? `${photo.file_modified_time.slice(0, 16).replace("T", " ")} UTC`
            : undefined,
        ],
        [
          "Dimensions",
          photo.width_px && photo.height_px ? `${photo.width_px} × ${photo.height_px}` : undefined,
        ],
        ["Camera", [photo.camera_make, photo.camera_model].filter(Boolean).join(" ")],
        ["Lens", [photo.lens_make, photo.lens_model].filter(Boolean).join(" ")],
        ["ISO", photo.iso],
        ["Aperture", photo.f_number == null ? undefined : `f/${photo.f_number}`],
        [
          "Exposure",
          photo.exposure_time_seconds == null
            ? undefined
            : photo.exposure_time_seconds > 0 &&
                photo.exposure_time_seconds < 1 &&
                Math.abs(
                  1 / photo.exposure_time_seconds - Math.round(1 / photo.exposure_time_seconds),
                ) <=
                  1e-6 / photo.exposure_time_seconds
              ? `1/${Math.round(1 / photo.exposure_time_seconds)} s`
              : `${photo.exposure_time_seconds} s`,
        ],
        [
          "Exposure bias",
          photo.exposure_bias_ev == null ? undefined : `${photo.exposure_bias_ev} EV`,
        ],
        ["Focal length", photo.focal_length_mm == null ? undefined : `${photo.focal_length_mm} mm`],
        ["Orientation", photo.orientation == null ? undefined : orientations[photo.orientation]],
        ["Format", photo.media_type],
      ].filter(([, value]) => value !== undefined && value !== null && value !== "")
    : [];
}
