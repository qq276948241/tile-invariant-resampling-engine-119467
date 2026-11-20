# This package is no longer being updated! Please look for alternatives if that bothers you.

Resize
======

Image resizing for the [Go programming language](http://golang.org) with common interpolation methods.

[![Build Status](https://travis-ci.org/nfnt/resize.svg)](https://travis-ci.org/nfnt/resize)

Installation
------------

```bash
$ go get github.com/nfnt/resize
```

It's that easy!

Usage
-----

This package needs at least Go 1.1. Import package with

```go
import "github.com/nfnt/resize"
```

The resize package provides 2 functions:

* `resize.Resize` creates a scaled image with new dimensions (`width`, `height`) using the interpolation function `interp`.
  If either `width` or `height` is set to 0, it will be set to an aspect ratio preserving value.
* `resize.Thumbnail` downscales an image preserving its aspect ratio to the maximum dimensions (`maxWidth`, `maxHeight`).
  It will return the original image if original sizes are smaller than the provided dimensions.

```go
resize.Resize(width, height uint, img image.Image, interp resize.InterpolationFunction) image.Image
resize.Thumbnail(maxWidth, maxHeight uint, img image.Image, interp resize.InterpolationFunction) image.Image
```

The provided interpolation functions are (from fast to slow execution time)

- `NearestNeighbor`: [Nearest-neighbor interpolation](http://en.wikipedia.org/wiki/Nearest-neighbor_interpolation)
- `Bilinear`: [Bilinear interpolation](http://en.wikipedia.org/wiki/Bilinear_interpolation)
- `Bicubic`: [Bicubic interpolation](http://en.wikipedia.org/wiki/Bicubic_interpolation)
- `MitchellNetravali`: [Mitchell-Netravali interpolation](http://dl.acm.org/citation.cfm?id=378514)
- `Lanczos2`: [Lanczos resampling](http://en.wikipedia.org/wiki/Lanczos_resampling) with a=2
- `Lanczos3`: [Lanczos resampling](http://en.wikipedia.org/wiki/Lanczos_resampling) with a=3

Which of these methods gives the best results depends on your use case.

Sample usage:

```go
package main

import (
	"github.com/nfnt/resize"
	"image/jpeg"
	"log"
	"os"
)

func main() {
	// open "test.jpg"
	file, err := os.Open("test.jpg")
	if err != nil {
		log.Fatal(err)
	}

	// decode jpeg into image.Image
	img, err := jpeg.Decode(file)
	if err != nil {
		log.Fatal(err)
	}
	file.Close()

	// resize to width 1000 using Lanczos resampling
	// and preserve aspect ratio
	m := resize.Resize(1000, 0, img, resize.Lanczos3)

	out, err := os.Create("test_resized.jpg")
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()

	// write new image to file
	jpeg.Encode(out, m, nil)
}
```

Caveats
-------

* Optimized access routines are used for `image.RGBA`, `image.NRGBA`, `image.RGBA64`, `image.NRGBA64`, `image.YCbCr`, `image.Gray`, and `image.Gray16` types. All other image types are accessed in a generic way that will result in slow processing speed.
* JPEG images are stored in `image.YCbCr`. This image format stores data in a way that will decrease processing speed. A resize may be up to 2 times slower than with `image.RGBA`. 

Deterministic pipeline
----------------------

Alongside the classic API above, the package provides a validated,
tiled, cancellable pipeline built on the same interpolation kernels:

```go
pipe := resize.NewPipeline(resize.Options{
    MaxMemoryBytes: 256 << 20, // fail instead of allocating past a budget
    TileHeight:     64,        // rows per tile; 0 = single tile
    Workers:        0,         // 0 = GOMAXPROCS
})
img, err := pipe.Resize(ctx, 1000, 0, src, resize.Lanczos3)
```

Guarantees, verified by `go test` and by the offline acceptance
command `go run ./cmd/resizecheck` (deterministic output, non-zero
exit on failure):

* **Compatibility**: for valid inputs the pipeline produces
  byte-identical output to `Resize`/`Thumbnail`, including the
  historical `0.7` rounding bias for aspect-preserved dimensions and
  the same-size fast path that returns the input image.
* **Validation**: zero dimensions, dimensions above `MaxDimension`,
  out-of-range interpolation functions, unsupported image types, and
  truncated pixel buffers are rejected with typed sentinel errors
  (`ErrZeroDimension`, `ErrDimensionOverflow`, `ErrUnsupportedSampler`,
  `ErrUnsupportedColorModel`, `ErrTruncatedImage`, `ErrNilImage`)
  instead of being silently tolerated. A failed call never returns a
  partially computed image.
* **Determinism**: filter weights are precomputed integer tables and
  each output pixel is a pure function of them, so any tile size, tile
  order, and worker count yields the same bytes.
* **Cancellation**: the context is checked before every tile; on
  cancellation the error wraps `context.Canceled`/
  `context.DeadlineExceeded` and the result is nil.
* **Memory**: `EstimateMemory` reports the peak allocation of a
  request; `Options.MaxMemoryBytes` rejects requests over budget with
  `ErrBudgetExhausted` before anything is allocated. Pipelines hold no
  mutable state and share no buffers between calls, so concurrent use
  is safe.


Downsizing Samples
-------

Downsizing is not as simple as it might look like. Images have to be filtered before they are scaled down, otherwise aliasing might occur.
Filtering is highly subjective: Applying too much will blur the whole image, too little will make aliasing become apparent.
Resize tries to provide sane defaults that should suffice in most cases.

### Artificial sample

Original image
![Rings](http://nfnt.github.com/img/rings_lg_orig.png)

<table>
<tr>
<th><img src="http://nfnt.github.com/img/rings_300_NearestNeighbor.png" /><br>Nearest-Neighbor</th>
<th><img src="http://nfnt.github.com/img/rings_300_Bilinear.png" /><br>Bilinear</th>
</tr>
<tr>
<th><img src="http://nfnt.github.com/img/rings_300_Bicubic.png" /><br>Bicubic</th>
<th><img src="http://nfnt.github.com/img/rings_300_MitchellNetravali.png" /><br>Mitchell-Netravali</th>
</tr>
<tr>
<th><img src="http://nfnt.github.com/img/rings_300_Lanczos2.png" /><br>Lanczos2</th>
<th><img src="http://nfnt.github.com/img/rings_300_Lanczos3.png" /><br>Lanczos3</th>
</tr>
</table>

### Real-Life sample

Original image  
![Original](http://nfnt.github.com/img/IMG_3694_720.jpg)

<table>
<tr>
<th><img src="http://nfnt.github.com/img/IMG_3694_300_NearestNeighbor.png" /><br>Nearest-Neighbor</th>
<th><img src="http://nfnt.github.com/img/IMG_3694_300_Bilinear.png" /><br>Bilinear</th>
</tr>
<tr>
<th><img src="http://nfnt.github.com/img/IMG_3694_300_Bicubic.png" /><br>Bicubic</th>
<th><img src="http://nfnt.github.com/img/IMG_3694_300_MitchellNetravali.png" /><br>Mitchell-Netravali</th>
</tr>
<tr>
<th><img src="http://nfnt.github.com/img/IMG_3694_300_Lanczos2.png" /><br>Lanczos2</th>
<th><img src="http://nfnt.github.com/img/IMG_3694_300_Lanczos3.png" /><br>Lanczos3</th>
</tr>
</table>


License
-------

Copyright (c) 2012 Jan Schlicht <janschlicht@gmail.com>
Resize is released under a MIT style license.
