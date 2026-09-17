import type { MediaArtifactMetadata, MediaSuppliedMetadata } from "./generated/docbank";

// These routes require JSON metadata without a filename before the file part.
// FormData cannot set a JSON part's content type without adding a filename.
export function mediaFormData(input: {
  metadata: MediaArtifactMetadata | MediaSuppliedMetadata;
  file: Blob;
}): Blob {
  const boundary = `docbank-${crypto.randomUUID()}`;
  const filename = input.metadata.filename.replace(/\r/g, "%0D").replace(/\n/g, "%0A").replace(/"/g, "%22");
  return new Blob([
    `--${boundary}\r\nContent-Disposition: form-data; name="metadata"\r\nContent-Type: application/json\r\n\r\n`,
    JSON.stringify(input.metadata),
    `\r\n--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="${filename}"\r\nContent-Type: ${input.metadata.media_type}\r\n\r\n`,
    input.file,
    `\r\n--${boundary}--\r\n`,
  ], { type: `multipart/form-data; boundary=${boundary}` });
}
