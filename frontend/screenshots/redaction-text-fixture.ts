// Synthetic sources with literal text and crop positions, independent of the reader.
export function textSelectionPDF(
  kind: "small" | "fractional" | "duplicate" | "edge" | "tight" | "large" = "small",
): Buffer {
  const objects = [
    "<< /Type /Catalog /Pages 2 0 R >>",
    "",
    "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
  ];
  const kids: number[] = [];
  for (const angle of kind === "tight" ||
  kind === "edge" ||
  kind === "duplicate" ||
  kind === "large"
    ? [0]
    : [0, 90, 180, 270]) {
    const id = objects.length + 1;
    kids.push(id);
    const fractional = kind === "fractional";
    const boxes =
      kind === "large"
        ? "/MediaBox [0 0 3500 144] /CropBox [0 0 3500 144]"
        : kind === "edge"
          ? "/MediaBox [0 0 324 144] /CropBox [24 24 300 120]"
          : fractional
            ? "/MediaBox [0 0 1296 1008] /CropBox [38.5512 25.1234 1190.5512 817.1234]"
            : "/MediaBox [0 0 216 144] /CropBox [24 24 192 120]";
    const content =
      kind === "large"
        ? `BT /F1 4 Tf 12 96 Td (${"A".repeat(1025)}) Tj ET\n`
        : kind === "edge"
          ? "BT /F1 12 Tf 160 96 Td (KEEP FIRST KEEP FIRST SECRET) Tj -124 -24 Td (KEEP FIRST KEEP FIRST SECRET) Tj ET\n"
          : kind === "tight"
            ? "BT /F1 12 Tf 36 96 Td [(SECRET ) 350 (KEEP)] TJ ET\n"
            : `BT /F1 12 Tf ${fractional ? "72.5512 781.1234" : "36 96"} Td (KEEP FIRST SECRET) Tj 0 -24 Td (SECRET KEEP SECOND) Tj ${kind === "duplicate" ? "0 -24 Td (KEEP FIRST SECRET) Tj" : ""} ET\n`;
    objects.push(
      `<< /Type /Page /Parent 2 0 R ${boxes} /Rotate ${angle} /Resources << /Font << /F1 3 0 R >> >> /Contents ${id + 1} 0 R >>`,
      `<< /Length ${Buffer.byteLength(content)} >>\nstream\n${content}endstream`,
    );
  }
  objects[1] = `<< /Type /Pages /Count ${kids.length} /Kids [${kids.map((n) => `${n} 0 R`).join(" ")}] >>`;
  return encodePDF(objects);
}
function encodePDF(objects: string[]): Buffer {
  let pdf = "%PDF-1.7\n";
  const offsets: number[] = [];
  objects.forEach((value, index) => {
    offsets.push(Buffer.byteLength(pdf));
    pdf += `${index + 1} 0 obj\n${value}\nendobj\n`;
  });
  const xref = Buffer.byteLength(pdf);
  pdf += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n${offsets.map((n) => `${String(n).padStart(10, "0")} 00000 n \n`).join("")}trailer << /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(pdf);
}

export const standardTextFonts = [
  "Courier",
  "Courier-Bold",
  "Courier-Oblique",
  "Courier-BoldOblique",
  "Helvetica",
  "Helvetica-Bold",
  "Helvetica-Oblique",
  "Helvetica-BoldOblique",
  "Times-Roman",
  "Times-Bold",
  "Times-Italic",
  "Times-BoldItalic",
];
const font = (name: string) => `<< /Type /Font /Subtype /Type1 /BaseFont /${name} >>`;
export const variedTextCases = [
  {
    name: "TJ generated space",
    target: "D WORDS",
    match: true,
    fonts: [font("Helvetica")],
    content: "BT /F1 12 Tf 72 720 Td [(KERNE) -333 (D WORDS)] TJ ET",
  },
  {
    name: "fi glyph name",
    target: "office",
    match: false,
    fonts: [
      "<< /Type /Font /Subtype /Type1 /BaseFont /Times-Roman /Encoding << /BaseEncoding /WinAnsiEncoding /Differences [174 /fi] >> >>",
    ],
    content: "BT /F1 12 Tf 72 720 Td (of\\256ce filing) Tj ET",
  },
  {
    name: "fi ToUnicode",
    target: "office",
    match: false,
    fonts: [
      "<< /Type /Font /Subtype /Type1 /BaseFont /Times-Roman /Encoding << /BaseEncoding /WinAnsiEncoding /Differences [174 /fi] >> /ToUnicode CMAP >>",
    ],
    cmap: "1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <AE> <00660069> endbfchar",
    content: "BT /F1 12 Tf 72 720 Td (of\\256ce filing) Tj ET",
  },
  {
    name: "three fonts",
    target: "Beta",
    match: true,
    fonts: [font("Helvetica"), font("Times-Roman"), font("Courier")],
    content: "BT /F1 12 Tf 72 720 Td (Alpha ) Tj /F2 12 Tf (Beta ) Tj /F3 12 Tf (Gamma) Tj ET",
  },
  {
    name: "WinAnsi accents",
    target: "résumé",
    match: true,
    fonts: ["<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"],
    content: "BT /F1 12 Tf 72 720 Td (caf\\351 na\\357ve r\\351sum\\351) Tj ET",
  },
  {
    name: "tight words",
    target: "SECRET",
    match: true,
    fonts: [font("Helvetica")],
    content: "BT /F1 12 Tf 72 720 Td [(SECRET ) 300 (KEEP)] TJ ET",
  },
];
export function textCoveragePDF(standard: boolean): Buffer {
  const pages = standard
    ? standardTextFonts.map((name) => ({
        fonts: [font(name)],
        cmap: "",
        content: "BT /F1 12 Tf 72.37 720 Td (of the staff of life, if it fits.) Tj ET",
      }))
    : variedTextCases;
  const objects = ["<< /Type /Catalog /Pages 2 0 R >>", ""],
    kids: number[] = [];
  for (const page of pages) {
    const fonts = page.fonts.map((value) => {
      if (page.cmap) {
        const id = objects.length + 1;
        objects.push(
          `<< /Length ${Buffer.byteLength(page.cmap)} >>\nstream\n${page.cmap}\nendstream`,
        );
        value = value.replace("CMAP", `${id} 0 R`);
      }
      objects.push(value);
      return objects.length;
    });
    const content = objects.length + 1;
    objects.push(
      `<< /Length ${Buffer.byteLength(page.content)} >>\nstream\n${page.content}\nendstream`,
    );
    kids.push(objects.length + 1);
    objects.push(
      `<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << ${fonts.map((id, i) => `/F${i + 1} ${id} 0 R`).join(" ")} >> >> /Contents ${content} 0 R >>`,
    );
  }
  objects[1] = `<< /Type /Pages /Count ${kids.length} /Kids [${kids.map((id) => `${id} 0 R`).join(" ")}] >>`;
  return encodePDF(objects);
}
