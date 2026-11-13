// Geometry only. Layout and all OOXML generation belong to the Go converter.
import fs from "node:fs";
import { pathToFileURL } from "node:url";
import { crc32, deflateSync } from "node:zlib";

const [file, entry, maxPagesString] = process.argv.slice(1);
const { openPdf } = await import(pathToFileURL(entry).href);
const doc = await openPdf(new Uint8Array(fs.readFileSync(file))).catch(
  (error) => {
    process.stderr.write(`${error?.message ?? error}\n`); // e.g. needs a password
    process.exit(1);
  }
);
const say = (value) => fs.writeSync(1, `${JSON.stringify(value)}\n`);

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
  const rows = Buffer.alloc(height * stride);
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
    crc.writeUInt32BE(crc32(Buffer.concat([type, data])));
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

try {
  if (doc.pageCount < 1 || doc.pageCount > Number(maxPagesString)) {
    throw new Error(`page limit: ${doc.pageCount}`);
  }
  if (Object.values(doc.structureTruncated()).some(Boolean)) {
    throw new Error("PDF structure truncated");
  }
  let scans = 0;
  // Rendering and PNG encoding dominate conversion time, so a page is encoded
  // only when emitted, and scans wait while every page so far is one: an
  // all-scan document is refused without encoding a page.
  const waiting = [];
  const emit = ({ info, pageIndex, images, warnings, ...rest }) => {
    const rendered = pageImages(doc, pageIndex, info, images);
    // Past Recto's render limits (more than 32 knockout groups) the rest
    // draw as if isolated: close, not exact.
    if (rendered.some((r) => r.truncated)) {
      warnings.push("Some images are approximate: too many layered effects");
    }
    say({
      width: info.width,
      height: info.height,
      ...rest,
      warnings,
      images: rendered.map(({ truncated, ...image }) => image),
    });
  };
  for (let pageIndex = 0; pageIndex < doc.pageCount; pageIndex++) {
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
    const links = doc.links(pageIndex);
    const glyphs = [];
    // Glyphs Word cannot carry as text (no Unicode, or rotated) keep their
    // look as rendered images of their boxes instead of refusing the page.
    const raster = [];
    let droppedLinks = 0;
    let last;
    for (let i = 0; i < text.count; i++) {
      if (
        text.flags[i] & 2 ||
        text.codepoints[i] === 0xff_fd ||
        (tilted(text.angles[i]) && !level(text, i))
      ) {
        const [x, y, width, height] = text.boxes.subarray(4 * i, 4 * i + 4);
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
      if (/[\u0590-\u08ff\u202a-\u202e\u2066-\u2069]/u.test(value)) {
        throw new Error(
          `page ${pageIndex + 1}: bidirectional text needs manual review`
        );
      }
      if (value === "\n" || value === "\r") {
        last = undefined;
        continue;
      }
      if (text.flags[i] & 1) {
        continue;
      }
      const [boxX, y, boxWidth, height] = text.boxes.subarray(4 * i, 4 * i + 4);
      let x = boxX;
      let width = boxWidth;
      if (!value.trim() && (!last || width <= 0)) {
        continue;
      }
      const run = text.runIds[i];
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
        const [a, b, c, d] = text.runs.matrices.subarray(6 * run, 6 * run + 4);
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
      const target = links.find(
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
        weight: font?.weight ?? 400,
        // A slant drawn with the text matrix (faux italic) reads as a small
        // angle on glyphs that still advance along one baseline.
        italic: !!(font?.flags & 64) || tilted(text.angles[i]),
        serif: !!(font?.flags & 2),
        mono: !!(font?.flags & 1),
        color,
        link,
        // Tz as Word's character scale, in percent (a mirroring negative Tz
        // keeps its width).
        scale: Math.round(Math.abs(text.hScales[i]) * 100),
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
      const at = last && last.x + last.advance + last.count * last.tracking;
      if (
        space ||
        (sameStyle(last, g) &&
          Math.abs(g.x - at) <= Math.max(0.3, g.size * 0.04))
      ) {
        if (!space) {
          last.tracking = (g.x - last.x - last.advance) / last.count;
        }
        last.text += g.text;
        last.width = Math.max(last.width, g.x + g.width - last.x);
        last.advance += g.width;
        last.count++;
      } else {
        const tab = tabbed !== undefined && Math.abs(tabbed - g.baseline) < 0.5;
        last = { ...g, advance: g.width, count: 1, tab };
        spans.push(last);
      }
      tabbed = undefined;
    }
    for (const s of spans) {
      delete s.advance;
      delete s.count;
    }
    const segments = [];
    for (let i = 0; i < pathCount; i++) {
      const [, , pathWidth, pathHeight] = objects.paths.boxes.subarray(
        i * 4,
        i * 4 + 4
      );
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
      const add = (a, b) => {
        if (
          a &&
          b &&
          (Math.abs(a[0] - b[0]) < 0.5 || Math.abs(a[1] - b[1]) < 0.5)
        ) {
          segments.push({ x1: a[0], y1: a[1], x2: b[0], y2: b[1] });
        }
      };
      for (let j = begin; j < end; j++) {
        const point = Array.from(
          objects.segments.points.subarray(2 * j, 2 * j + 2)
        );
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
      const [x, y, width, height] = objects.images.boxes.subarray(
        4 * i,
        4 * i + 4
      );
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
    // (usually white), not art.
    const backdrop = (b) =>
      (b.width >= info.width * 0.98 && b.height >= info.height * 0.98) ||
      (b.width * b.height >= info.width * info.height * 0.5 &&
        spans.some((s) => s.text.trim()) &&
        spans.every(
          (s) =>
            !s.text.trim() ||
            (s.x >= b.x - 1 &&
              s.y >= b.y - 1 &&
              s.x + s.width <= b.x + b.width + 1 &&
              s.y + s.height <= b.y + b.height + 1)
        ));
    let dense = false;
    const art =
      clusters(
        Array.from({ length: pathCount }, (_, i) => {
          const [x0, y0, w0, h0] = objects.paths.boxes.subarray(
            4 * i,
            4 * i + 4
          );
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
        }).filter((b) => !backdrop(b)),
        2
      ) ?? ((dense = true), []);
    const underText = (b) =>
      spans.some(
        (s) =>
          s.text.trim() &&
          Math.min(s.x + s.width, b.x + b.width) > Math.max(s.x, b.x) &&
          Math.min(s.y + s.height, b.y + b.height) > Math.max(s.y, b.y)
      );
    let skippedArt = false;
    art.splice(0, art.length, ...art.filter((b) => b.thick || !underText(b)));
    const artRoom = Math.min(50, 100 - images.length);
    if (art.length > artRoom) {
      art.sort((a, b) => b.width * b.height - a.width * a.height);
      art.length = artRoom;
      skippedArt = true;
    }
    for (const b of art) {
      addImage(b);
    }
    const regions = clusters(raster, 2) ?? ((dense = true), []);
    leftOut += Math.max(0, images.length + regions.length - 100);
    for (const r of regions.slice(0, Math.max(0, 100 - images.length))) {
      addImage(r, true);
    }
    const crowded =
      leftOut > 0 || dense || skippedArt || pathCount < objects.paths.count;
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
      spans.some((s) =>
        /[\u3040-\u30ff\u3400-\u9fff\uac00-\ud7af]/u.test(s.text)
      )
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
    if (objects.images.count && !spans.some((s) => s.text.trim())) {
      // A scan in a document with text keeps its look; all scans is a refusal.
      images.splice(0);
      addImage({ x: 0, y: 0, width: info.width, height: info.height });
      warnings.push(
        "Scanned page kept as an image: no editable text without OCR"
      );
      scans++;
    }
    const page = { info, pageIndex, spans, segments, images, warnings };
    if (scans === pageIndex + 1) {
      waiting.push(page); // all scans so far: maybe a refusal
      continue;
    }
    for (const pending of waiting.splice(0)) {
      emit(pending);
    }
    emit(page);
  }
  if (scans === doc.pageCount) {
    throw new Error("scanned document: every page is an image; OCR required");
  }
} catch (error) {
  process.stderr.write(`${error?.message ?? error}\n`);
  process.exitCode = 1;
} finally {
  doc.close();
}
