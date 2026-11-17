// Geometry only. Layout and all OOXML generation belong to the Go converter.
import fs from "node:fs";
import os from "node:os";
import { pathToFileURL } from "node:url";
import {
  isMainThread,
  parentPort,
  Worker,
  workerData,
} from "node:worker_threads";
import { crc32, deflateSync } from "node:zlib";

const [file, entry, maxPagesString] = isMainThread
  ? process.argv.slice(1)
  : workerData;
const { openPdf } = await import(pathToFileURL(entry).href);
// say writes one line of output whole. A worker thread's stdio leaves the
// pipe non-blocking: a write can take part of a page, or none (EAGAIN) while
// the converter catches up.
const pause = new Int32Array(new SharedArrayBuffer(4));
const say = (value) => {
  const line = Buffer.from(`${JSON.stringify(value)}\n`);
  for (let at = 0; at < line.length;) {
    try {
      at += fs.writeSync(1, line, at);
    } catch (error) {
      if (error.code !== "EAGAIN") {
        throw error;
      }
      Atomics.wait(pause, 0, 0, 1);
    }
  }
};

// level reports a glyph whose run advances along a horizontal baseline:
// its neighbour in the same text object sits beside it, not above.
function level(text, i) {
  for (const j of [i + 1, i - 1]) {
    const sameRun =
      text.runIds[j] === text.runIds[i] ||
      (text.fontIds[j] === text.fontIds[i] &&
        text.angles[j] === text.angles[i]);
    if (j >= 0 && j < text.count && sameRun) {
      const dx = text.boxes[4 * j] - text.boxes[4 * i];
      const dy =
        text.boxes[4 * j + 1] +
        text.boxes[4 * j + 3] -
        (text.boxes[4 * i + 1] + text.boxes[4 * i + 3]);
      return Math.abs(dy) < 0.5 && Math.abs(dx) > 0.5;
    }
  }
  return false;
}

const touching = (a, b, pad) =>
  a.x <= b.x + b.width + pad &&
  b.x <= a.x + a.width + pad &&
  a.y <= b.y + b.height + pad &&
  b.y <= a.y + a.height + pad;

// Boxes that touch within `pad` points merge; a grid keeps it near linear.
// Null when the page needs more than a few million comparisons.
function clusters(boxes, pad) {
  let budget = 4_000_000;
  const parent = boxes.map((_, i) => i);
  const root = (start) => {
    let i = start;
    while (parent[i] !== i) {
      parent[i] = parent[parent[i]];
      i = parent[i];
    }
    return i;
  };
  const cell = 32;
  const grid = new Map();
  for (const [i, b] of boxes.entries()) {
    const x0 = Math.floor((b.x - pad) / cell);
    const x1 = Math.floor((b.x + b.width + pad) / cell);
    const y0 = Math.floor((b.y - pad) / cell);
    const y1 = Math.floor((b.y + b.height + pad) / cell);
    for (let gx = x0; gx <= x1; gx++) {
      for (let gy = y0; gy <= y1; gy++) {
        const key = gx * 65_536 + gy;
        const list = grid.get(key) ?? [];
        let covered = false;
        for (const j of list) {
          if (--budget < 0) {
            return null;
          }
          const c = boxes[j];
          if (touching(b, c, pad)) {
            parent[root(i)] = root(j);
            // A box inside a stored one adds nothing the stored one won't
            // catch: stacked copies of one shape stay one entry.
            covered ||=
              b.x >= c.x &&
              b.y >= c.y &&
              b.x + b.width <= c.x + c.width &&
              b.y + b.height <= c.y + c.height;
          }
        }
        if (!covered) {
          list.push(i);
          grid.set(key, list);
        }
      }
    }
  }
  const out = new Map();
  for (const [i, b] of boxes.entries()) {
    const r = root(i);
    const c = out.get(r);
    if (c) {
      const right = Math.max(c.x + c.width, b.x + b.width);
      const bottom = Math.max(c.y + c.height, b.y + b.height);
      c.x = Math.min(c.x, b.x);
      c.y = Math.min(c.y, b.y);
      c.width = right - c.x;
      c.height = bottom - c.y;
      c.thick ||= b.thick;
    } else {
      out.set(r, { ...b });
    }
  }
  return [...out.values()];
}

// A page region cut to the page; null when nothing is left.
// A dingbat font Word lacks draws its own pictures whatever its text says (a
// court form's checkbox read as "’"): those glyphs keep their look as images.
// Word's own Wingdings and Webdings draw as text.
function pictographs(name = "") {
  const family = name.replace(/^[A-Z]{6}\+/, "");
  return (
    /dingbat|iconic|wingding|webding/i.test(family) &&
    !/^(wingdings( ?[23])?|webdings)(-|,|$)/i.test(family)
  );
}

// A font's weight, or its name's style when it reports none: Type 3 fonts
// ("NotoSansCJKsc-Bold") carry no weight, and bold headings came out plain.
function weight(font) {
  const style =
    (font?.name ?? "").replace(/^[A-Z]{6}\+/, "").split(/[-,]/)[1] ?? "";
  // Other weights name a Word family ("Arial Black"), as fontName has it.
  const named =
    /bold/i.test(style) && !/semi|demi|extra|ultra/i.test(style) ? 700 : 400;
  return Math.max(font?.weight ?? 400, named);
}

// four reads four numbers from a typed array as a plain array: a subarray
// view destructured per glyph was a tenth of a page's reading.
const four = (a, at) => [a[at], a[at + 1], a[at + 2], a[at + 3]];

// hidden reports whether a page's text is an OCR layer it does not show
// (rendering mode 3, or painted over by its scan): Word drew it over the scan,
// doubled, and the extra text ran onto pages of its own. A glyph is hidden
// when rendering the page without text changes none of its box's pixels; the
// layer is hidden when nine in ten of its glyphs are, as one thin "." at this
// resolution can look. Only pages with a large image are checked (two renders).
function hidden(pdf, pageIndex, info, text, images) {
  // Where large images lie, on a 2pt grid: each image marks its corners in
  // a difference array, one prefix sum fills the grid, and a glyph is one
  // lookup, however many images a page draws (or repeats).
  const cell = 2;
  const cols = Math.ceil(info.width / cell) + 2;
  const rows = Math.ceil(info.height / cell) + 2;
  const grid = new Int32Array(cols * rows);
  const at = (v, n) => Math.min(n - 2, Math.max(0, Math.floor(v / cell)));
  let big = false;
  for (let i = 0; i < images.count; i++) {
    const [x, y, w, h] = four(images.boxes, 4 * i);
    if (w * h > 0.25 * info.width * info.height) {
      big = true;
      const [c0, c1] = [at(x, cols), at(x + w, cols) + 1];
      const [r0, r1] = [at(y, rows), at(y + h, rows) + 1];
      grid[r0 * cols + c0]++;
      grid[r0 * cols + c1]--;
      grid[r1 * cols + c0]--;
      grid[r1 * cols + c1]++;
    }
  }
  if (!big || !text.count) {
    return false;
  }
  // Prefix sums along rows, then down columns.
  for (let k = 1; k < grid.length; k++) {
    grid[k] += k % cols ? grid[k - 1] : 0;
  }
  for (let k = cols; k < grid.length; k++) {
    grid[k] += grid[k - cols];
  }
  // An OCR layer lies on its scan: text a tenth of which is off every large
  // image is the page's own, with no need to render the page twice.
  let ink = 0;
  let on = 0;
  for (let i = 0; i < text.count; i++) {
    if (text.codepoints[i] > 32) {
      const cx = text.boxes[4 * i] + text.boxes[4 * i + 2] / 2;
      const cy = text.boxes[4 * i + 1] + text.boxes[4 * i + 3] / 2;
      ink++;
      on += grid[at(cy, rows) * cols + at(cx, cols)] > 0 ? 1 : 0;
    }
  }
  if (on < 0.9 * ink) {
    return false;
  }
  const options = { width: 400, maxHeight: 2048 };
  const shown = pdf.render(pageIndex, { ...options, text: true });
  const bare = pdf.render(pageIndex, { ...options, text: false });
  const sx = shown.width / info.width;
  const sy = shown.height / info.height;
  ink = 0;
  let unseen = 0;
  for (let i = 0; i < text.count; i++) {
    if (!String.fromCodePoint(text.codepoints[i]).trim()) {
      continue;
    }
    const x0 = Math.max(0, Math.floor(text.boxes[4 * i] * sx));
    const y0 = Math.max(0, Math.floor(text.boxes[4 * i + 1] * sy));
    const x1 = Math.min(
      shown.width,
      Math.ceil((text.boxes[4 * i] + text.boxes[4 * i + 2]) * sx)
    );
    const y1 = Math.min(
      shown.height,
      Math.ceil((text.boxes[4 * i + 1] + text.boxes[4 * i + 3]) * sy)
    );
    let same = true;
    for (let y = y0; same && y < y1; y++) {
      const end = (y * shown.width + x1) * 4;
      for (let k = (y * shown.width + x0) * 4; k < end; k++) {
        if (Math.abs(shown.rgba[k] - bare.rgba[k]) > 24) {
          same = false;
          break;
        }
      }
    }
    ink++;
    unseen += same ? 1 : 0;
  }
  return ink > 0 && unseen >= 0.9 * ink;
}

// inked colors the rules Word may draw from a render of the page without
// text (about a millisecond): a rule its surroundings hide (a white frame
// around a chart's source line) is left out, and a colored one keeps its
// color. Anti-aliasing greys a thin black rule, so grey is black.
function inked(pdf, pageIndex, info, segments) {
  const open = segments.filter((s) => !s.painted);
  if (!open.length) {
    return;
  }
  const page = pdf.render(pageIndex, {
    width: Math.min(2048, Math.ceil(info.width * 2)),
    maxHeight: 2048,
    text: false,
  });
  const sx = page.width / info.width;
  const sy = page.height / info.height;
  const at = (x, y) => {
    const px = Math.min(page.width - 1, Math.max(0, Math.round(x * sx)));
    const py = Math.min(page.height - 1, Math.max(0, Math.round(y * sy)));
    const i = (py * page.width + px) * 4;
    return [page.rgba[i], page.rgba[i + 1], page.rgba[i + 2]];
  };
  const off = (a, b) => Math.max(...a.map((v, k) => Math.abs(v - b[k])));
  for (const s of open) {
    const horizontal = Math.abs(s.y2 - s.y1) < 0.5;
    const length = Math.hypot(s.x2 - s.x1, s.y2 - s.y1);
    const steps = Math.min(48, Math.max(2, Math.floor(length / 2)));
    let best = null;
    let most = 0;
    for (let k = 0; k <= steps; k++) {
      const x = s.x1 + ((s.x2 - s.x1) * k) / steps;
      const y = s.y1 + ((s.y2 - s.y1) * k) / steps;
      // The background: 2pt clear of the stroke to either side.
      const d = (s.width || 1) / 2 + 2;
      const beside = horizontal
        ? [at(x, y - d), at(x, y + d)]
        : [at(x - d, y), at(x + d, y)];
      // Across the rule too: an edge of a thin filled bar has its ink to
      // one side, half a point at most.
      for (const across of [-0.5, 0, 0.5]) {
        const ink = horizontal ? at(x, y + across) : at(x + across, y);
        // Either side: a rule beside a fill or a parallel rule is still ink.
        const diff = Math.max(off(ink, beside[0]), off(ink, beside[1]));
        if (diff > most) {
          most = diff;
          best = ink;
        }
      }
    }
    if (most < 40) {
      s.painted = true; // nothing to see: not drawn
    } else if (Math.max(...best) - Math.min(...best) > 24) {
      // A hue, lightened by anti-aliasing: back to full strength.
      const k = 255 / Math.max(1, 255 - Math.min(...best));
      s.color = best
        .map((v) =>
          Math.max(0, Math.round(255 - (255 - v) * k))
            .toString(16)
            .padStart(2, "0")
        )
        .join("");
    }
  }
}

function clip(info, b) {
  const x = Math.max(0, b.x);
  const y = Math.max(0, b.y);
  const box = {
    height: Math.min(info.height, b.y + b.height) - y,
    width: Math.min(info.width, b.x + b.width) - x,
    x,
    y,
  };
  return box.width < 0.5 || box.height < 0.5 ? null : box;
}

// A region render's pixels are the whole-page render's at the same scale
// (Recto's contract), and it costs about as much, so a page with several
// regions renders once and crops. One region renders alone. Images and art
// render without the page's text, which Word draws over them as text; only a
// region of characters kept as an image (box.text) renders with it.
function pageImages(pdf, pageIndex, info, boxes) {
  const options = {
    maxHeight: 2048,
    width: Math.min(2048, Math.ceil(info.width * 1.5)),
  };
  const withText = boxes.filter((b) => b.text).length;
  const counts = { true: withText, false: boxes.length - withText };
  const pages = {};
  return boxes.map(({ text, ...box }) => {
    const opts = { ...options, text };
    if (counts[text] < 2) {
      const bitmap = pdf.render(pageIndex, { ...opts, region: box });
      return { ...box, png: png(bitmap), truncated: bitmap.truncated };
    }
    pages[text] ??= pdf.render(pageIndex, opts);
    return {
      ...box,
      png: png(cropped(pages[text], info, box)),
      truncated: pages[text].truncated,
    };
  });
}

function cropped(page, info, box) {
  const sx = page.width / info.width;
  const sy = page.height / info.height;
  const x = Math.floor(box.x * sx);
  const y = Math.floor(box.y * sy);
  const width = Math.min(page.width, Math.ceil((box.x + box.width) * sx)) - x;
  const height =
    Math.min(page.height, Math.ceil((box.y + box.height) * sy)) - y;
  return crop(page, x, y, width, height);
}

function crop(bitmap, x, y, width, height) {
  const rgba = new Uint8Array(width * height * 4);
  for (let row = 0; row < height; row++) {
    const start = ((y + row) * bitmap.width + x) * 4;
    rgba.set(bitmap.rgba.subarray(start, start + width * 4), row * width * 4);
  }
  return { width, height, rgba };
}

// Controls XML cannot carry (tab, newline and return can).
const unsafe = (c) =>
  (c < 0x20 && c !== 0x09 && c !== 0x0a && c !== 0x0d) ||
  c === 0xff_fe ||
  c === 0xff_ff;

function png(bitmap) {
  // Recto's bitmaps are opaque: RGB, with PNG's Up filter (each byte less
  // the one above it), which turns the rows of a rendered page into runs of
  // zeros that deflate at a fast level squeezes well.
  const { width, height, rgba } = bitmap;
  const stride = 1 + width * 3;
  const rows = Buffer.allocUnsafe(height * stride); // every byte written
  for (let i = 0, o = 0, y = 0; y < height; y++) {
    rows[o++] = 2;
    for (let x = 0; x < width; x++, i += 4) {
      const up = i - width * 4;
      rows[o++] = y ? rgba[i] - rgba[up] : rgba[i];
      rows[o++] = y ? rgba[i + 1] - rgba[up + 1] : rgba[i + 1];
      rows[o++] = y ? rgba[i + 2] - rgba[up + 2] : rgba[i + 2];
    }
  }
  const chunk = (name, data) => {
    const type = Buffer.from(name);
    const header = Buffer.alloc(4);
    header.writeUInt32BE(data.length);
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(data, crc32(type)));
    return Buffer.concat([header, type, data, crc]);
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width);
  header.writeUInt32BE(height, 4);
  header[8] = 8;
  header[9] = 2;
  return Buffer.concat([
    Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]),
    chunk("IHDR", header),
    chunk("IDAT", deflateSync(rows, { level: 3 })),
    chunk("IEND", Buffer.alloc(0)),
  ]).toString("base64");
}

// build reads one page: its geometry, and the boxes its images will be
// rendered from (finish renders them). scan: a page with nothing but a scan.
function build(doc, pageIndex) {
  let scan = false;
  const info = doc.pageInfo(pageIndex);
  // Angles are in the content frame; /Rotate turns the page clockwise for
  // display, so text reads horizontally where angle equals the rotation.
  const rotation = (info.rotation * Math.PI) / 180;
  const tilted = (angle) => {
    const a = Math.atan2(
      Math.sin(angle - rotation),
      Math.cos(angle - rotation)
    );
    return Math.abs(a) > 0.01;
  };
  const { text, objects, annotations } = doc.scan(pageIndex);
  if (text.truncated || objects.truncated) {
    throw new Error(`page ${pageIndex + 1} truncated`);
  }
  if (text.count > 200_000) {
    throw new Error("page complexity limit");
  }
  // Past these, vector art and images are left out, not the page.
  const pathCount = objects.paths.count > 20_000 ? 0 : objects.paths.count;
  const fonts = text.fonts;
  // Per font, not per glyph: both read the font's name.
  const weights = fonts.map(weight);
  const pictures = fonts.map((f) => pictographs(f?.name));
  const links = doc.links(pageIndex);
  const glyphs = [];
  // Glyphs Word cannot carry as text (no Unicode, or rotated) keep their
  // look as rendered images of their boxes instead of refusing the page.
  const raster = [];
  let droppedLinks = 0;
  let last;
  const ocr = hidden(doc, pageIndex, info, text, objects.images);
  const drawnAt = new Map(); // char:baseline -> 0.6pt cell -> x of the glyph kept
  // Right-to-left text needs Word's bidi layout, which a page of it gets by
  // review, not conversion; a few words of it in a left-to-right page (a
  // name in Arabic in a Georgian article) keep their look as images.
  const bidi = (c) =>
    (c >= 0x5_90 && c <= 0x8_ff) ||
    (c >= 0x20_2a && c <= 0x20_2e) ||
    (c >= 0x20_66 && c <= 0x20_69);
  let ink = 0;
  let rtl = 0;
  for (let i = 0; i < text.count; i++) {
    ink += text.codepoints[i] > 32 ? 1 : 0;
    rtl += bidi(text.codepoints[i]) ? 1 : 0;
  }
  if (!ocr && rtl > 0.1 * ink) {
    throw new Error(
      `page ${pageIndex + 1}: bidirectional text needs manual review`
    );
  }
  for (let i = 0; i < (ocr ? 0 : text.count); i++) {
    const tilt = tilted(text.angles[i]);
    if (
      text.flags[i] & 2 ||
      bidi(text.codepoints[i]) ||
      text.codepoints[i] === 0xff_fd ||
      pictures[text.fontIds[i]] ||
      (tilt && !level(text, i))
    ) {
      const [x, y, width, height] = four(text.boxes, 4 * i);
      // Clipped first: a glyph box miles off the page would span millions
      // of cluster grid cells.
      const box = clip(info, { x, y, width, height });
      if (box) {
        raster.push(box);
      }
      last = undefined;
      continue;
    }
    let value = String.fromCodePoint(text.codepoints[i]);
    // Word's PDF export writes a displayed optional hyphen as U+0002.
    if (text.codepoints[i] === 2) {
      value = "\u00ad";
    } else if (unsafe(text.codepoints[i])) {
      continue;
    }
    if (value === "\n" || value === "\r") {
      last = undefined;
      continue;
    }
    if (text.flags[i] & 1) {
      continue;
    }
    const [boxX, y, boxWidth, height] = four(text.boxes, 4 * i);
    let x = boxX;
    let width = boxWidth;
    if (!value.trim() && (!last || width <= 0)) {
      continue;
    }
    // A glyph drawn again where it already is (text split into pieces
    // that overlap, faux bold drawn twice) is one glyph: Word wrote it
    // twice ("archite ectural", "thhesis").
    if (value.trim()) {
      // Kept glyphs are 0.6pt apart, so a 0.6pt cell holds at most one:
      // three lookups, however many of this character share the line.
      const key = `${value}:${Math.round(y)}`;
      let cells = drawnAt.get(key);
      if (!cells) {
        drawnAt.set(key, (cells = new Map()));
      }
      const cell = Math.floor(boxX / 0.6);
      const near = (c) => Math.abs((cells.get(c) ?? Infinity) - boxX) < 0.6;
      if (near(cell - 1) || near(cell) || near(cell + 1)) {
        continue;
      }
      cells.set(cell, boxX);
    }
    const run = text.runIds[i];
    // A nonspacing mark (a Thai tone mark, a Devanagari vowel sign above)
    // drawn on the glyph before it is part of that glyph: split off, Word
    // drew it alone on a dotted circle and broke the run in two.
    if (
      last &&
      /^\p{Mn}+$/u.test(value) &&
      boxX >= last.x - 0.5 &&
      boxX < last.x + last.width
    ) {
      last.text += value;
      continue;
    }
    // A ligature's letters ("f", "i" of "ﬁ") share one glyph's box: one
    // glyph, its text both letters.
    if (
      last &&
      run >= 0 &&
      run === text.runIds[i - 1] &&
      boxX === text.boxes[4 * (i - 1)] &&
      boxWidth === text.boxes[4 * (i - 1) + 2]
    ) {
      last.text += value;
      continue;
    }
    if (run >= 0) {
      // A slant drawn with the text matrix (faux italic) widens each box
      // by its height times the shear; the advance is the box less that.
      // Up in text space is (c, d) in the viewer frame, where up is -y.
      const [a, b, c, d] = four(text.runs.matrices, 6 * run);
      const shear = d < 0 && Math.abs(b) < 1e-3 * Math.abs(a) ? c / -d : 0;
      if (Math.abs(shear) > 0.01) {
        if (shear < 0) {
          x += height * -shear; // leaning left: the box starts at the top
        }
        width = Math.max(0, width - height * Math.abs(shear));
      }
    }
    const font = fonts[text.fontIds[i]];
    const baseline = run >= 0 ? text.runs.origins[run * 2 + 1] : y + height;
    const target =
      links.length > 0 &&
      links.find(
        ({ box }) =>
          x + width / 2 >= box.x &&
          x + width / 2 <= box.x + box.width &&
          y + height / 2 >= box.y &&
          y + height / 2 <= box.y + box.height
      )?.target;
    let link = "";
    if (target?.kind === "uri") {
      // Only web and mail links become Word hyperlinks; others keep their text.
      if (/^(https?:|mailto:)/i.test(target.uri)) {
        link = target.uri;
      } else {
        droppedLinks++;
      }
    } else if (target?.kind === "page" && target.destination.page >= 0) {
      link = `#page${target.destination.page + 1}`;
    } else if (target && target.kind !== "none") {
      droppedLinks++;
    }
    const fill = text.fills[i] >>> 8;
    const color = fill.toString(16).padStart(6, "0").toUpperCase();
    const size = text.fontSizes[i];
    last = {
      text: value,
      x,
      y,
      width,
      height,
      baseline,
      size,
      font: font?.name ?? "Arial",
      weight: weights[text.fontIds[i]] ?? 400,
      // A slant drawn with the text matrix (faux italic) reads as a small
      // angle on glyphs that still advance along one baseline.
      italic: !!(font?.flags & 64) || tilt,
      serif: !!(font?.flags & 2),
      mono: !!(font?.flags & 1),
      color,
      link,
      // Tz as Word's character scale, in percent (a mirroring negative Tz
      // keeps its width).
      // Word's range (1-600%); a huge Tz would not fit the Go int.
      scale: Math.min(600, Math.round(Math.abs(text.hScales[i]) * 100) || 0),
    };
    glyphs.push(last);
  }
  const sameStyle = (a, b) =>
    a &&
    b &&
    a.font === b.font &&
    Math.abs(a.size - b.size) < 0.01 &&
    Math.abs(a.baseline - b.baseline) < 0.2 &&
    a.color === b.color &&
    a.link === b.link &&
    a.weight === b.weight &&
    a.italic === b.italic &&
    a.scale === b.scale;
  // A glyph the next one starts well inside advances only to it: CJK
  // punctuation set half width ("）、") kept its full em box, and Word drew
  // it full width and wrapped the line. Not a space (a page number drawn
  // over one split its paragraph), an overprint (faux bold) or a combining
  // mark, which start near the glyph's own x.
  for (let i = 0; i + 1 < glyphs.length; i++) {
    const g = glyphs[i],
      next = glyphs[i + 1];
    if (
      g.text.trim() &&
      next.x - (g.x + g.width) < -Math.max(0.5, g.size * 0.12) &&
      next.x - g.x >= 0.4 * g.width &&
      Math.abs(next.baseline - g.baseline) < 0.2 &&
      !/\p{M}/u.test(next.text)
    ) {
      g.width = next.x - g.x;
    }
  }
  // Boxes carry advances, not tight ink widths. Keep the extra source
  // advance as ordinary Word tracking, which survives edits and rendering.
  for (let i = 0; i < glyphs.length; i++) {
    const g = glyphs[i],
      next = glyphs[i + 1],
      previous = glyphs[i - 1];
    const gap = next ? next.x - (g.x + g.width) : Infinity;
    g.tracking =
      sameStyle(g, next) && Math.abs(gap) <= Math.max(0.5, g.size * 0.12)
        ? gap
        : sameStyle(previous, g)
          ? previous.tracking
          : 0;
  }
  const spans = [];
  last = undefined;
  let advance = 0; // last's glyph advances and glyph count
  let count = 0;
  let tabbed; // the baseline of a tab just drawn
  for (const g of glyphs) {
    // A drawn tab ends its span: the next text starts at its own x, which
    // Word reaches with a tab stop (a tab character would go to Word's
    // default stops instead).
    if (g.text === "\t") {
      tabbed = g.baseline;
      last = undefined;
      continue;
    }
    // A drawn space belongs to the span before it on the same line, even
    // across a style change: a span of only a space would be dropped, and
    // overlapping slanted boxes leave no gap to infer it from.
    const space =
      last && !g.text.trim() && Math.abs(last.baseline - g.baseline) < 0.5;
    // One average tracking must place each glyph near its source x;
    // producers that nudge every glyph otherwise yield a run per letter.
    const at = last && last.x + advance + count * last.tracking;
    if (
      space ||
      (sameStyle(last, g) && Math.abs(g.x - at) <= Math.max(0.3, g.size * 0.04))
    ) {
      if (!space) {
        last.tracking = (g.x - last.x - advance) / count;
      }
      last.text += g.text;
      last.width = Math.max(last.width, g.x + g.width - last.x);
      if (g.text.trim()) {
        last.ink = g.x + g.width - last.x;
      }
      advance += g.width;
      count++;
    } else {
      const tab = tabbed !== undefined && Math.abs(tabbed - g.baseline) < 0.5;
      // ink: the width to the last glyph that isn't a space, whose
      // advance a justified line stretches.
      last = { ...g, tab, ink: g.text.trim() ? g.width : 0 };
      advance = g.width;
      count = 1;
      spans.push(last);
    }
    tabbed = undefined;
  }
  const segments = [];
  const { paths } = objects;
  const { points } = objects.segments;
  for (let i = 0; i < pathCount; i++) {
    const [boxX, boxY, pathWidth, pathHeight] = four(paths.boxes, i * 4);
    // Filled backgrounds are not table borders. Thin filled rectangles are.
    if (
      !(objects.paths.flags[i] & 2) &&
      Math.min(pathWidth, pathHeight) > 1.5
    ) {
      continue;
    }
    let origin, previous;
    const begin = objects.paths.start[i];
    const end = objects.paths.start[i + 1];
    // A stroked path's box takes in its stroke past the outline: across a
    // horizontal rule it is the y inset, across a vertical the x (a rule
    // scaled along its length keeps its thickness).
    let insetX = 0;
    let insetY = 0;
    if (objects.paths.flags[i] & 2) {
      let minX = Infinity;
      let minY = Infinity;
      for (let j = begin; j < end; j++) {
        minX = Math.min(minX, objects.segments.points[2 * j]);
        minY = Math.min(minY, objects.segments.points[2 * j + 1]);
      }
      insetX = Math.min(Math.max(minX - boxX, 0), 20);
      insetY = Math.min(Math.max(minY - boxY, 0), 20);
    }
    // Only rules the writer reads: horizontals of an underline's length
    // (2pt) and verticals of a cell wall's (10pt). A label's curves came
    // as 168,000 sub-point pieces, 20 MB of JSON nothing read.
    const add = (a, b) => {
      const dx = Math.abs(a?.[0] - b?.[0]);
      const dy = Math.abs(a?.[1] - b?.[1]);
      if (dy < 0.5 && dx >= 2) {
        segments.push({
          x1: a[0],
          y1: a[1],
          x2: b[0],
          y2: b[1],
          width: insetY,
        });
      } else if (dx < 0.5 && dy > 10) {
        segments.push({
          x1: a[0],
          y1: a[1],
          x2: b[0],
          y2: b[1],
          width: insetX,
        });
      }
    };
    for (let j = begin; j < end; j++) {
      const point = [points[2 * j], points[2 * j + 1]];
      const kind = objects.segments.kinds[j];
      if ((kind & 3) === 2) {
        origin = point;
      } else if ((kind & 3) === 0) {
        add(previous, point);
      }
      previous = point;
      if (kind & 4) {
        add(previous, origin);
      }
    }
  }
  const images = [];
  const addImage = (b, withText = false) => {
    const box = clip(info, b);
    if (box) {
      images.push({ ...box, text: withText });
    }
  };
  const placed = [];
  for (let i = 0; i < objects.images.count; i++) {
    const [x, y, width, height] = four(objects.images.boxes, 4 * i);
    if (width > 0 && height > 0) {
      placed.push({ x, y, width, height });
    }
  }
  placed.sort((a, b) => b.width * b.height - a.width * a.height);
  let leftOut = Math.max(0, placed.length - 60);
  const kept = [];
  for (const box of placed.slice(0, 60)) {
    const { x, y, width, height } = box;
    // A region renders everything drawn in it: an image inside one already
    // kept (a stack of the same picture) is in that render.
    if (
      kept.some(
        (k) =>
          x >= k.x - 1 &&
          y >= k.y - 1 &&
          x + width <= k.x + k.width + 1 &&
          y + height <= k.y + k.height + 1
      )
    ) {
      continue;
    }
    kept.push(box);
    addImage(box);
  }
  // Vector art keeps its look as an image rendered without the page's
  // text: rules, logos, signature lines, and fills under text (shaded
  // cells, panels). Thin art under text is left out: ruled tables and
  // underlines are rebuilt in Word from their segments.
  // A page fill, or a panel behind all of the page's text, is background
  // (usually white), not art: unless light text stands on no smaller fill,
  // which only a dark page fill shows (white on a navy cover came out white
  // on white). White text in a shaded header cell has its cell.
  const panels = [];
  for (let i = 0; i < pathCount; i++) {
    const [x, y, w, h] = four(objects.paths.boxes, 4 * i);
    if (objects.paths.flags[i] & 1 && w * h < 0.5 * info.width * info.height) {
      panels.push({ x, y, w, h });
    }
  }
  const light = spans.some((s) => {
    const c = Number.parseInt(s.color, 16);
    const cx = s.x + s.width / 2;
    const cy = s.y + s.height / 2;
    return (
      s.text.trim() &&
      0.299 * (c >> 16) + 0.587 * ((c >> 8) & 255) + 0.114 * (c & 255) > 200 &&
      !panels.some(
        (b) => cx >= b.x && cx <= b.x + b.w && cy >= b.y && cy <= b.y + b.h
      )
    );
  });
  const backdrop = (b) =>
    !light &&
    ((b.width >= info.width * 0.98 && b.height >= info.height * 0.98) ||
      (b.width * b.height >= info.width * info.height * 0.5 &&
        spans.some((s) => s.text.trim()) &&
        spans.every(
          (s) =>
            !s.text.trim() ||
            (s.x >= b.x - 1 &&
              s.y >= b.y - 1 &&
              s.x + s.width <= b.x + b.width + 1 &&
              s.y + s.height <= b.y + b.height + 1)
        )));
  let dense = false;
  const boxes = Array.from({ length: pathCount }, (_, i) => {
    const [x0, y0, w0, h0] = four(objects.paths.boxes, 4 * i);
    // Clip to the page: a fill drawn far past the edge is a backdrop.
    const x = Math.max(x0, 0);
    const y = Math.max(y0, 0);
    const width = Math.min(x0 + w0, info.width) - x;
    const height = Math.min(y0 + h0, info.height) - y;
    // Thin paths are rules: table borders and underlines, rebuilt in Word.
    const thick =
      Math.min(width, height) > 1.5 && !!(objects.paths.flags[i] & 1);
    return {
      x,
      y,
      width: Math.max(width, 0.5),
      height: Math.max(height, 0.5),
      thick,
    };
  }).filter((b) => !backdrop(b));
  // Past the path limit (a map of 20,000 dots), or too dense to cluster
  // (an engineering drawing), the drawings are too many to place one by
  // one: the area they cover is kept as one image, drawn without the
  // page's text, rather than left out.
  let art = pathCount < objects.paths.count ? null : clusters(boxes, 2);
  if (!art) {
    let union;
    for (let i = 0; i < objects.paths.count; i++) {
      const [x0, y0, w0, h0] = four(objects.paths.boxes, 4 * i);
      const b = clip(info, { x: x0, y: y0, width: w0, height: h0 });
      if (!b || backdrop(b)) {
        continue;
      }
      union = union
        ? {
            x: Math.min(union.x, b.x),
            y: Math.min(union.y, b.y),
            width:
              Math.max(union.x + union.width, b.x + b.width) -
              Math.min(union.x, b.x),
            height:
              Math.max(union.y + union.height, b.y + b.height) -
              Math.min(union.y, b.y),
          }
        : b;
    }
    art = union ? [{ ...union, thick: true }] : [];
  }
  const underText = (b) =>
    spans.some(
      (s) =>
        s.text.trim() &&
        Math.min(s.x + s.width, b.x + b.width) > Math.max(s.x, b.x) &&
        Math.min(s.y + s.height, b.y + b.height) > Math.max(s.y, b.y)
    );
  let skippedArt = false;
  const drawn = (list) => list.filter((b) => b.thick || !underText(b));
  art = drawn(art);
  const artRoom = Math.min(50, 100 - images.length);
  // A map of thousands of dots is more drawings than images a page holds:
  // nearby drawings merge into fewer, larger images (each still drawn
  // without the page's text) before any is left out.
  for (let pad = 8; art.length > artRoom && pad <= 128; pad *= 4) {
    const merged = clusters(boxes, pad);
    if (!merged) {
      break;
    }
    art = drawn(merged);
  }
  if (art.length > artRoom) {
    art.sort((a, b) => b.width * b.height - a.width * a.height);
    art.length = artRoom;
    skippedArt = true;
  }
  for (const b of art) {
    addImage(b);
  }
  // Rules already in an art image are not drawn again as Word lines.
  for (const s of segments) {
    const mx = (s.x1 + s.x2) / 2;
    const my = (s.y1 + s.y2) / 2;
    s.painted = art.some(
      (b) =>
        mx >= b.x - 1 &&
        mx <= b.x + b.width + 1 &&
        my >= b.y - 1 &&
        my <= b.y + b.height + 1
    );
  }
  inked(doc, pageIndex, info, segments);
  const regions = clusters(raster, 2) ?? ((dense = true), []);
  leftOut += Math.max(0, images.length + regions.length - 100);
  for (const r of regions.slice(0, Math.max(0, 100 - images.length))) {
    addImage(r, true);
  }
  const crowded = leftOut > 0 || dense || skippedArt;
  const warnings = [];
  if (crowded) {
    warnings.push(
      "Page too complex to keep every image and drawing: the smallest were left out"
    );
  }
  if (raster.length) {
    warnings.push(
      `${raster.length} rotated or unmapped characters kept as images, not editable text`
    );
  }
  if (droppedLinks) {
    warnings.push(
      "Links other than web, mail and page links keep their text without the link"
    );
  }
  if (
    spans.some((s) => /[\u3040-\u30ff\u3400-\u9fff\uac00-\ud7af]/u.test(s.text))
  ) {
    warnings.push("CJK text and font fidelity require manual review");
  }
  // An annotation's appearance (a stamped signature, a filled field) keeps
  // its look as an image: without the page's text, unless the appearance
  // draws text of its own.
  const shown = annotations.filter(
    (a) => a.subtype !== 2 && a.appearanceObjects > 0
  );
  for (const a of shown.slice(0, Math.max(0, 100 - images.length))) {
    addImage(a.box, a.appearanceTextObjects > 0);
  }
  if (annotations.some((a) => a.subtype !== 2)) {
    warnings.push(
      "PDF annotations and form fields are kept as images, not Word comments or fields"
    );
  }
  if (
    objects.images.count &&
    !raster.length &&
    !spans.some((s) => s.text.trim())
  ) {
    // A scan in a document with text keeps its look; all scans is a refusal.
    // Not a page of unmapped characters: their images carry its text, and a
    // page image renders without it (a printed web article came out blank).
    // A scan with a hidden OCR layer keeps its look too, and counts as text.
    images.splice(0);
    addImage({ x: 0, y: 0, width: info.width, height: info.height });
    if (ocr) {
      warnings.push(
        "Scanned page kept as an image: its hidden OCR text is not editable text"
      );
    } else {
      warnings.push(
        "Scanned page kept as an image: no editable text without OCR"
      );
      scan = true;
    }
  }
  return { page: { info, pageIndex, spans, segments, images, warnings }, scan };
}

// finish renders a built page's images: the page's line of output.
function finish(doc, { info, pageIndex, images, warnings, ...rest }) {
  const rendered = pageImages(doc, pageIndex, info, images);
  // Past Recto's render limits (more than 32 knockout groups) the rest
  // draw as if isolated: close, not exact.
  if (rendered.some((r) => r.truncated)) {
    warnings.push("Some images are approximate: too many layered effects");
  }
  return {
    width: info.width,
    height: info.height,
    ...rest,
    warnings,
    images: rendered.map(({ truncated: _, ...image }) => image),
  };
}

// A page and, unless it's a scan, its line of output.
function readPage(doc, pageIndex) {
  const built = build(doc, pageIndex);
  return built.scan ? built : { ...built, out: finish(doc, built.page) };
}

// Pages are independent: past a few, worker threads (each with its own
// Recto) read and render them while this thread writes them in order. A
// page is read at most a few ahead of the one written, so memory stays
// bounded.
async function main() {
  const doc = await openPdf(new Uint8Array(fs.readFileSync(file))).catch(
    (error) => {
      process.stderr.write(`${error?.message ?? error}\n`); // e.g. needs a password
      process.exit(1);
    }
  );
  const workers = [];
  try {
    if (doc.pageCount < 1 || doc.pageCount > Number(maxPagesString)) {
      throw new Error(`page limit: ${doc.pageCount}`);
    }
    if (Object.values(doc.structureTruncated()).some(Boolean)) {
      throw new Error("PDF structure truncated");
    }
    // A thread holds its own Recto and pages: a heavy document's peak went
    // from 413 to 806 MB on four. One per GiB the process may still use
    // (its cgroup's, in a VM): a 1 GiB VM reads serially.
    const threads = Math.min(
      4,
      os.availableParallelism() - 1,
      Math.floor(doc.pageCount / 4),
      Math.floor(process.availableMemory() / 2 ** 30)
    );
    const code = process.execArgv[process.execArgv.indexOf("--eval") + 1];
    // One thread would only wait on: two or none.
    for (let i = 0; i < threads && threads > 1 && code; i++) {
      workers.push(thread(code));
    }
    if (workers.length) {
      doc.close(); // the threads read their own: give this one's memory back
    }
    const on = (i) => workers[i % workers.length];
    const read = workers.length
      ? (i) => on(i).ask({ page: i })
      : (i) => readPage(doc, i);
    const render = async (built) =>
      workers.length
        ? (await on(built.pageIndex).ask({ finish: built })).out
        : finish(doc, built);
    const ahead = 2 * Math.max(1, workers.length);
    const queue = [];
    const next = (i) => {
      if (i < doc.pageCount) {
        const p = Promise.resolve().then(() => read(i));
        p.catch(() => {}); // awaited in order below
        queue.push(p);
      }
    };
    for (let i = 0; i < ahead; i++) {
      next(i);
    }
    // Rendering and PNG encoding dominate conversion time, so a scan is
    // rendered only once a page with text follows: an all-scan document is
    // refused without encoding a page.
    let scans = 0;
    const waiting = [];
    for (let pageIndex = 0; pageIndex < doc.pageCount; pageIndex++) {
      const built = await queue.shift();
      next(pageIndex + ahead);
      scans += built.scan ? 1 : 0;
      if (scans === pageIndex + 1) {
        waiting.push(built.page); // all scans so far: maybe a refusal
        continue;
      }
      for (const pending of waiting.splice(0)) {
        say(await render(pending));
      }
      say(built.out ?? (await render(built.page)));
    }
    if (scans === doc.pageCount) {
      throw new Error("scanned document: every page is an image; OCR required");
    }
    // Damage (an object the xref places where none is) surfaces while pages
    // read, on whichever page first touches it in a given Recto: so it is
    // asked of every Recto once all are read, and reported for the document.
    const all = await Promise.all(workers.map((w) => w.ask({ damaged: 1 })));
    if (all.some((r) => r.damaged) || (!all.length && doc.damaged())) {
      say({ damaged: true });
    }
  } catch (error) {
    process.stderr.write(`${error?.message ?? error}\n`);
    process.exitCode = 1;
  } finally {
    if (!workers.length) {
      doc.close();
    }
    for (const w of workers) {
      w.worker.terminate();
    }
  }
}

// thread starts a worker running this reader; ask sends it one request,
// answered in order.
function thread(code) {
  const worker = new Worker(
    new URL(`data:text/javascript,${encodeURIComponent(code)}`),
    {
      workerData: [file, entry, maxPagesString],
      resourceLimits: { maxOldGenerationSizeMb: 512 },
    }
  );
  const pending = [];
  const fail = (error) => {
    for (const { reject } of pending.splice(0)) {
      reject(error);
    }
  };
  worker.on("message", (reply) => {
    const { resolve, reject } = pending.shift();
    if (reply.error) {
      reject(new Error(reply.error));
    } else {
      resolve(reply);
    }
  });
  worker.on("error", fail);
  worker.on("exit", () => fail(new Error("reader thread exited")));
  return {
    worker,
    ask: (request) =>
      new Promise((resolve, reject) => {
        pending.push({ resolve, reject });
        worker.postMessage(request);
      }),
  };
}

// serve answers the main thread's requests, in order, on its own document.
async function serve() {
  const doc = await openPdf(new Uint8Array(fs.readFileSync(file)));
  parentPort.on("message", (request) => {
    try {
      parentPort.postMessage(
        request.finish
          ? { out: finish(doc, request.finish) }
          : request.damaged
            ? { damaged: doc.damaged() }
            : readPage(doc, request.page)
      );
    } catch (error) {
      parentPort.postMessage({ error: `${error?.message ?? error}` });
    }
  });
}

await (isMainThread ? main() : serve());
