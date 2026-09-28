<script module lang="ts">
  export type FileFamily =
    | "folder" | "pdf" | "sheet" | "image" | "email" | "archive"
    | "audio" | "video" | "code" | "text" | "file";

  export function fileFamily(kind: string, mime: string | undefined): FileFamily {
    if (kind === "dir") return "folder";
    const type = (mime ?? "").split(";")[0]!.trim().toLowerCase();
    if (type === "application/pdf") return "pdf";
    if (type === "text/csv" || type.includes("spreadsheet") || type.includes("excel")) return "sheet";
    if (type.startsWith("image/")) return "image";
    if (type === "message/rfc822" || type === "application/mbox") return "email";
    if (type.includes("zip") || type.includes("tar") || type.includes("compressed")) return "archive";
    if (type.startsWith("audio/")) return "audio";
    if (type.startsWith("video/")) return "video";
    if (type === "application/json" || type.includes("xml") || type.includes("javascript")) return "code";
    if (type.startsWith("text/") || type.includes("word") || type.includes("document")) return "text";
    return "file";
  }

  const LABELS: Record<string, string> = {
    "application/pdf": "PDF",
    "text/plain": "Plain text",
    "text/markdown": "Markdown",
    "text/csv": "CSV",
    "text/html": "HTML",
    "application/json": "JSON",
    "message/rfc822": "Email",
    "application/mbox": "Mailbox",
    "application/zip": "ZIP archive",
    "application/msword": "Word document",
    "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "Word document",
    "application/vnd.ms-excel": "Excel spreadsheet",
    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "Excel spreadsheet",
    "application/vnd.openxmlformats-officedocument.presentationml.presentation": "PowerPoint",
  };

  export function fileTypeLabel(kind: string, mime: string | undefined): string {
    if (kind === "dir") return "Folder";
    const type = (mime ?? "").split(";")[0]!.trim().toLowerCase();
    if (!type) return "File";
    const known = LABELS[type];
    if (known) return known;
    const [major, minor = ""] = type.split("/");
    if (major === "image" || major === "audio" || major === "video") {
      return `${minor.replace(/^x-/, "").toUpperCase()} ${major}`;
    }
    return type;
  }
</script>

<script lang="ts">
  import ArchiveIcon from "@lucide/svelte/icons/file-archive";
  import AudioIcon from "@lucide/svelte/icons/file-audio";
  import CodeIcon from "@lucide/svelte/icons/file-code";
  import FileIcon from "@lucide/svelte/icons/file";
  import FolderIcon from "@lucide/svelte/icons/folder";
  import ImageIcon from "@lucide/svelte/icons/file-image";
  import MailIcon from "@lucide/svelte/icons/mail";
  import SheetIcon from "@lucide/svelte/icons/file-spreadsheet";
  import TextIcon from "@lucide/svelte/icons/file-text";
  import VideoIcon from "@lucide/svelte/icons/file-video";

  let { kind, mime = undefined, size = 18 }: { kind: string; mime?: string | undefined; size?: number } = $props();

  const family = $derived(fileFamily(kind, mime));
  const Icon = $derived({
    folder: FolderIcon, pdf: TextIcon, sheet: SheetIcon, image: ImageIcon, email: MailIcon,
    archive: ArchiveIcon, audio: AudioIcon, video: VideoIcon, code: CodeIcon, text: TextIcon, file: FileIcon,
  }[family]);
</script>

<span class="file-type-icon file-type-icon--{family}" aria-hidden="true">
  <Icon {size} strokeWidth={1.75} />
</span>

<style>
  .file-type-icon {
    display: inline-flex;
    flex: 0 0 auto;
    color: var(--text-muted);
  }

  .file-type-icon--folder {
    color: var(--folder-ink);
  }

  .file-type-icon--folder :global(svg) {
    fill: var(--folder-fill);
  }

  .file-type-icon--pdf { color: var(--accent-red); }
  .file-type-icon--sheet { color: var(--accent-green); }
  .file-type-icon--image { color: var(--accent-purple); }
  .file-type-icon--email { color: var(--accent-blue); }
  .file-type-icon--archive { color: var(--accent-amber); }
  .file-type-icon--audio,
  .file-type-icon--video { color: var(--accent-teal); }
</style>
