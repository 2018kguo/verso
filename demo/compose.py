"""Ink F1 scores and side-by-side demo images (Pillow only).

    python3 compose.py score SOURCE_DIR RENDER_DIR
        mean Ink F1 over every source page (a page the render lacks scores 0)
    python3 compose.py sheet OUT.png SOURCE.png LABEL=RENDER.png ...

Ink F1 is the README metric: pixels darker than luma 220 count as ink, and a
pixel matches when the other render has ink within 2 px. Page directories hold
pdftoppm's numbered PNGs.
"""

import sys
from pathlib import Path

from PIL import Image, ImageChops, ImageDraw, ImageFilter, ImageFont


def ink(path, size):
    g = Image.open(path).convert("L").resize(size)
    return g.point(lambda v: 255 if v < 220 else 0)


def count(mask):
    return mask.histogram()[255]


def f1(source, render):
    size = Image.open(source).size
    src, out = ink(source, size), ink(render, size)
    grow = lambda m: m.filter(ImageFilter.MaxFilter(5))
    s, o = count(src), count(out)
    if not s and not o:
        return 1.0
    if not s or not o:
        return 0.0
    precision = count(ImageChops.multiply(out, grow(src))) / o
    recall = count(ImageChops.multiply(src, grow(out))) / s
    return 2 * precision * recall / (precision + recall) if precision + recall else 0.0


def score(source_dir, render_dir):
    sources = sorted(Path(source_dir).glob("*.png"))
    renders = sorted(Path(render_dir).glob("*.png"))
    pages = [f1(s, r) for s, r in zip(sources, renders)] + [0.0] * max(0, len(sources) - len(renders))
    print(f"{sum(pages) / len(pages):.3f} {len(renders)} " + " ".join(f"{p:.2f}" for p in pages))


def sheet(out, source, *panels):
    base = Image.open(source).convert("RGB")
    tiles = [("Source PDF", base)]
    for p in panels:
        label, path = p.split("=", 1)
        img = Image.open(path).convert("RGB").resize(base.size) if Path(path).exists() else Image.new("RGB", base.size, "white")
        tiles.append((label, img))
    pad, head = 24, 56
    w, h = base.size
    page = Image.new("RGB", (len(tiles) * (w + pad) + pad, h + head + pad), "white")
    draw = ImageDraw.Draw(page)
    try:
        font = ImageFont.truetype("/System/Library/Fonts/Helvetica.ttc", 30)
    except OSError:
        font = ImageFont.load_default()
    for i, (label, img) in enumerate(tiles):
        x = pad + i * (w + pad)
        draw.text((x, 12), label, fill="black", font=font)
        page.paste(img, (x, head))
        draw.rectangle([x - 1, head - 1, x + w, head + h], outline=(180, 180, 180))
    page.save(out, optimize=True)


if __name__ == "__main__":
    {"score": score, "sheet": sheet}[sys.argv[1]](*sys.argv[2:])
