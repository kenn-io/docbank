# JPEG DC fixtures

Pillow 12 generated these images from a synthetic YCbCr gradient. Every file has nonzero DC coefficients. The 4:4:4 and 4:2:2 files restart every three blocks. The 4:2:0 file sets a 48-block interval so Go can decode its noninterleaved scans for comparison. Pillow supports 4:4:4, 4:2:2, and 4:2:0 through its subsampling option; it cannot set unequal chroma sampling such as 2x2/2x1/1x1.

Run this script outside the repository with `python -I`, with `out` set to this testdata directory:

```python
from PIL import Image
from pathlib import Path
out = Path("testdata")
image = Image.new("YCbCr", (64, 48))
image.putdata([(48+x+y, 104+x//2, 112+y//2) for y in range(48) for x in range(64)])
image = image.convert("RGB")
for name, sampling, progressive in [("progressive-444", 0, True), ("progressive-420", 2, True), ("baseline-422", 1, False)]:
    image.save(out / (name+".jpg"), quality=90, subsampling=sampling, progressive=progressive, restart_marker_blocks=48 if sampling == 2 else 3)
```
