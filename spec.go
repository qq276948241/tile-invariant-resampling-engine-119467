/*
Copyright (c) 2012, Jan Schlicht <jan.schlicht@gmail.com>

Permission to use, copy, modify, and/or distribute this software for any purpose
with or without fee is hereby granted, provided that the above copyright notice
and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY AND
FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM LOSS
OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER
TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF
THIS SOFTWARE.
*/

package resize

import (
	"errors"
	"fmt"
	"image"
)

// Sentinel errors returned by the deterministic pipeline (see Pipeline).
// All errors returned by NewSpec and Pipeline.Resize wrap one of these
// values (or a context error), so callers can classify failures with
// errors.Is. A failed call never returns a partially computed image:
// the image return value is always nil when err is non-nil.
var (
	// ErrNilImage is returned when a nil image.Image is passed.
	ErrNilImage = errors.New("resize: nil image")

	// ErrZeroDimension is returned when the source image has empty
	// bounds, when both requested output dimensions are zero, or when
	// aspect-ratio rounding resolves an output dimension to zero.
	ErrZeroDimension = errors.New("resize: zero dimension")

	// ErrDimensionOverflow is returned when a requested or resolved
	// output dimension exceeds MaxDimension.
	ErrDimensionOverflow = errors.New("resize: dimension overflow")

	// ErrUnsupportedSampler is returned when the interpolation function
	// is outside the range [NearestNeighbor, Lanczos3]. The legacy
	// Resize silently falls back to nearest-neighbor for such values;
	// the pipeline rejects them instead.
	ErrUnsupportedSampler = errors.New("resize: unsupported interpolation function")

	// ErrUnsupportedColorModel is returned for image types the pipeline
	// has no deterministic fast path for. Supported concrete types are
	// *image.RGBA, *image.NRGBA, *image.YCbCr (with a known subsample
	// ratio), *image.RGBA64, *image.NRGBA64, *image.Gray and
	// *image.Gray16. The legacy Resize falls back to a slow generic
	// path for other types; the pipeline rejects them so that every
	// accepted input has a specified, byte-stable result.
	ErrUnsupportedColorModel = errors.New("resize: unsupported color model")

	// ErrTruncatedImage is returned when a pixel buffer is shorter than
	// the image bounds and stride require, or when the stride is
	// smaller than one row of pixels. Such images would panic or read
	// out of bounds in the legacy code path.
	ErrTruncatedImage = errors.New("resize: pixel buffer too small for bounds")

	// ErrBudgetExhausted is returned when the estimated peak memory of
	// a resize exceeds Options.MaxMemoryBytes. The check happens before
	// any buffer is allocated.
	ErrBudgetExhausted = errors.New("resize: memory budget exhausted")
)

// MaxDimension is the largest width or height, in pixels, that the
// deterministic pipeline accepts for an output image. It keeps
// dimension arithmetic and buffer sizes far away from int overflow.
const MaxDimension = 1 << 24

// pixelKind identifies the concrete fast path a Spec will use.
type pixelKind uint8

const (
	kindRGBA pixelKind = iota
	kindNRGBA
	kindYCbCr
	kindRGBA64
	kindNRGBA64
	kindGray
	kindGray16
)

// A Spec is a validated, fully resolved resize request. It is produced
// by NewSpec and consumed by Pipeline.Resize and EstimateMemory.
//
// Dimension and rounding semantics (kept identical to Resize):
//
//   - Width or Height may be 0, meaning "preserve the aspect ratio".
//     Both zero is rejected with ErrZeroDimension.
//   - A resolved dimension is uint(0.7 + src/scale): the historical
//     0.7 bias of Resize, so odd source sizes round the same way as
//     the public API. If this resolves to 0 the request is rejected.
//   - SrcBounds may have any origin, including negative coordinates;
//     the output image always has origin (0,0).
type Spec struct {
	// Width and Height are the requested output dimensions; one of
	// them may be 0 (aspect-preserving).
	Width, Height uint
	// Interp is the requested interpolation function.
	Interp InterpolationFunction
	// SrcBounds is the bounds rectangle of the source image.
	SrcBounds image.Rectangle
	// OutWidth and OutHeight are the resolved output dimensions.
	OutWidth, OutHeight int

	kind pixelKind
}

// NewSpec validates a resize request and resolves its output
// dimensions. It performs no pixel work. On error the returned Spec is
// the zero value and the error wraps one of the sentinel errors
// declared above.
func NewSpec(width, height uint, img image.Image, interp InterpolationFunction) (Spec, error) {
	if img == nil {
		return Spec{}, ErrNilImage
	}
	b := img.Bounds()
	dx, dy := b.Dx(), b.Dy()
	if dx <= 0 || dy <= 0 {
		return Spec{}, fmt.Errorf("%w: source bounds %v", ErrZeroDimension, b)
	}
	if interp < NearestNeighbor || interp > Lanczos3 {
		return Spec{}, fmt.Errorf("%w: %d", ErrUnsupportedSampler, int(interp))
	}
	kind, err := classify(img)
	if err != nil {
		return Spec{}, err
	}
	if width == 0 && height == 0 {
		return Spec{}, fmt.Errorf("%w: both output dimensions are zero", ErrZeroDimension)
	}
	if width > MaxDimension || height > MaxDimension {
		return Spec{}, fmt.Errorf("%w: requested %dx%d exceeds MaxDimension %d",
			ErrDimensionOverflow, width, height, MaxDimension)
	}

	// Resolve aspect-preserving dimensions exactly like Resize does.
	scaleX, scaleY := calcFactors(width, height, float64(dx), float64(dy))
	w, h := width, height
	if w == 0 {
		w = uint(0.7 + float64(dx)/scaleX)
	}
	if h == 0 {
		h = uint(0.7 + float64(dy)/scaleY)
	}
	if w == 0 || h == 0 {
		return Spec{}, fmt.Errorf("%w: aspect-ratio rounding resolved to %dx%d",
			ErrZeroDimension, w, h)
	}
	if w > MaxDimension || h > MaxDimension {
		return Spec{}, fmt.Errorf("%w: resolved %dx%d exceeds MaxDimension %d",
			ErrDimensionOverflow, w, h, MaxDimension)
	}

	return Spec{
		Width:     width,
		Height:    height,
		Interp:    interp,
		SrcBounds: b,
		OutWidth:  int(w),
		OutHeight: int(h),
		kind:      kind,
	}, nil
}

// EstimateMemory returns a conservative upper bound, in bytes, of the
// peak memory the pipeline will allocate for a resize described by
// spec: the transposed intermediate, the output image, and (for YCbCr
// input) the unpacked source copy. It lets callers check
// Options.MaxMemoryBytes before starting work. The estimate never
// undercounts the buffers the pipeline itself allocates.
func EstimateMemory(spec Spec) int64 {
	dx := int64(spec.SrcBounds.Dx())
	dy := int64(spec.SrcBounds.Dy())
	w := int64(spec.OutWidth)
	h := int64(spec.OutHeight)
	switch spec.kind {
	case kindRGBA, kindNRGBA:
		return 4 * (dy*w + w*h)
	case kindRGBA64, kindNRGBA64:
		return 8 * (dy*w + w*h)
	case kindGray:
		return dy*w + w*h
	case kindGray16:
		return 2 * (dy*w + w*h)
	case kindYCbCr:
		// Unpacked 3-byte ycc copy of the source, the transposed
		// intermediate, the ycc result, plus the final subsampled
		// YCbCr image (at most 2 bytes/pixel for 444, less otherwise).
		return 3*(dx*dy+dy*w+w*h) + 2*w*h
	}
	return 0
}

// classify validates the concrete image type and the consistency of
// its pixel buffers, and selects the fast path for it.
func classify(img image.Image) (pixelKind, error) {
	b := img.Bounds()
	dx, dy := b.Dx(), b.Dy()

	// check verifies that a plane of bpp bytes per pixel with the
	// given stride holds at least dx*dy pixels starting at Pix[0]
	// (Pix always starts at the bounds minimum for the standard
	// library image types).
	check := func(what string, pixLen, stride, bpp, w, h int) error {
		if stride < w*bpp {
			return fmt.Errorf("%w: %s stride %d smaller than row of %d pixels x %d bytes",
				ErrTruncatedImage, what, stride, w, bpp)
		}
		need := stride*(h-1) + w*bpp
		if pixLen < need {
			return fmt.Errorf("%w: %s has %d bytes, need at least %d",
				ErrTruncatedImage, what, pixLen, need)
		}
		return nil
	}

	switch in := img.(type) {
	case *image.RGBA:
		return kindRGBA, check("RGBA.Pix", len(in.Pix), in.Stride, 4, dx, dy)
	case *image.NRGBA:
		return kindNRGBA, check("NRGBA.Pix", len(in.Pix), in.Stride, 4, dx, dy)
	case *image.RGBA64:
		return kindRGBA64, check("RGBA64.Pix", len(in.Pix), in.Stride, 8, dx, dy)
	case *image.NRGBA64:
		return kindNRGBA64, check("NRGBA64.Pix", len(in.Pix), in.Stride, 8, dx, dy)
	case *image.Gray:
		return kindGray, check("Gray.Pix", len(in.Pix), in.Stride, 1, dx, dy)
	case *image.Gray16:
		return kindGray16, check("Gray16.Pix", len(in.Pix), in.Stride, 2, dx, dy)
	case *image.YCbCr:
		cw, ch, err := chromaDims(in.SubsampleRatio, dx, dy)
		if err != nil {
			return 0, err
		}
		if err := check("YCbCr.Y", len(in.Y), in.YStride, 1, dx, dy); err != nil {
			return 0, err
		}
		if err := check("YCbCr.Cb", len(in.Cb), in.CStride, 1, cw, ch); err != nil {
			return 0, err
		}
		if err := check("YCbCr.Cr", len(in.Cr), in.CStride, 1, cw, ch); err != nil {
			return 0, err
		}
		return kindYCbCr, nil
	default:
		return 0, fmt.Errorf("%w: %T", ErrUnsupportedColorModel, img)
	}
}

// chromaDims returns the chroma plane dimensions for a YCbCr subsample
// ratio, matching the standard library's image.NewYCbCr layout.
func chromaDims(ratio image.YCbCrSubsampleRatio, w, h int) (cw, ch int, err error) {
	switch ratio {
	case image.YCbCrSubsampleRatio444:
		return w, h, nil
	case image.YCbCrSubsampleRatio422:
		return (w + 1) / 2, h, nil
	case image.YCbCrSubsampleRatio420:
		return (w + 1) / 2, (h + 1) / 2, nil
	case image.YCbCrSubsampleRatio440:
		return w, (h + 1) / 2, nil
	case image.YCbCrSubsampleRatio411:
		return (w + 3) / 4, h, nil
	case image.YCbCrSubsampleRatio410:
		return (w + 3) / 4, (h + 3) / 4, nil
	}
	return 0, 0, fmt.Errorf("%w: YCbCr subsample ratio %d", ErrUnsupportedColorModel, int(ratio))
}
