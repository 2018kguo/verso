#!/bin/sh
# LibreOffice's Writer PDF import as a competitor: CONVERTER [FLAGS] INPUT OUTPUT.
# Flags are accepted for CLI parity and ignored. Needs soffice on PATH.
set -eu
while [ $# -gt 2 ]; do shift; done
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
soffice "-env:UserInstallation=file://$work/profile" --headless --infilter=writer_pdf_import \
  --convert-to 'docx:MS Word 2007 XML' --outdir "$work" "$1" >/dev/null
mv "$work/$(basename "${1%.*}").docx" "$2"
echo '{}'
