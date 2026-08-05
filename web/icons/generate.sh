#!/bin/sh
# Regenerates the PNGs from icon.svg, which is the only source of truth here.
# Needs ImageMagick. Committing the PNGs means the build stays dependency-free;
# this script is how you reproduce them, not something the app runs.
set -eu
cd "$(dirname "$0")"

# Everything is rendered at 4x and downsampled. ImageMagick's internal SVG
# renderer antialiases noticeably better that way than rasterising straight to
# the target size. The viewBox is 512 units, so density 288 gives 2048px.
DENSITY=288

# The maskable and apple-touch variants are the same artwork with the corner
# radius removed. Both are cropped by the platform to its own shape -- a circle
# on some Android launchers, a squircle on iOS -- and a source that is already
# rounded leaves transparent corners showing through the mask.
sed 's/rx="112"/rx="0"/' icon.svg > .full-bleed.svg
trap 'rm -f .full-bleed.svg' EXIT

png() {
  convert -background none -density "$DENSITY" "$1" \
          -resize "${2}x${2}" -depth 8 -strip "PNG32:$3"
}

png icon.svg        192 icon-192.png
png icon.svg        512 icon-512.png
png .full-bleed.svg 512 icon-maskable-512.png
png .full-bleed.svg 180 apple-touch-icon.png

echo "regenerated:"
ls -la icon-192.png icon-512.png icon-maskable-512.png apple-touch-icon.png
