#!/usr/bin/env bash
# Render the README savings figure (docs/img/readme/savings-figure.html) to light and dark PNGs.
# Needs network access for the Inter font, a Chromium binary (set CHROME, or have chromium on PATH) and python3 with Pillow.
set -euo pipefail
cd "$(dirname "$0")/.."
CHROME="${CHROME:-$(command -v chromium || command -v chromium-browser || command -v google-chrome || true)}"
: "${CHROME:?no Chromium found; set CHROME to a Chromium binary}"
src="file://$PWD/docs/img/readme/savings-figure.html"
for theme in light dark; do
  out="docs/img/readme/savings-$theme.png"
  # Render taller than the content: headless Chromium can skip painting the last rows of the window.
  "$CHROME" --headless=new --no-sandbox --disable-gpu --hide-scrollbars \
    --force-device-scale-factor=2 --window-size=1200,1500 --virtual-time-budget=5000 \
    --screenshot="$out" "$src#$theme"
  # Crop the empty rows below the cards, keep a 52 px (104 device px) bottom margin.
  python3 - "$out" <<'PY'
import sys
from PIL import Image, ImageChops
p = sys.argv[1]
im = Image.open(p).convert("RGB")
bg = Image.new("RGB", im.size, im.getpixel((5, im.height - 5)))
box = ImageChops.difference(im, bg).getbbox()
if box is None:
    sys.exit(f"{p}: the render is empty (only background); check the Chromium output")
im.crop((0, 0, im.width, min(im.height, box[3] + 104))).save(p, optimize=True)
PY
done
