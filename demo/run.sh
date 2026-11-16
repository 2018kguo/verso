#!/bin/sh
# Rebuild the demo: Word exports each source to PDF, three converters turn the
# PDF back into .docx, Word renders each result, and compose.py lays page 1 of
# each side by side. macOS + Microsoft Word; pdf2docx ($PDF2DOCX) and soffice.
#
#   VALIDATOR=path/to/verso-validate.dll demo/run.sh
set -eu
D=$(cd "$(dirname "$0")" && pwd); R=$(dirname "$D")
box=$(mktemp -d "$HOME/Library/Containers/com.microsoft.Word/Data/verso-demo-XXXX")
trap 'rm -rf "$box"' EXIT

word_pdf() { # DOCX PDF: Word opens the file and saves it as PDF
  cp "$1" "$box/in.docx"; rm -f "$box/out.pdf"
  osascript - "$box/in.docx" "$box/out.pdf" <<'EOF'
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
  cp "$box/out.pdf" "$2"
}

valid() { dotnet "$VALIDATOR" "$1" >/dev/null 2>&1 && echo valid || echo INVALID; }

(cd "$R" && go build -o "$box/verso" ./cmd/verso)
python3 "$D/make_sources.py" "$box/src"
mkdir -p "$D/pdf"
for src in "$box"/src/*.docx; do
  n=$(basename "$src" .docx); w="$box/$n"; mkdir -p "$w"
  word_pdf "$src" "$D/pdf/$n.pdf"
  "$box/verso" --allow-warnings "$D/pdf/$n.pdf" "$w/verso.docx" >/dev/null
  ${PDF2DOCX:-pdf2docx} convert "$D/pdf/$n.pdf" "$w/pdf2docx.docx" >/dev/null 2>&1
  soffice --headless --infilter=writer_pdf_import --convert-to 'docx:MS Word 2007 XML' --outdir "$w" "$D/pdf/$n.pdf" >/dev/null 2>&1
  mv "$w/$n.docx" "$w/libreoffice.docx"
  pdftoppm -r 100 -f 1 -l 1 -png -singlefile "$D/pdf/$n.pdf" "$w/source"
  panels=""
  for c in verso pdf2docx libreoffice; do
    word_pdf "$w/$c.docx" "$w/$c.pdf"
    pdftoppm -r 100 -f 1 -l 1 -png -singlefile "$w/$c.pdf" "$w/$c"
    echo "$n $c: $(valid "$w/$c.docx"), $(pdfinfo "$w/$c.pdf" | awk '/^Pages/{print $2}') pages (source $(pdfinfo "$D/pdf/$n.pdf" | awk '/^Pages/{print $2}'))"
    panels="$panels $c=$w/$c.png"
  done
  python3 "$D/compose.py" "$D/$n.png" "$w/source.png" $panels
done
