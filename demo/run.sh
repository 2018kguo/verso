#!/bin/sh
# Rebuild the demo from sources.tsv: download each PDF (checking its hash),
# convert it with verso, pdf2docx and LibreOffice, have Microsoft Word render
# every result, score each against the source with Ink F1, and lay one page
# side by side. Writes demo/NAME.png and demo/results.tsv.
#
#   VALIDATOR=path/to/verso-validate.dll PDF2DOCX=path/to/pdf2docx [KEEP=dir] demo/run.sh [NAME...]
#
# SOURCES (a sources.tsv), OUT (where images and results go) and CONVERTERS
# ("verso" alone is fastest) point it at another set of PDFs.
#
# macOS with Microsoft Word, LibreOffice (soffice), Poppler and Pillow. The
# source PDFs are not part of this repository; see sources.tsv for licenses.
set -eu
D=$(cd "$(dirname "$0")" && pwd); R=$(dirname "$D")
box=$(mktemp -d "$HOME/Library/Containers/com.microsoft.Word/Data/verso-demo-XXXX")
trap 'rm -rf "$box"' EXIT

word_pdf() { # DOCX PDF: Word opens the file and saves it as PDF (5 minutes at most)
  cp "$1" "$box/in.docx"; rm -f "$box/out.pdf"
  perl -e 'alarm 300; exec @ARGV' osascript - "$box/in.docx" "$box/out.pdf" <<'EOF' || osascript -e 'tell application "Microsoft Word" to close every document saving no' || true
on run argv
  tell application "Microsoft Word"
    launch
    set n to count of documents
    open (POSIX file (item 1 of argv))
    repeat 240 times
      if (count of documents) > n then exit repeat
      delay 0.25
    end repeat
    set d to document 1
    save as d file name (item 2 of argv) file format format PDF
    close d saving no
  end tell
end run
EOF
  [ -f "$box/out.pdf" ] && cp "$box/out.pdf" "$2"
}

valid() { dotnet "$VALIDATOR" "$1" >/dev/null 2>&1 && echo valid || echo invalid; }
pages() { pdfinfo "$1" 2>/dev/null | awk '/^Pages/{print $2}'; }

(cd "$R" && go build -o "$box/verso" ./cmd/verso)
S=${SOURCES:-$D/sources.tsv}; O=${OUT:-$D}; mkdir -p "$O"
results="$O/results.tsv"
[ $# -eq 0 ] && printf 'name\tconverter\tvalid\tpages\tsource pages\tink F1\n' > "$results"
while IFS="$(printf '\t')" read -r n page sha url license <&3; do
  [ "$n" = name ] && continue
  [ $# -gt 0 ] && ! echo " $* " | grep -q " $n " && continue
  w="$box/$n"; mkdir -p "$w/source"
  curl -fsSL -A "Mozilla/5.0" -o "$w/in.pdf" "$url"
  [ "$sha" = - ] || echo "$sha  $w/in.pdf" | shasum -a 256 -c --quiet
  pdftoppm -r 50 -png "$w/in.pdf" "$w/source/p"
  pdftoppm -r 100 -f "$page" -l "$page" -png -singlefile "$w/in.pdf" "$w/source-page"
  want() { echo " ${CONVERTERS:-verso pdf2docx libreoffice} " | grep -q " $1 "; }
  want verso && { "$box/verso" --allow-warnings "$w/in.pdf" "$w/verso.docx" >/dev/null || true; }
  want pdf2docx && { ${PDF2DOCX:-pdf2docx} convert "$w/in.pdf" "$w/pdf2docx.docx" >/dev/null 2>&1 || true; }
  if want libreoffice; then
    soffice --headless --infilter=writer_pdf_import --convert-to 'docx:MS Word 2007 XML' --outdir "$w" "$w/in.pdf" >/dev/null 2>&1 || true
    [ -f "$w/in.docx" ] && mv "$w/in.docx" "$w/libreoffice.docx"
  fi
  panels=""; nl='
'
  for c in ${CONVERTERS:-verso pdf2docx libreoffice}; do
    v=failed; np=0; score=0.000
    if [ -f "$w/$c.docx" ]; then
      v=$(valid "$w/$c.docx")
      word_pdf "$w/$c.docx" "$w/$c.pdf" || true
      if [ -f "$w/$c.pdf" ]; then
        mkdir -p "$w/$c"
        pdftoppm -r 50 -png "$w/$c.pdf" "$w/$c/p"
        pdftoppm -r 100 -f "$page" -l "$page" -png -singlefile "$w/$c.pdf" "$w/$c-page" 2>/dev/null || true
        np=$(pages "$w/$c.pdf"); score=$(python3 "$D/compose.py" score "$w/source" "$w/$c" | cut -d' ' -f1)
      fi
    fi
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$n" "$c" "$v" "$np" "$(pages "$w/in.pdf")" "$score" | tee -a "$results"
    panels="$panels$c · $v · Ink F1 $score=$w/$c-page.png$nl"
  done
  # Labels carry spaces: split the panel list on newlines only.
  (IFS=$nl; python3 "$D/compose.py" sheet "$O/$n.png" "$w/source-page.png" $panels)
  [ -n "${KEEP:-}" ] && cp -R "$w" "$KEEP/" # the PDFs, documents and renders, for a closer look
done 3<"$S"
