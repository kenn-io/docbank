export type PickerKind = "files" | "folder";

export function localFilePath(file: File): string {
  return (file.webkitRelativePath || file.name).normalize("NFC");
}
