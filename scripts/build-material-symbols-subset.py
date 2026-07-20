#!/usr/bin/env python3
import re
import shutil
import subprocess
from pathlib import Path
from fontTools.ttLib import TTFont

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "node_modules/material-symbols/material-symbols-outlined.woff2"
OUTPUT = ROOT / "src/assets/material-symbols-outlined.woff2"

if not SOURCE.exists():
    raise SystemExit("run bun install before building the icon subset")
if not shutil.which("pyftsubset"):
    raise SystemExit("pyftsubset is required (python3-fonttools)")

font = TTFont(SOURCE)
glyphs = set(font.getGlyphOrder())
used = {"help"}
for path in (ROOT / "src").rglob("*.js"):
    for token in re.findall(r"(?<![\w-])([a-z][a-z0-9_]{1,})(?![\w-])", path.read_text()):
        if token in glyphs:
            used.add(token)

OUTPUT.parent.mkdir(parents=True, exist_ok=True)
subprocess.run([
    "pyftsubset",
    str(SOURCE),
    f"--output-file={OUTPUT}",
    f"--glyphs={','.join(sorted(used))}",
    f"--text={''.join(sorted(used))}",
    "--layout-features=*",
    "--no-layout-closure",
    "--glyph-names",
    "--symbol-cmap",
    "--legacy-cmap",
    "--notdef-glyph",
    "--notdef-outline",
    "--recommended-glyphs",
    "--name-IDs=*",
    "--name-legacy",
    "--name-languages=*",
    "--drop-tables+=STAT",
    "--flavor=woff2",
], check=True)

if OUTPUT.read_bytes()[:4] != b"wOF2":
    raise SystemExit("generated file is not WOFF2")
if OUTPUT.stat().st_size >= SOURCE.stat().st_size // 2:
    raise SystemExit("generated subset is unexpectedly large")

subset = TTFont(OUTPUT)
features = {record.FeatureTag for record in subset["GSUB"].table.FeatureList.FeatureRecord}
if "rlig" not in features:
    raise SystemExit("generated subset lost required ligature substitutions")
for required in ("api", "hub", "settings", "volunteer_activism"):
    if required not in subset.getGlyphOrder():
        raise SystemExit(f"generated subset is missing {required}")
print(f"Material Symbols: {len(used)} glyph names, {SOURCE.stat().st_size} -> {OUTPUT.stat().st_size} bytes")
