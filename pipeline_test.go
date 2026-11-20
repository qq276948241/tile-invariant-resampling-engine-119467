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
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"sync"
	"testing"
	"time"
)

// pixBytes returns a type tag and the raw pixel bytes of an image for
// byte-exact comparison.
func pixBytes(t *testing.T, img image.Image) (string, []byte) {
	t.Helper()
	switch m := img.(type) {
	case *image.RGBA:
		return "RGBA", m.Pix
	case *image.NRGBA:
		return "NRGBA", m.Pix
	case *image.RGBA64:
		return "RGBA64", m.Pix
	case *image.NRGBA64:
		return "NRGBA64", m.Pix
	case *image.Gray:
		return "Gray", m.Pix
	case *image.Gray16:
		return "Gray16", m.Pix
	case *image.YCbCr:
		all := make([]byte, 0, len(m.Y)+len(m.Cb)+len(m.Cr))
		all = append(all, m.Y...)
		all = append(all, m.Cb...)
		all = append(all, m.Cr...)
		return "YCbCr", all
	}
	t.Fatalf("unexpected image type %T", img)
	return "", nil
}

func gradientRGBA(w, h int) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := m.PixOffset(x, y)
			m.Pix[i+0] = uint8(x*4 + y)
			m.Pix[i+1] = uint8(y*5 + x)
			m.Pix[i+2] = uint8((x+y)*3 + 7)
			m.Pix[i+3] = 0xff
		}
	}
	return m
}

func testImages() map[string]image.Image {
	nrgba := image.NewNRGBA(image.Rect(0, 0, 16, 12))
	for i := 0; i < len(nrgba.Pix); i += 4 {
		nrgba.Pix[i+0] = uint8(i / 4)
		nrgba.Pix[i+1] = uint8(i / 2)
		nrgba.Pix[i+2] = 200
		nrgba.Pix[i+3] = uint8(128 + i/4%128)
	}
	ycc := image.NewYCbCr(image.Rect(0, 0, 16, 12), image.YCbCrSubsampleRatio420)
	for i := range ycc.Y {
		ycc.Y[i] = uint8(i * 3)
	}
	for i := range ycc.Cb {
		ycc.Cb[i] = uint8(100 + i)
		ycc.Cr[i] = uint8(200 - i)
	}
	rgba64 := image.NewRGBA64(image.Rect(0, 0, 9, 7))
	for i := 0; i < len(rgba64.Pix); i++ {
		rgba64.Pix[i] = uint8(i*5 + 1)
	}
	nrgba64 := image.NewNRGBA64(image.Rect(0, 0, 9, 7))
	for i := 0; i < len(nrgba64.Pix); i += 8 {
		nrgba64.Pix[i+0] = uint8(i)
		nrgba64.Pix[i+2] = uint8(i + 3)
		nrgba64.Pix[i+4] = uint8(i + 5)
		nrgba64.Pix[i+6] = 0xff
		nrgba64.Pix[i+7] = 0xff
	}
	gray := image.NewGray(image.Rect(0, 0, 11, 5))
	for i := range gray.Pix {
		gray.Pix[i] = uint8(i * 7)
	}
	gray16 := image.NewGray16(image.Rect(0, 0, 11, 5))
	for i := 0; i < len(gray16.Pix); i += 2 {
		gray16.Pix[i] = uint8(i)
		gray16.Pix[i+1] = uint8(i + 1)
	}
	return map[string]image.Image{
		"RGBA":    gradientRGBA(16, 12),
		"NRGBA":   nrgba,
		"YCbCr":   ycc,
		"RGBA64":  rgba64,
		"NRGBA64": nrgba64,
		"Gray":    gray,
		"Gray16":  gray16,
	}
}

func TestNewSpecErrorTypes(t *testing.T) {
	good := gradientRGBA(8, 8)
	truncatedRGBA := &image.RGBA{Pix: make([]byte, 10), Stride: 32, Rect: image.Rect(0, 0, 8, 8)}
	badStride := &image.RGBA{Pix: make([]byte, 64), Stride: 3, Rect: image.Rect(0, 0, 8, 8)}
	truncatedGray := &image.Gray{Pix: make([]byte, 5), Stride: 8, Rect: image.Rect(0, 0, 8, 8)}
	truncatedYCbCr := &image.YCbCr{
		Y:              make([]byte, 64),
		Cb:             make([]byte, 3), // 420 8x8 needs 4x4=16
		Cr:             make([]byte, 16),
		YStride:        8,
		CStride:        4,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, 8, 8),
	}
	badRatio := &image.YCbCr{
		Y:              make([]byte, 64),
		Cb:             make([]byte, 64),
		Cr:             make([]byte, 64),
		YStride:        8,
		CStride:        8,
		SubsampleRatio: image.YCbCrSubsampleRatio(99),
		Rect:           image.Rect(0, 0, 8, 8),
	}

	cases := []struct {
		name    string
		w, h    uint
		img     image.Image
		interp  InterpolationFunction
		wantErr error
	}{
		{"nil image", 4, 4, nil, Bilinear, ErrNilImage},
		{"zero-width source", 4, 4, image.NewRGBA(image.Rect(0, 0, 0, 5)), Bilinear, ErrZeroDimension},
		{"zero-height source", 4, 4, image.NewGray(image.Rect(0, 0, 5, 0)), Bilinear, ErrZeroDimension},
		{"both output dims zero", 0, 0, good, Bilinear, ErrZeroDimension},
		{"requested width overflow", MaxDimension + 1, 4, good, Bilinear, ErrDimensionOverflow},
		{"requested height overflow", 4, 1 << 40, good, Bilinear, ErrDimensionOverflow},
		{"resolved dim overflow", 0, MaxDimension, image.NewRGBA(image.Rect(0, 0, 1000, 1)), Bilinear, ErrDimensionOverflow},
		{"rounding resolves to zero", 0, 1, image.NewRGBA(image.Rect(0, 0, 1, 1000)), Bilinear, ErrZeroDimension},
		{"sampler too large", 4, 4, good, InterpolationFunction(99), ErrUnsupportedSampler},
		{"sampler negative", 4, 4, good, InterpolationFunction(-1), ErrUnsupportedSampler},
		{"paletted", 4, 4, image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.Black}), Bilinear, ErrUnsupportedColorModel},
		{"alpha", 4, 4, image.NewAlpha(image.Rect(0, 0, 8, 8)), Bilinear, ErrUnsupportedColorModel},
		{"uniform", 4, 4, image.NewUniform(color.White), Bilinear, ErrUnsupportedColorModel},
		{"cmyk", 4, 4, image.NewCMYK(image.Rect(0, 0, 8, 8)), Bilinear, ErrUnsupportedColorModel},
		{"truncated RGBA", 4, 4, truncatedRGBA, Bilinear, ErrTruncatedImage},
		{"stride smaller than row", 4, 4, badStride, Bilinear, ErrTruncatedImage},
		{"truncated Gray", 4, 4, truncatedGray, Bilinear, ErrTruncatedImage},
		{"truncated YCbCr chroma", 4, 4, truncatedYCbCr, Bilinear, ErrTruncatedImage},
		{"unknown subsample ratio", 4, 4, badRatio, Bilinear, ErrUnsupportedColorModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := NewSpec(tc.w, tc.h, tc.img, tc.interp)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewSpec err = %v, want %v", err, tc.wantErr)
			}
			if spec != (Spec{}) {
				t.Fatalf("NewSpec returned non-zero Spec on error: %+v", spec)
			}
			// The pipeline must surface the same error and no image.
			img, err := NewPipeline(Options{}).Resize(context.Background(), tc.w, tc.h, tc.img, tc.interp)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Resize err = %v, want %v", err, tc.wantErr)
			}
			if img != nil {
				t.Fatalf("Resize returned non-nil image %T on error", img)
			}
		})
	}
}

func TestSpecRoundingMatchesResize(t *testing.T) {
	// Odd source dimensions exercise the historical 0.7 rounding bias.
	src := image.NewGray(image.Rect(0, 0, 13, 11))
	cases := []struct{ w, h uint }{
		{0, 7}, {9, 0}, {13, 11}, {1, 1}, {100, 3}, {5, 5},
	}
	for _, tc := range cases {
		spec, err := NewSpec(tc.w, tc.h, src, Lanczos3)
		if err != nil {
			t.Fatalf("NewSpec(%d, %d): %v", tc.w, tc.h, err)
		}
		got := Resize(tc.w, tc.h, src, Lanczos3)
		if spec.OutWidth != got.Bounds().Dx() || spec.OutHeight != got.Bounds().Dy() {
			t.Errorf("NewSpec(%d, %d) resolved %dx%d, Resize produced %v",
				tc.w, tc.h, spec.OutWidth, spec.OutHeight, got.Bounds())
		}
	}
}

func TestPipelineMatchesPublicAPI(t *testing.T) {
	interps := []InterpolationFunction{NearestNeighbor, Bilinear, Bicubic, MitchellNetravali, Lanczos2, Lanczos3}
	for name, img := range testImages() {
		for _, interp := range interps {
			for _, dims := range [][2]uint{{37, 29}, {0, 7}, {9, 0}} {
				t.Run(name, func(t *testing.T) {
					want := Resize(dims[0], dims[1], img, interp)
					got, err := NewPipeline(Options{TileHeight: 3}).Resize(context.Background(), dims[0], dims[1], img, interp)
					if err != nil {
						t.Fatalf("Resize(%d, %d, %s, %d): %v", dims[0], dims[1], name, interp, err)
					}
					wtag, wpix := pixBytes(t, want)
					gtag, gpix := pixBytes(t, got)
					if wtag != gtag {
						t.Fatalf("dims=%v interp=%d: type %s != Resize type %s", dims, interp, gtag, wtag)
					}
					if got.Bounds() != want.Bounds() {
						t.Fatalf("dims=%v interp=%d: bounds %v != %v", dims, interp, got.Bounds(), want.Bounds())
					}
					if !bytes.Equal(gpix, wpix) {
						t.Fatalf("dims=%v interp=%d: pipeline pixels differ from Resize", dims, interp)
					}
				})
			}
		}
	}
}

func TestPipelineTileOrderInvariance(t *testing.T) {
	src := gradientRGBA(64, 48)
	for _, interp := range []InterpolationFunction{NearestNeighbor, Bilinear, Lanczos2, Lanczos3} {
		ref, err := NewPipeline(Options{TileHeight: 0, Workers: 1}).Resize(context.Background(), 37, 29, src, interp)
		if err != nil {
			t.Fatal(err)
		}
		rtag, rpix := pixBytes(t, ref)
		for _, th := range []int{1, 2, 3, 5, 7, 16, 1000} {
			for _, workers := range []int{1, 2, 4, 16} {
				got, err := NewPipeline(Options{TileHeight: th, Workers: workers}).
					Resize(context.Background(), 37, 29, src, interp)
				if err != nil {
					t.Fatalf("interp=%d tile=%d workers=%d: %v", interp, th, workers, err)
				}
				gtag, gpix := pixBytes(t, got)
				if gtag != rtag || !bytes.Equal(gpix, rpix) {
					t.Fatalf("interp=%d tile=%d workers=%d: result differs from single-tile reference", interp, th, workers)
				}
			}
		}
	}
}

func TestPipelineNarrowAndTinyImages(t *testing.T) {
	for _, dims := range [][2]int{{1, 64}, {64, 1}, {1, 1}, {2, 2}} {
		src := gradientRGBA(dims[0], dims[1])
		got, err := NewPipeline(Options{TileHeight: 1}).Resize(context.Background(), 7, 5, src, Lanczos3)
		if err != nil {
			t.Fatalf("%v: %v", dims, err)
		}
		if got.Bounds() != image.Rect(0, 0, 7, 5) {
			t.Fatalf("%v: bounds %v", dims, got.Bounds())
		}
		want := Resize(7, 5, src, Lanczos3)
		_, wpix := pixBytes(t, want)
		_, gpix := pixBytes(t, got)
		if !bytes.Equal(gpix, wpix) {
			t.Fatalf("%v: pipeline differs from Resize", dims)
		}
	}
}

func TestPipelineNegativeOriginBounds(t *testing.T) {
	src := image.NewRGBA(image.Rect(-5, -3, 11, 9))
	for y := -3; y < 9; y++ {
		for x := -5; x < 11; x++ {
			i := src.PixOffset(x, y)
			src.Pix[i+0] = uint8(x*6 - y*2)
			src.Pix[i+1] = uint8(y*9 + x)
			src.Pix[i+2] = uint8(x*x + y)
			src.Pix[i+3] = 0xff
		}
	}
	got, err := NewPipeline(Options{TileHeight: 2}).Resize(context.Background(), 8, 6, src, Bilinear)
	if err != nil {
		t.Fatal(err)
	}
	// Output origin is always (0,0) regardless of the source origin.
	if got.Bounds() != image.Rect(0, 0, 8, 6) {
		t.Fatalf("bounds = %v, want (0,0)-(8,6)", got.Bounds())
	}
	want := Resize(8, 6, src, Bilinear)
	_, wpix := pixBytes(t, want)
	_, gpix := pixBytes(t, got)
	if !bytes.Equal(gpix, wpix) {
		t.Fatal("pipeline differs from Resize for negative-origin source")
	}
}

func TestPipelineFullyTransparentInput(t *testing.T) {
	// A fully transparent NRGBA with garbage RGB must produce an
	// all-zero RGBA: premultiplication zeroes every color contribution.
	src := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for i := 0; i < len(src.Pix); i += 4 {
		src.Pix[i+0] = 200
		src.Pix[i+1] = 100
		src.Pix[i+2] = 50
		src.Pix[i+3] = 0
	}
	got, err := NewPipeline(Options{}).Resize(context.Background(), 8, 8, src, Bilinear)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := got.(*image.RGBA)
	if !ok {
		t.Fatalf("NRGBA input produced %T, want *image.RGBA", got)
	}
	for i, b := range out.Pix {
		if b != 0 {
			t.Fatalf("Pix[%d] = %d, want 0 for fully transparent input", i, b)
		}
	}
}

func TestPipelineTransparentEdgesPremultiplied(t *testing.T) {
	// Opaque white center, fully transparent border with garbage RGB.
	// The border is 3px wide, wider than the bilinear kernel reach at
	// 8->4 (2 source pixels), so the corner output pixel samples only
	// fully transparent source pixels.
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			i := src.PixOffset(x, y)
			if x >= 3 && x < 6 && y >= 3 && y < 6 {
				src.Pix[i+0], src.Pix[i+1], src.Pix[i+2], src.Pix[i+3] = 255, 255, 255, 255
			} else {
				src.Pix[i+0], src.Pix[i+1], src.Pix[i+2], src.Pix[i+3] = 255, 128, 64, 0
			}
		}
	}
	got, err := NewPipeline(Options{}).Resize(context.Background(), 4, 4, src, Bilinear)
	if err != nil {
		t.Fatal(err)
	}
	out := got.(*image.RGBA)
	// The corner samples only fully transparent source pixels: with
	// premultiplication the garbage RGB contributes nothing.
	if corner := out.Pix[0:4]; !bytes.Equal(corner, []byte{0, 0, 0, 0}) {
		t.Fatalf("corner = %v, want transparent black", corner)
	}
	centerAlpha := out.Pix[out.PixOffset(2, 2)+3]
	if centerAlpha == 0 {
		t.Fatal("center alpha = 0, want > 0 near the opaque region")
	}
	// Premultiplied invariant: no color channel may exceed alpha.
	for i := 0; i < len(out.Pix); i += 4 {
		a := out.Pix[i+3]
		for c := 0; c < 3; c++ {
			if out.Pix[i+c] > a {
				t.Fatalf("pixel %d: channel %d value %d exceeds alpha %d", i/4, c, out.Pix[i+c], a)
			}
		}
	}
}

func TestPipelineExactPixelsNearest(t *testing.T) {
	// Hand-computed: 4x1 [10 20 30 40] -> 2x1 nearest averages the two
	// taps inside the kernel window: [15 35]. The vertical pass is 1:1.
	src := image.NewGray(image.Rect(0, 0, 4, 1))
	copy(src.Pix, []byte{10, 20, 30, 40})
	got, err := NewPipeline(Options{}).Resize(context.Background(), 2, 1, src, NearestNeighbor)
	if err != nil {
		t.Fatal(err)
	}
	out := got.(*image.Gray)
	if !bytes.Equal(out.Pix, []byte{15, 35}) {
		t.Fatalf("nearest 4x1->2x1 = %v, want [15 35]", out.Pix)
	}
}

func TestPipelineExactPixelsBilinear(t *testing.T) {
	// Hand-computed: 2x1 [0 255] -> 4x1 bilinear with 8-bit fixed
	// point coefficients and truncating division: [0 63 191 255].
	src := image.NewGray(image.Rect(0, 0, 2, 1))
	copy(src.Pix, []byte{0, 255})
	got, err := NewPipeline(Options{}).Resize(context.Background(), 4, 1, src, Bilinear)
	if err != nil {
		t.Fatal(err)
	}
	out := got.(*image.Gray)
	if !bytes.Equal(out.Pix, []byte{0, 63, 191, 255}) {
		t.Fatalf("bilinear 2x1->4x1 = %v, want [0 63 191 255]", out.Pix)
	}
}

func TestPipelineCancellation(t *testing.T) {
	src := gradientRGBA(256, 256)
	pipe := NewPipeline(Options{TileHeight: 2, Workers: 2})

	// Pre-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	img, err := pipe.Resize(ctx, 128, 128, src, Lanczos3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled: err = %v, want context.Canceled", err)
	}
	if img != nil {
		t.Fatalf("pre-cancelled: got non-nil image %T", img)
	}

	// Expired deadline.
	dctx, dcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer dcancel()
	if _, err := pipe.Resize(dctx, 128, 128, src, Lanczos3); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired deadline: err = %v, want context.DeadlineExceeded", err)
	}

	// Mid-stream cancellation via the Progress hook.
	mctx, mcancel := context.WithCancel(context.Background())
	defer mcancel()
	var calls int
	var mu sync.Mutex
	mpipe := NewPipeline(Options{
		TileHeight: 2,
		Workers:    3,
		Progress: func(completed, total int) {
			mu.Lock()
			calls++
			mu.Unlock()
			if completed == 3 {
				mcancel()
			}
		},
	})
	img, err = mpipe.Resize(mctx, 128, 128, src, Lanczos3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-stream: err = %v, want context.Canceled", err)
	}
	if img != nil {
		t.Fatalf("mid-stream: got non-nil image %T", img)
	}
	mu.Lock()
	if calls == 0 {
		t.Fatal("mid-stream: Progress was never called")
	}
	mu.Unlock()

	// The same pipeline must still work afterwards: no shared state
	// was corrupted by the cancelled run.
	img, err = mpipe.Resize(context.Background(), 128, 128, src, Lanczos3)
	if err != nil {
		t.Fatalf("post-cancel run: %v", err)
	}
	if img.Bounds() != image.Rect(0, 0, 128, 128) {
		t.Fatalf("post-cancel bounds = %v", img.Bounds())
	}
}

func TestPipelineBudget(t *testing.T) {
	src := gradientRGBA(64, 48)
	spec, err := NewSpec(37, 29, src, Bilinear)
	if err != nil {
		t.Fatal(err)
	}
	est := EstimateMemory(spec)
	if est <= 0 {
		t.Fatalf("EstimateMemory = %d, want > 0", est)
	}

	img, err := NewPipeline(Options{MaxMemoryBytes: est - 1}).Resize(context.Background(), 37, 29, src, Bilinear)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("budget est-1: err = %v, want ErrBudgetExhausted", err)
	}
	if img != nil {
		t.Fatalf("budget est-1: got non-nil image %T", img)
	}

	if _, err := NewPipeline(Options{MaxMemoryBytes: est}).Resize(context.Background(), 37, 29, src, Bilinear); err != nil {
		t.Fatalf("budget == estimate: unexpected err %v", err)
	}
	if _, err := NewPipeline(Options{MaxMemoryBytes: 0}).Resize(context.Background(), 37, 29, src, Bilinear); err != nil {
		t.Fatalf("unlimited budget: unexpected err %v", err)
	}
}

func TestEstimateMemoryValues(t *testing.T) {
	mk := func(img image.Image) Spec {
		spec, err := NewSpec(10, 8, img, Bilinear)
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	// 16x12 source -> 10x8 output.
	if got, want := EstimateMemory(mk(image.NewRGBA(image.Rect(0, 0, 16, 12)))), int64(4*(12*10+10*8)); got != want {
		t.Errorf("RGBA estimate = %d, want %d", got, want)
	}
	if got, want := EstimateMemory(mk(image.NewRGBA64(image.Rect(0, 0, 16, 12)))), int64(8*(12*10+10*8)); got != want {
		t.Errorf("RGBA64 estimate = %d, want %d", got, want)
	}
	if got, want := EstimateMemory(mk(image.NewGray(image.Rect(0, 0, 16, 12)))), int64(12*10+10*8); got != want {
		t.Errorf("Gray estimate = %d, want %d", got, want)
	}
	if got, want := EstimateMemory(mk(image.NewGray16(image.Rect(0, 0, 16, 12)))), int64(2*(12*10+10*8)); got != want {
		t.Errorf("Gray16 estimate = %d, want %d", got, want)
	}
	ycc := image.NewYCbCr(image.Rect(0, 0, 16, 12), image.YCbCrSubsampleRatio420)
	if got, want := EstimateMemory(mk(ycc)), int64(3*(16*12+12*10+10*8)+2*10*8); got != want {
		t.Errorf("YCbCr estimate = %d, want %d", got, want)
	}
}

func TestPipelineConcurrentNoPollution(t *testing.T) {
	// One shared Pipeline, one shared read-only source, many
	// concurrent calls: every result must equal the solo reference.
	// Run with -race to catch shared-buffer pollution.
	src := gradientRGBA(48, 36)
	pipe := NewPipeline(Options{TileHeight: 3, Workers: 4})
	ref, err := pipe.Resize(context.Background(), 20, 14, src, Lanczos3)
	if err != nil {
		t.Fatal(err)
	}
	_, rpix := pixBytes(t, ref)

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := pipe.Resize(context.Background(), 20, 14, src, Lanczos3)
			if err != nil {
				errs <- err
				return
			}
			_, gpix := pixBytes(t, got)
			if !bytes.Equal(gpix, rpix) {
				errs <- errors.New("concurrent result differs from reference")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestPipelineDoesNotMutateSource(t *testing.T) {
	src := gradientRGBA(32, 24)
	before := append([]byte(nil), src.Pix...)
	if _, err := NewPipeline(Options{TileHeight: 2}).Resize(context.Background(), 10, 8, src, Lanczos3); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(src.Pix, before) {
		t.Fatal("source image was modified by Resize")
	}
}

func TestPipelineTrivialCaseReturnsInput(t *testing.T) {
	src := gradientRGBA(16, 16)
	got, err := NewPipeline(Options{}).Resize(context.Background(), 16, 16, src, Bilinear)
	if err != nil {
		t.Fatal(err)
	}
	if got != image.Image(src) {
		t.Fatalf("same-size resize returned %T, want the input image (Resize compatibility)", got)
	}
}

func TestPipelineNilContext(t *testing.T) {
	// A nil context is treated as context.Background().
	if _, err := NewPipeline(Options{}).Resize(nil, 8, 8, gradientRGBA(4, 4), Bilinear); err != nil {
		t.Fatal(err)
	}
}

// Benchmarks: the tiled pipeline should track the public Resize
// closely (same kernels, same two-pass structure, same parallelism).
// A single-tile pipeline run is expected to be slower: one tile means
// one worker goroutine, while Resize and the tiled pipeline spread
// row bands across GOMAXPROCS workers. Run with:
//
//	go test -run=NONE -bench='Resize|Pipeline' -benchtime=100x .
//
// twice and compare: timings vary, but BenchmarkPipelineTiledLanczos3
// should stay in the same ballpark as BenchmarkResizeLanczos3.
func BenchmarkResizeLanczos3(b *testing.B) {
	src := gradientRGBA(512, 384)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Resize(256, 192, src, Lanczos3)
	}
}

func BenchmarkPipelineSingleTileLanczos3(b *testing.B) {
	src := gradientRGBA(512, 384)
	pipe := NewPipeline(Options{})
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := pipe.Resize(ctx, 256, 192, src, Lanczos3); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPipelineTiledLanczos3(b *testing.B) {
	src := gradientRGBA(512, 384)
	pipe := NewPipeline(Options{TileHeight: 16})
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := pipe.Resize(ctx, 256, 192, src, Lanczos3); err != nil {
			b.Fatal(err)
		}
	}
}
