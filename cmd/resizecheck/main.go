// Command resizecheck is the offline acceptance tool for the
// deterministic resize pipeline. It needs no external files: it
// generates a fixed set of minimal sample images from seeded
// generators and verifies, for each of them,
//
//   - regression: the pipeline produces byte-identical output to the
//     public Resize and Thumbnail APIs,
//   - tiling: any tile height and worker count produces byte-identical
//     output to a single-tile run,
//   - repeat: repeated runs produce byte-identical output,
//   - cancel: a cancelled context yields a nil image, a context error,
//     and no leaked goroutines,
//   - budget: a too-small memory budget fails with ErrBudgetExhausted
//     before allocating, an exactly-sufficient one succeeds,
//   - errors: invalid inputs are rejected with the documented sentinel
//     errors.
//
// Every check prints one line; the exit status is non-zero if any
// check fails. The output contains no timestamps, addresses, or
// map-order-dependent content, so two runs of the same binary produce
// identical output.
//
// Usage:
//
//	go run ./cmd/resizecheck [-tile N] [-runs N]
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"os"
	"runtime"

	"github.com/nfnt/resize"
)

var (
	tileHeight = flag.Int("tile", 7, "tile height used for the regression and repeat checks")
	runs       = flag.Int("runs", 2, "number of repetitions for the repeat check")
)

var interps = []struct {
	name   string
	interp resize.InterpolationFunction
}{
	{"NearestNeighbor", resize.NearestNeighbor},
	{"Bilinear", resize.Bilinear},
	{"Lanczos2", resize.Lanczos2},
	{"Lanczos3", resize.Lanczos3},
}

type sample struct {
	name string
	img  image.Image
}

// samples returns the minimal deterministic sample set. Every
// generator uses fixed constants or a fixed-seed PRNG.
func samples() []sample {
	return []sample{
		{"RGBA-gradient", gradientRGBA(64, 48)},
		{"NRGBA-alpha-border", alphaBorderNRGBA(32, 32)},
		{"Gray16-ramp", rampGray16(17, 13)},
		{"YCbCr-420", makeYCbCr(32, 24)},
		{"RGBA-narrow-1x64", gradientRGBA(1, 64)},
		{"RGBA-wide-64x1", gradientRGBA(64, 1)},
		{"RGBA-negative-origin", negativeOriginRGBA()},
		{"NRGBA-transparent", transparentNRGBA(16, 16)},
	}
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

// alphaBorderNRGBA returns an image with a fully transparent 4px
// border whose RGB bytes deliberately contain garbage, and an opaque
// random center. It exercises premultiplied-alpha handling at fully
// transparent edges.
func alphaBorderNRGBA(w, h int) *image.NRGBA {
	rng := rand.New(rand.NewSource(1))
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := m.PixOffset(x, y)
			m.Pix[i+0] = uint8(rng.Intn(256))
			m.Pix[i+1] = uint8(rng.Intn(256))
			m.Pix[i+2] = uint8(rng.Intn(256))
			if x < 4 || y < 4 || x >= w-4 || y >= h-4 {
				m.Pix[i+3] = 0
			} else {
				m.Pix[i+3] = 0xff
			}
		}
	}
	return m
}

func transparentNRGBA(w, h int) *image.NRGBA {
	rng := rand.New(rand.NewSource(2))
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(m.Pix); i += 4 {
		m.Pix[i+0] = uint8(rng.Intn(256))
		m.Pix[i+1] = uint8(rng.Intn(256))
		m.Pix[i+2] = uint8(rng.Intn(256))
		m.Pix[i+3] = 0
	}
	return m
}

func rampGray16(w, h int) *image.Gray16 {
	m := image.NewGray16(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.SetGray16(x, y, color.Gray16{Y: uint16(x*1000 + y*997)})
		}
	}
	return m
}

func makeYCbCr(w, h int) *image.YCbCr {
	m := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Y[y*m.YStride+x] = uint8(x*3 + y*2)
		}
	}
	for i := range m.Cb {
		m.Cb[i] = uint8(128 + i)
		m.Cr[i] = uint8(200 - i)
	}
	return m
}

func negativeOriginRGBA() *image.RGBA {
	m := image.NewRGBA(image.Rect(-5, -3, 11, 9))
	for y := -3; y < 9; y++ {
		for x := -5; x < 11; x++ {
			i := m.PixOffset(x, y)
			m.Pix[i+0] = uint8(x*6 - y*2)
			m.Pix[i+1] = uint8(y*9 + x)
			m.Pix[i+2] = uint8(x*x + y)
			m.Pix[i+3] = 0xff
		}
	}
	return m
}

// pixBytes returns a type tag and the raw pixel bytes of an image, for
// byte-exact comparison across runs.
func pixBytes(img image.Image) (string, []byte, error) {
	switch t := img.(type) {
	case *image.RGBA:
		return "RGBA", t.Pix, nil
	case *image.NRGBA:
		return "NRGBA", t.Pix, nil
	case *image.RGBA64:
		return "RGBA64", t.Pix, nil
	case *image.NRGBA64:
		return "NRGBA64", t.Pix, nil
	case *image.Gray:
		return "Gray", t.Pix, nil
	case *image.Gray16:
		return "Gray16", t.Pix, nil
	case *image.YCbCr:
		all := make([]byte, 0, len(t.Y)+len(t.Cb)+len(t.Cr))
		all = append(all, t.Y...)
		all = append(all, t.Cb...)
		all = append(all, t.Cr...)
		return "YCbCr", all, nil
	}
	return "", nil, fmt.Errorf("unexpected image type %T", img)
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:4])
}

type checker struct {
	passed, failed int
}

func (c *checker) ok(name, detail string) {
	c.passed++
	fmt.Printf("ok    %s %s\n", name, detail)
}

func (c *checker) fail(name, detail string) {
	c.failed++
	fmt.Printf("FAIL  %s %s\n", name, detail)
}

// check runs fn and records the outcome. fn returns a detail string
// on success or an error on failure.
func (c *checker) check(name string, fn func() (string, error)) {
	detail, err := fn()
	if err != nil {
		c.fail(name, err.Error())
		return
	}
	c.ok(name, detail)
}

func mustPix(img image.Image) (string, []byte) {
	tag, pix, err := pixBytes(img)
	if err != nil {
		return "ERROR", []byte(err.Error())
	}
	return tag, pix
}

func main() {
	flag.Parse()
	c := &checker{}
	pipe := resize.NewPipeline(resize.Options{TileHeight: *tileHeight})
	ctx := context.Background()

	for _, s := range samples() {
		s := s
		for _, ip := range interps {
			ip := ip
			// Regression against the public Resize API.
			c.check(fmt.Sprintf("regression/%s/%s", s.name, ip.name), func() (string, error) {
				want := resize.Resize(37, 29, s.img, ip.interp)
				got, err := pipe.Resize(ctx, 37, 29, s.img, ip.interp)
				if err != nil {
					return "", err
				}
				wtag, wpix := mustPix(want)
				gtag, gpix := mustPix(got)
				if wtag != gtag {
					return "", fmt.Errorf("type mismatch: Resize=%s pipeline=%s", wtag, gtag)
				}
				if got.Bounds() != want.Bounds() {
					return "", fmt.Errorf("bounds mismatch: Resize=%v pipeline=%v", want.Bounds(), got.Bounds())
				}
				if !bytes.Equal(wpix, gpix) {
					return "", fmt.Errorf("pixel mismatch: Resize hash=%s pipeline hash=%s",
						shortHash(wpix), shortHash(gpix))
				}
				return fmt.Sprintf("out=%dx%d type=%s bytes=%d hash=%s",
					got.Bounds().Dx(), got.Bounds().Dy(), gtag, len(gpix), shortHash(gpix)), nil
			})

			// Regression against the public Thumbnail API.
			c.check(fmt.Sprintf("thumbnail/%s/%s", s.name, ip.name), func() (string, error) {
				want := resize.Thumbnail(40, 40, s.img, ip.interp)
				w := uint(want.Bounds().Dx())
				h := uint(want.Bounds().Dy())
				got, err := pipe.Resize(ctx, w, h, s.img, ip.interp)
				if err != nil {
					return "", err
				}
				wtag, wpix := mustPix(want)
				gtag, gpix := mustPix(got)
				if wtag != gtag || !bytes.Equal(wpix, gpix) {
					return "", fmt.Errorf("thumbnail mismatch: Resize hash=%s pipeline hash=%s",
						shortHash(wpix), shortHash(gpix))
				}
				return fmt.Sprintf("out=%dx%d hash=%s", w, h, shortHash(gpix)), nil
			})
		}

		// Tiling invariance: several tile heights and worker counts
		// must reproduce the single-tile result byte for byte.
		c.check(fmt.Sprintf("tiling/%s", s.name), func() (string, error) {
			ref, err := resize.NewPipeline(resize.Options{TileHeight: 0, Workers: 1}).
				Resize(ctx, 37, 29, s.img, resize.Lanczos3)
			if err != nil {
				return "", err
			}
			rtag, rpix := mustPix(ref)
			for _, th := range []int{1, 3, 7, 1000} {
				for _, workers := range []int{1, 2, 8} {
					got, err := resize.NewPipeline(resize.Options{TileHeight: th, Workers: workers}).
						Resize(ctx, 37, 29, s.img, resize.Lanczos3)
					if err != nil {
						return "", err
					}
					gtag, gpix := mustPix(got)
					if gtag != rtag || !bytes.Equal(gpix, rpix) {
						return "", fmt.Errorf("tile=%d workers=%d: hash=%s, want %s",
							th, workers, shortHash(gpix), shortHash(rpix))
					}
				}
			}
			return fmt.Sprintf("tileHeights={1,3,7,1000} workers={1,2,8} hash=%s", shortHash(rpix)), nil
		})

		// Repeat determinism.
		c.check(fmt.Sprintf("repeat/%s", s.name), func() (string, error) {
			var ref string
			for run := 0; run < *runs; run++ {
				got, err := pipe.Resize(ctx, 37, 29, s.img, resize.Bilinear)
				if err != nil {
					return "", err
				}
				_, pix := mustPix(got)
				h := shortHash(pix)
				if run == 0 {
					ref = h
				} else if h != ref {
					return "", fmt.Errorf("run %d hash=%s, run 0 hash=%s", run, h, ref)
				}
			}
			return fmt.Sprintf("runs=%d hash=%s", *runs, ref), nil
		})
	}

	// Cancellation: pre-cancelled context and mid-stream cancellation
	// via the Progress hook must both fail cleanly.
	c.check("cancel/pre-cancelled", func() (string, error) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		img, err := pipe.Resize(ctx, 64, 64, gradientRGBA(128, 128), resize.Lanczos3)
		if !errors.Is(err, context.Canceled) {
			return "", fmt.Errorf("err=%v, want context.Canceled", err)
		}
		if img != nil {
			return "", errors.New("returned a non-nil image on cancellation")
		}
		return "err=context.Canceled image=nil", nil
	})

	c.check("cancel/mid-stream", func() (string, error) {
		before := runtime.NumGoroutine()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := resize.NewPipeline(resize.Options{
			TileHeight: 4,
			Workers:    2,
			Progress: func(completed, total int) {
				if completed == 3 {
					cancel()
				}
			},
		})
		img, err := p.Resize(ctx, 256, 256, gradientRGBA(512, 512), resize.Lanczos3)
		if !errors.Is(err, context.Canceled) {
			return "", fmt.Errorf("err=%v, want context.Canceled", err)
		}
		if img != nil {
			return "", errors.New("returned a non-nil image on cancellation")
		}
		after := runtime.NumGoroutine()
		if after > before {
			return "", fmt.Errorf("goroutines leaked: before=%d after=%d", before, after)
		}
		return fmt.Sprintf("err=context.Canceled image=nil goroutines=%d->%d", before, after), nil
	})

	// Memory budget: one byte under the estimate must fail before any
	// allocation; exactly the estimate must succeed.
	c.check("budget/exhausted-and-exact", func() (string, error) {
		src := gradientRGBA(64, 48)
		spec, err := resize.NewSpec(37, 29, src, resize.Bilinear)
		if err != nil {
			return "", err
		}
		est := resize.EstimateMemory(spec)
		tight := resize.NewPipeline(resize.Options{MaxMemoryBytes: est - 1})
		if img, err := tight.Resize(ctx, 37, 29, src, resize.Bilinear); !errors.Is(err, resize.ErrBudgetExhausted) || img != nil {
			return "", fmt.Errorf("budget=%d: img=%v err=%v, want nil + ErrBudgetExhausted", est-1, img != nil, err)
		}
		exact := resize.NewPipeline(resize.Options{MaxMemoryBytes: est})
		if _, err := exact.Resize(ctx, 37, 29, src, resize.Bilinear); err != nil {
			return "", fmt.Errorf("budget=%d (exact estimate): unexpected err=%v", est, err)
		}
		return fmt.Sprintf("estimate=%d under=ErrBudgetExhausted exact=ok", est), nil
	})

	// Input validation: each bad input must fail with its documented
	// sentinel error and a nil image.
	c.check("errors/sentinels", func() (string, error) {
		good := gradientRGBA(8, 8)
		truncated := &image.RGBA{Pix: make([]byte, 10), Stride: 4 * 8, Rect: image.Rect(0, 0, 8, 8)}
		paletted := image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.Black, color.White})
		cases := []struct {
			name    string
			w, h    uint
			img     image.Image
			interp  resize.InterpolationFunction
			wantErr error
		}{
			{"nil-image", 4, 4, nil, resize.Bilinear, resize.ErrNilImage},
			{"zero-source", 4, 4, image.NewRGBA(image.Rect(0, 0, 0, 0)), resize.Bilinear, resize.ErrZeroDimension},
			{"both-zero", 0, 0, good, resize.Bilinear, resize.ErrZeroDimension},
			{"overflow", resize.MaxDimension + 1, 4, good, resize.Bilinear, resize.ErrDimensionOverflow},
			{"bad-sampler-high", 4, 4, good, resize.InterpolationFunction(99), resize.ErrUnsupportedSampler},
			{"bad-sampler-negative", 4, 4, good, resize.InterpolationFunction(-1), resize.ErrUnsupportedSampler},
			{"paletted", 4, 4, paletted, resize.Bilinear, resize.ErrUnsupportedColorModel},
			{"truncated", 4, 4, truncated, resize.Bilinear, resize.ErrTruncatedImage},
		}
		for _, tc := range cases {
			img, err := pipe.Resize(ctx, tc.w, tc.h, tc.img, tc.interp)
			if !errors.Is(err, tc.wantErr) {
				return "", fmt.Errorf("%s: err=%v, want %v", tc.name, err, tc.wantErr)
			}
			if img != nil {
				return "", fmt.Errorf("%s: got non-nil image on error", tc.name)
			}
		}
		return fmt.Sprintf("%d cases rejected with documented sentinels", len(cases)), nil
	})

	fmt.Printf("summary: passed=%d failed=%d\n", c.passed, c.failed)
	if c.failed > 0 {
		os.Exit(1)
	}
}
