#!/usr/bin/env python3
"""Generate deterministic, synthetic OCR benchmark pages outside the repository."""

import argparse
import json
from pathlib import Path

from PIL import Image, ImageDraw, ImageFilter, ImageFont


WIDTH, HEIGHT = 1240, 1754
REGULAR = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"
BOLD = "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"
SERIF = "/usr/share/fonts/truetype/dejavu/DejaVuSerif.ttf"
CJK = "/usr/share/fonts/truetype/arphic/uming.ttc"


def font(path: str, size: int) -> ImageFont.FreeTypeFont:
    return ImageFont.truetype(path, size)


def page() -> tuple[Image.Image, ImageDraw.ImageDraw]:
    image = Image.new("RGB", (WIDTH, HEIGHT), "white")
    return image, ImageDraw.Draw(image)


def ordinary(path: Path) -> None:
    image, draw = page()
    draw.text((90, 90), "Quarterly Archive Report", font=font(BOLD, 55), fill="black")
    draw.text((90, 190), "This synthetic page tests ordinary searchable prose.", font=font(REGULAR, 31), fill="black")
    draw.text((90, 250), "Invoice reference: DBK-4827. Total records: 1,024.", font=font(REGULAR, 31), fill="black")
    image.save(path)


def columns(path: Path) -> None:
    image, draw = page()
    draw.text((70, 65), "Two-Column Bulletin", font=font(BOLD, 48), fill="black")
    left = ["LEFT COLUMN", "Alpha begins the reading order.", "Beta follows alpha.", "Gamma closes the left column."]
    right = ["RIGHT COLUMN", "Delta begins after gamma.", "Epsilon follows delta.", "Zeta closes the page."]
    for x, lines in ((70, left), (650, right)):
        for i, line in enumerate(lines):
            draw.text((x, 180 + i * 70), line, font=font(BOLD if i == 0 else REGULAR, 29), fill="black")
    draw.line((620, 160, 620, 650), fill="#aaaaaa", width=2)
    image.save(path)


def table(path: Path) -> None:
    image, draw = page()
    draw.text((70, 65), "Inventory Table", font=font(BOLD, 48), fill="black")
    cells = [["Item", "Count", "Price"], ["Archive box", "12", "$48.00"], ["Index card", "300", "$15.00"], ["Total", "312", "$63.00"]]
    xs, ys = [70, 520, 790, 1120], [180, 270, 360, 450, 540]
    for x in xs:
        draw.line((x, ys[0], x, ys[-1]), fill="black", width=3)
    for y in ys:
        draw.line((xs[0], y, xs[-1], y), fill="black", width=3)
    for row, values in enumerate(cells):
        for col, value in enumerate(values):
            draw.text((xs[col] + 18, ys[row] + 22), value, font=font(BOLD if row == 0 else REGULAR, 28), fill="black")
    image.save(path)


def formula(path: Path) -> None:
    image, draw = page()
    draw.text((70, 65), "Formula Notes", font=font(BOLD, 48), fill="black")
    draw.text((100, 220), "Euler identity:", font=font(REGULAR, 34), fill="black")
    draw.text((420, 210), "e^(i*pi) + 1 = 0", font=font(SERIF, 48), fill="black")
    draw.text((100, 330), "Quadratic formula:", font=font(REGULAR, 34), fill="black")
    draw.text((420, 320), "x = (-b +/- sqrt(b^2 - 4ac)) / 2a", font=font(SERIF, 42), fill="black")
    image.save(path)


def multilingual(path: Path) -> None:
    image, draw = page()
    draw.text((70, 65), "Multilingual Notice", font=font(BOLD, 48), fill="black")
    lines = ["English: Private documents remain local.", "Español: Los documentos permanecen locales.", "Français : Les documents restent locaux."]
    for i, line in enumerate(lines):
        draw.text((80, 180 + i * 75), line, font=font(REGULAR, 31), fill="black")
    draw.text((80, 435), "中文：文档保留在本地。", font=font(CJK, 38), fill="black")
    image.save(path)


def poor_scan(path: Path) -> None:
    image, draw = page()
    draw.text((100, 150), "DAMAGED SCAN", font=font(BOLD, 52), fill="#333333")
    draw.text((100, 260), "Ledger entry 1907: cedar shipment forty-two crates.", font=font(SERIF, 32), fill="#555555")
    for y in range(100, 650, 37):
        draw.line((70, y, 1150, y + 8), fill="#dddddd", width=1)
    image = image.rotate(1.4, resample=Image.Resampling.BICUBIC, fillcolor="white").filter(ImageFilter.GaussianBlur(0.8))
    image.save(path, quality=45)


def handwriting(path: Path) -> None:
    image, draw = page()
    script = "/usr/share/fonts/opentype/urw-base35/Z003-MediumItalic.otf"
    draw.text((90, 120), "Handwritten-style field note", font=font(script, 55), fill="#1d315c")
    draw.text((100, 250), "Meet Rowan at the north archive, 3:45 pm.", font=font(script, 45), fill="#1d315c")
    draw.text((100, 340), "Bring catalog 27B and the blue ledger.", font=font(script, 45), fill="#1d315c")
    image = image.rotate(-0.7, resample=Image.Resampling.BICUBIC, fillcolor="white")
    image.save(path)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    cases = {
        "ordinary": (ordinary, ["DBK-4827", "1,024"]),
        "columns": (columns, ["Alpha", "Gamma", "Delta", "Zeta"]),
        "table": (table, ["Archive box", "$63.00"]),
        "formula": (formula, ["Euler", "sqrt"]),
        "multilingual": (multilingual, ["Español", "Français", "文档"]),
        "poor_scan": (poor_scan, ["1907", "forty-two"]),
        "handwriting": (handwriting, ["Rowan", "27B"]),
    }
    manifest = {}
    for name, (render, expected) in cases.items():
        target = args.output / f"{name}.png"
        render(target)
        manifest[name] = {"file": target.name, "expected": expected, "pages": 1}
    first = Image.open(args.output / "ordinary.png")
    second = Image.open(args.output / "table.png")
    first.save(args.output / "two_page.pdf", save_all=True, append_images=[second], resolution=150)
    manifest["two_page"] = {
        "file": "two_page.pdf",
        "expected": ["DBK-4827", "Archive box"],
        "pages": 2,
    }
    (args.output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
