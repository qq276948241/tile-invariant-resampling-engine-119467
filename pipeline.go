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
	"context"
	"fmt"
	"image"
	"runtime"
	"sync"
	"sync/atomic"
)

// Options configures a Pipeline. The zero value is valid and processes
// each resize as a single tile per pass with GOMAXPROCS workers and no
// memory budget.
type Options struct {
	// MaxMemoryBytes is the peak memory budget for one resize,
	// compared against EstimateMemory before any buffer is allocated.
	// Zero means unlimited. When the estimate exceeds the budget the
	// call fails with ErrBudgetExhausted and allocates nothing.
	MaxMemoryBytes int64

	// TileHeight is the height, in output rows, of one tile in each
	// filtering pass. Values <= 0 (or larger than the image) select a
	// single tile. Tiling never changes the result: every output pixel
	// is computed from the same precomputed integer coefficient table
	// in the same fixed accumulation order, so any tile size, tile
	// order, or worker count yields byte-identical output.
	TileHeight int

	// Workers is the number of goroutines processing tiles of one
	// pass. Values <= 0 select runtime.GOMAXPROCS(0). Tiles within a
	// pass write disjoint output rows, so workers never touch shared
	// mutable state.
	Workers int

	// Progress, if non-nil, is called after each completed tile with
	// the number of completed tiles and the total for the current
	// pass. It may be called concurrently from worker goroutines and
	// must be safe for concurrent use. Cancelling the context passed
	// to Resize from inside Progress is a supported way to abort a
	// resize mid-stream.
	Progress func(completed, total int)
}

// A Pipeline performs deterministic, tiled, cancellable image scaling.
//
// Determinism: a resize runs as two separable passes (horizontal, then
// vertical on the transposed intermediate), exactly like Resize. All
// filter weights are computed once up front as integer coefficient
// tables (16-bit fixed point for 8-bit images, 32-bit for 16-bit
// images; the rounding point is the int16/int32 conversion in
// createWeights8/createWeights16, followed by truncating integer
// division by the coefficient sum). Each output pixel is a pure
// function of the source pixels and those tables, so the result is
// byte-identical for any tile size, tile completion order, and worker
// count, and byte-identical to Resize for the same arguments.
//
// Alpha: NRGBA and NRGBA64 inputs are alpha-premultiplied before
// filtering and written out as premultiplied RGBA/RGBA64, matching
// Resize. Fully transparent pixels therefore contribute no color, and
// every output pixel satisfies R,G,B <= A.
//
// Bounds and stride: the source may have any bounds origin (including
// negative coordinates) and any valid stride; pixel access is
// bounds-relative. The output always has origin (0,0) and a tight
// stride.
//
// Buffer reuse and aliasing: a Pipeline holds no mutable state. Every
// Resize call allocates its own intermediate and output buffers; the
// intermediate is shared read-write only between the two passes of
// that one call, and no buffer is ever shared between calls, so
// concurrent calls on one Pipeline cannot pollute each other. The
// destination is always newly allocated and never aliases the source,
// and the source is never written.
//
// Cancellation and failure: Resize checks the context before each
// tile. On cancellation, budget exhaustion, or any other error it
// returns a nil image — never a partially computed one — and all
// buffers of the call become garbage immediately. The error wraps
// context.Canceled/context.DeadlineExceeded or one of the package
// sentinel errors.
//
// A Pipeline is safe for concurrent use by multiple goroutines.
type Pipeline struct {
	opts Options
}

// NewPipeline returns a Pipeline with the given options.
func NewPipeline(opts Options) *Pipeline {
	return &Pipeline{opts: opts}
}

// Options returns the options the pipeline was created with.
func (p *Pipeline) Options() Options {
	return p.opts
}

// Resize scales img to the given dimensions like the package-level
// Resize, but validates the request (see NewSpec), honors the
// pipeline's memory budget and tiling options, and can be cancelled
// via ctx. A nil ctx is treated as context.Background().
//
// Like Resize, it returns img unchanged when the resolved output
// dimensions equal the source dimensions. Unlike Resize it rejects
// (rather than silently tolerating) zero-sized sources, out-of-range
// interpolation functions, unsupported image types, and truncated
// pixel buffers.
func (p *Pipeline) Resize(ctx context.Context, width, height uint, img image.Image, interp InterpolationFunction) (image.Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	spec, err := NewSpec(width, height, img, interp)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Trivial case, kept compatible with Resize: no scaling needed.
	if spec.OutWidth == spec.SrcBounds.Dx() && spec.OutHeight == spec.SrcBounds.Dy() {
		return img, nil
	}

	if budget := p.opts.MaxMemoryBytes; budget > 0 {
		if est := EstimateMemory(spec); est > budget {
			return nil, fmt.Errorf("%w: need ~%d bytes, budget is %d",
				ErrBudgetExhausted, est, budget)
		}
	}

	scaleX, scaleY := calcFactors(spec.Width, spec.Height,
		float64(spec.SrcBounds.Dx()), float64(spec.SrcBounds.Dy()))
	ow, oh := spec.OutWidth, spec.OutHeight
	dy := spec.SrcBounds.Dy()

	if interp == NearestNeighbor {
		return p.resizeNearest(ctx, img, scaleX, scaleY, ow, oh, dy)
	}

	taps, kernel := interp.kernel()
	switch input := img.(type) {
	case *image.RGBA:
		temp := image.NewRGBA(image.Rect(0, 0, dy, ow))
		result := image.NewRGBA(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeights8(ow, taps, blur, scaleX, kernel)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			resizeRGBA(input, s.(*image.RGBA), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeights8(oh, taps, blur, scaleY, kernel)
		if err := p.execPass(ctx, result, func(s image.Image) {
			resizeRGBA(temp, s.(*image.RGBA), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.NRGBA:
		temp := image.NewRGBA(image.Rect(0, 0, dy, ow))
		result := image.NewRGBA(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeights8(ow, taps, blur, scaleX, kernel)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			resizeNRGBA(input, s.(*image.RGBA), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeights8(oh, taps, blur, scaleY, kernel)
		if err := p.execPass(ctx, result, func(s image.Image) {
			resizeRGBA(temp, s.(*image.RGBA), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.YCbCr:
		temp := newYCC(image.Rect(0, 0, dy, ow), input.SubsampleRatio)
		result := newYCC(image.Rect(0, 0, ow, oh), image.YCbCrSubsampleRatio444)
		in := imageYCbCrToYCC(input)
		coeffs, offset, n := createWeights8(ow, taps, blur, scaleX, kernel)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			resizeYCbCr(in, s.(*ycc), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeights8(oh, taps, blur, scaleY, kernel)
		if err := p.execPass(ctx, result, func(s image.Image) {
			resizeYCbCr(temp, s.(*ycc), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result.YCbCr(), nil

	case *image.RGBA64:
		temp := image.NewRGBA64(image.Rect(0, 0, dy, ow))
		result := image.NewRGBA64(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeights16(ow, taps, blur, scaleX, kernel)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			resizeRGBA64(input, s.(*image.RGBA64), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeights16(oh, taps, blur, scaleY, kernel)
		if err := p.execPass(ctx, result, func(s image.Image) {
			resizeRGBA64(temp, s.(*image.RGBA64), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.NRGBA64:
		temp := image.NewRGBA64(image.Rect(0, 0, dy, ow))
		result := image.NewRGBA64(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeights16(ow, taps, blur, scaleX, kernel)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			resizeNRGBA64(input, s.(*image.RGBA64), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeights16(oh, taps, blur, scaleY, kernel)
		if err := p.execPass(ctx, result, func(s image.Image) {
			resizeRGBA64(temp, s.(*image.RGBA64), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.Gray:
		temp := image.NewGray(image.Rect(0, 0, dy, ow))
		result := image.NewGray(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeights8(ow, taps, blur, scaleX, kernel)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			resizeGray(input, s.(*image.Gray), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeights8(oh, taps, blur, scaleY, kernel)
		if err := p.execPass(ctx, result, func(s image.Image) {
			resizeGray(temp, s.(*image.Gray), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.Gray16:
		temp := image.NewGray16(image.Rect(0, 0, dy, ow))
		result := image.NewGray16(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeights16(ow, taps, blur, scaleX, kernel)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			resizeGray16(input, s.(*image.Gray16), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeights16(oh, taps, blur, scaleY, kernel)
		if err := p.execPass(ctx, result, func(s image.Image) {
			resizeGray16(temp, s.(*image.Gray16), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil
	}

	// Unreachable: NewSpec already rejected unsupported types.
	return nil, fmt.Errorf("%w: %T", ErrUnsupportedColorModel, img)
}

// resizeNearest is the nearest-neighbor variant of Resize, mirroring
// the package-level resizeNearest with tiled, cancellable passes.
func (p *Pipeline) resizeNearest(ctx context.Context, img image.Image, scaleX, scaleY float64, ow, oh, dy int) (image.Image, error) {
	taps, _ := NearestNeighbor.kernel()

	switch input := img.(type) {
	case *image.RGBA:
		temp := image.NewRGBA(image.Rect(0, 0, dy, ow))
		result := image.NewRGBA(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeightsNearest(ow, taps, blur, scaleX)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			nearestRGBA(input, s.(*image.RGBA), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeightsNearest(oh, taps, blur, scaleY)
		if err := p.execPass(ctx, result, func(s image.Image) {
			nearestRGBA(temp, s.(*image.RGBA), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.NRGBA:
		temp := image.NewNRGBA(image.Rect(0, 0, dy, ow))
		result := image.NewNRGBA(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeightsNearest(ow, taps, blur, scaleX)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			nearestNRGBA(input, s.(*image.NRGBA), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeightsNearest(oh, taps, blur, scaleY)
		if err := p.execPass(ctx, result, func(s image.Image) {
			nearestNRGBA(temp, s.(*image.NRGBA), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.YCbCr:
		temp := newYCC(image.Rect(0, 0, dy, ow), input.SubsampleRatio)
		result := newYCC(image.Rect(0, 0, ow, oh), image.YCbCrSubsampleRatio444)
		in := imageYCbCrToYCC(input)
		coeffs, offset, n := createWeightsNearest(ow, taps, blur, scaleX)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			nearestYCbCr(in, s.(*ycc), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeightsNearest(oh, taps, blur, scaleY)
		if err := p.execPass(ctx, result, func(s image.Image) {
			nearestYCbCr(temp, s.(*ycc), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result.YCbCr(), nil

	case *image.RGBA64:
		temp := image.NewRGBA64(image.Rect(0, 0, dy, ow))
		result := image.NewRGBA64(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeightsNearest(ow, taps, blur, scaleX)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			nearestRGBA64(input, s.(*image.RGBA64), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeightsNearest(oh, taps, blur, scaleY)
		if err := p.execPass(ctx, result, func(s image.Image) {
			nearestRGBA64(temp, s.(*image.RGBA64), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.NRGBA64:
		temp := image.NewNRGBA64(image.Rect(0, 0, dy, ow))
		result := image.NewNRGBA64(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeightsNearest(ow, taps, blur, scaleX)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			nearestNRGBA64(input, s.(*image.NRGBA64), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeightsNearest(oh, taps, blur, scaleY)
		if err := p.execPass(ctx, result, func(s image.Image) {
			nearestNRGBA64(temp, s.(*image.NRGBA64), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.Gray:
		temp := image.NewGray(image.Rect(0, 0, dy, ow))
		result := image.NewGray(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeightsNearest(ow, taps, blur, scaleX)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			nearestGray(input, s.(*image.Gray), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeightsNearest(oh, taps, blur, scaleY)
		if err := p.execPass(ctx, result, func(s image.Image) {
			nearestGray(temp, s.(*image.Gray), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil

	case *image.Gray16:
		temp := image.NewGray16(image.Rect(0, 0, dy, ow))
		result := image.NewGray16(image.Rect(0, 0, ow, oh))
		coeffs, offset, n := createWeightsNearest(ow, taps, blur, scaleX)
		if err := p.execPass(ctx, temp, func(s image.Image) {
			nearestGray16(input, s.(*image.Gray16), scaleX, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		coeffs, offset, n = createWeightsNearest(oh, taps, blur, scaleY)
		if err := p.execPass(ctx, result, func(s image.Image) {
			nearestGray16(temp, s.(*image.Gray16), scaleY, coeffs, offset, n)
		}); err != nil {
			return nil, err
		}
		return result, nil
	}

	// Unreachable: NewSpec already rejected unsupported types.
	return nil, fmt.Errorf("%w: %T", ErrUnsupportedColorModel, img)
}

// execPass splits one filtering pass over dst into row-band tiles and
// runs fn on each tile with a worker pool. Tiles are disjoint
// sub-images of dst; fn must be safe to run concurrently on disjoint
// tiles. The pass aborts early when ctx is cancelled, waits for all
// workers to finish, and reports the context error.
func (p *Pipeline) execPass(ctx context.Context, dst imageWithSubImage, fn func(image.Image)) error {
	b := dst.Bounds()
	tileHeight := p.opts.TileHeight
	if tileHeight <= 0 || tileHeight > b.Dy() {
		tileHeight = b.Dy()
	}
	total := (b.Dy() + tileHeight - 1) / tileHeight
	if total == 0 {
		return nil
	}
	workers := p.opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > total {
		workers = total
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()

	jobs := make(chan image.Rectangle)
	var wg sync.WaitGroup
	var completed int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				if ctx.Err() != nil {
					continue
				}
				fn(dst.SubImage(r))
				done := int(atomic.AddInt64(&completed, 1))
				if p.opts.Progress != nil {
					p.opts.Progress(done, total)
				}
			}
		}()
	}

feed:
	for y := b.Min.Y; y < b.Max.Y; y += tileHeight {
		r := image.Rect(b.Min.X, y, b.Max.X, min(y+tileHeight, b.Max.Y))
		select {
		case jobs <- r:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if atomic.LoadInt64(&completed) < int64(total) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	return nil
}
