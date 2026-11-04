// Independent PDF authoring keeps crop/rotation expectations literal in the browser test.
export function croppedReviewPDF(fractional = false): Buffer {
  const objects = [
    "<< /Type /Catalog /Pages 2 0 R >>",
    "",
    "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
  ];
  const kids: number[] = [];
  for (const rotation of [0, 90, 180, 270]) {
    const id = objects.length + 1;
    kids.push(id);
    const stream = fractional
      ? "BT /F1 12 Tf 72.5511 781.1234 Td (SECRET) Tj 72 0 Td (KEEP) Tj ET\n"
      : "BT /F1 12 Tf 48 72 Td (SECRET) Tj 72 0 Td (KEEP) Tj ET\n";
    const boxes = fractional
      ? "/MediaBox [0 0 1296 1008] /CropBox [38.5511 25.1234 1190.5511 817.1234]"
      : "/MediaBox [0 0 216 144] /CropBox [24 36 168 108]";
    // The fractional fixture also has an annotation outside the crop. The
    // server still requires a whole-page mask, even though the reader shows no ink.
    const annotation =
      fractional && rotation === 270
        ? "/Annots [<< /Type /Annot /Subtype /Text /Rect [0 0 1 1] /Contents (Synthetic note) >>]"
        : "";
    objects.push(
      `<< /Type /Page /Parent 2 0 R ${boxes} ${annotation} /Rotate ${rotation} /Resources << /Font << /F1 3 0 R >> >> /Contents ${id + 1} 0 R >>`,
      `<< /Length ${Buffer.byteLength(stream)} >>\nstream\n${stream}endstream`,
    );
  }
  objects[1] = `<< /Type /Pages /Count 4 /Kids [${kids.map((n) => `${n} 0 R`).join(" ")}] >>`;
  let pdf = "%PDF-1.7\n";
  const offsets: number[] = [];
  objects.forEach((o, i) => {
    offsets.push(Buffer.byteLength(pdf));
    pdf += `${i + 1} 0 obj\n${o}\nendobj\n`;
  });
  const xref = Buffer.byteLength(pdf);
  pdf += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n${offsets.map((n) => `${String(n).padStart(10, "0")} 00000 n \n`).join("")}trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(pdf);
}
