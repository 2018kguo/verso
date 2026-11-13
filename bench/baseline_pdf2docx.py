#!/usr/bin/env python3
"""Optional pdf2docx 0.5.13 adapter. The benchmark itself has no Python imports.

Preserve rejected raw candidates beside OUTPUT for diagnosis, but publish OUTPUT
only after the exact candidate passes Microsoft's SDK. The outer Go supervisor
owns the deadline/process-tree limits, including this Python process's children.
"""

import argparse
from contextlib import redirect_stdout
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
from xml.etree import ElementTree
from zipfile import ZipFile


def duration(value):
    parts = re.findall(r"(\d+(?:\.\d+)?)(ms|s|m|h)", value)
    if not parts or "".join(n + unit for n, unit in parts) != value:
        raise ValueError("timeout must use ms, s, m or h units")
    return sum(float(n) * {"ms": .001, "s": 1, "m": 60, "h": 3600}[unit] for n, unit in parts)


def deadline(_signal, _frame):
    raise TimeoutError("baseline conversion deadline exceeded")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--validator-dll", required=True)
    parser.add_argument("--max-pages", type=int, default=100)
    parser.add_argument("--timeout", default="2m")
    parser.add_argument("--allow-warnings", action="store_true")
    parser.add_argument("--publish-invalid", action="store_true",
                        help="diagnostic: publish a rejected candidate so the harness can score its fidelity")
    parser.add_argument("--recto", help="accepted for command-line parity; unused by baseline")
    parser.add_argument("input")
    parser.add_argument("output")
    args = parser.parse_args()
    started = time.monotonic()
    report = dict(pages=0, characters=0, tables=0, images=0, warnings=[], validated=False)
    output = Path(args.output)
    candidate = output.with_name("baseline-candidate.docx")
    converter = None
    try:
        seconds = duration(args.timeout)
        if seconds <= 0 or args.max_pages <= 0:
            raise ValueError("timeout and max-pages must be positive")
        signal.signal(signal.SIGALRM, deadline)
        signal.setitimer(signal.ITIMER_REAL, seconds)
        import fitz
        from pdf2docx import Converter

        if output.exists() or candidate.exists():
            raise ValueError("refuse to overwrite existing evidence")
        with fitz.open(args.input) as source:
            report["pages"] = len(source)
            if source.needs_pass:
                raise ValueError("encrypted source requires password")
            if len(source) > args.max_pages:
                raise ValueError("source exceeds max-pages")
            report["characters"] = sum(len(page.get_text()) for page in source)
            report["images"] = sum(len(page.get_images()) for page in source)
        converter = Converter(args.input)
        # Do not suppress the legacy converter's diagnostics: they go to stderr
        # in the wrapper, while stdout remains one machine-readable JSON record.
        with redirect_stdout(sys.stderr):
            converter.convert(str(candidate))
        converter.close()
        converter = None
        check = subprocess.run(
            ["dotnet", str(Path(args.validator_dll).resolve()), str(candidate.resolve()), "--json"],
            capture_output=True, text=True, timeout=25, check=False,
        )
        validation = json.loads(check.stdout)
        output.with_name("baseline-validation.json").write_text(json.dumps(validation, indent=2))
        candidate_hash = hashlib.sha256(candidate.read_bytes()).hexdigest()
        if (check.returncode != 0 or validation.get("Ok") is not True
                or validation.get("Sha256") != candidate_hash
                or not validation.get("Validator", "").startswith("microsoft-openxml:")):
            if args.publish_invalid:
                os.replace(candidate, output)
            raise ValueError("Microsoft validator rejected baseline candidate")
        with ZipFile(candidate) as package:
            document = ElementTree.fromstring(package.read("word/document.xml"))
            report["tables"] = len(document.findall(".//{http://schemas.openxmlformats.org/wordprocessingml/2006/main}tbl"))
        report["warnings"].append("characters and images count source text and XObjects")
        os.replace(candidate, output)
        report["validated"] = True
    except Exception as error:
        # Diagnostics remain in access-controlled evidence, never committed.
        print(f"{type(error).__name__}: {error}", file=sys.stderr)
        report["error"] = "baseline conversion or validation failed"
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        if converter is not None:
            try:
                converter.close()
            except Exception as error:
                print(f"close failed: {type(error).__name__}", file=sys.stderr)
        report["elapsedMs"] = (time.monotonic() - started) * 1000
        print(json.dumps(report))
    return 0 if report["validated"] else 1


if __name__ == "__main__":
    sys.exit(main())
