"""Side-by-side demo image and Ink F1 for one page (Pillow only).

    python3 compose.py OUT.png SOURCE.png LABEL=RENDER.png [LABEL=RENDER.png ...]

Ink F1 is the README metric: pixels darker than luma 220 count as ink, and a
pixel matches when the other render has ink within 2 px.
"""

import sys

from PIL import Image, ImageChops, ImageDraw, ImageFilter, ImageFont


def ink(path, size):
    g = Image.open(path).convert("L").resize(size)
    return g.point(lambda v: 255 if v < 220 else 0)


def count(mask):
    return mask.histogram()[255]


def f1(src, out):
    grow = lambda m: m.filter(ImageFilter.MaxFilter(5))
    s, o = count(src), count(out)
    if not s or not o:
        return 0.0
    precision = count(ImageChops.multiply(out, grow(src))) / o
    recall = count(ImageChops.multiply(src, grow(out))) / s
    return 2 * precision * recall / (precision + recall) if precision + recall else 0.0


def main(out, source, *panels):
    base = Image.open(source).convert("RGB")
    size = base.size
    src_ink = ink(source, size)
    tiles = [("Source PDF", base)]
    for p in panels:
        label, path = p.split("=", 1)
        img = Image.open(path).convert("RGB").resize(size)
        tiles.append((f"{label}  ·  Ink F1 {f1(src_ink, ink(path, size)):.3f}", img))
    pad, head = 24, 56
    w, h = size
    sheet = Image.new("RGB", (len(tiles) * (w + pad) + pad, h + head + pad), "white")
    draw = ImageDraw.Draw(sheet)
    try:
        font = ImageFont.truetype("/System/Library/Fonts/Helvetica.ttc", 30)
    except OSError:
        font = ImageFont.load_default()
    for i, (label, img) in enumerate(tiles):
        x = pad + i * (w + pad)
        draw.text((x, 12), label, fill="black", font=font)
        sheet.paste(img, (x, head))
        draw.rectangle([x - 1, head - 1, x + w, head + h], outline=(180, 180, 180))
    sheet.save(out, optimize=True)


if __name__ == "__main__":
    main(*sys.argv[1:])
