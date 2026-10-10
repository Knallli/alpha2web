// Package heicconv converts HEIF/HEIC stills (as the Sony cameras deliver
// them) to JPEG using a pure Go HEVC decoder.
package heicconv

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"sync"

	"github.com/gen2brain/h265/heic"
)

const jpegQuality = 85

// A 24 MP decode needs a few hundred MB; one at a time is plenty for a gallery.
var mu sync.Mutex

// ToJPEG decodes a HEIC image and re-encodes it as JPEG. If the long edge is
// larger than maxEdge (maxEdge > 0) the image is scaled down, keeping the aspect.
func ToJPEG(data []byte, maxEdge int) (out []byte, err error) {
	mu.Lock()
	defer mu.Unlock()
	defer func() {
		// the decoder is young; never let a hostile or odd file take the process down
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("heicconv: decoder panic: %v", r)
		}
	}()
	img, err := heic.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("heicconv: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, resample(img, maxEdge), &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resample box-averages img down to maxEdge on the long side and returns 8-bit RGBA
// (which also folds the 16-bit output of 10-bit sources to 8 bit).
func resample(img image.Image, maxEdge int) *image.RGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := sw, sh
	if m := max(sw, sh); maxEdge > 0 && m > maxEdge {
		dw, dh = max(1, sw*maxEdge/m), max(1, sh*maxEdge/m)
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	src, ok := img.(image.RGBA64Image)
	if !ok {
		src = rgba64Adapter{img}
	}
	for y := 0; y < dh; y++ {
		y0, y1 := y*sh/dh, max(y*sh/dh+1, (y+1)*sh/dh)
		for x := 0; x < dw; x++ {
			x0, x1 := x*sw/dw, max(x*sw/dw+1, (x+1)*sw/dw)
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					c := src.RGBA64At(b.Min.X+xx, b.Min.Y+yy)
					r, g, bl, a, n = r+uint64(c.R), g+uint64(c.G), bl+uint64(c.B), a+uint64(c.A), n+1
				}
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = byte(r/n>>8), byte(g/n>>8), byte(bl/n>>8), byte(a/n>>8)
		}
	}
	return dst
}

type rgba64Adapter struct{ image.Image }

func (a rgba64Adapter) RGBA64At(x, y int) color.RGBA64 {
	return color.RGBA64Model.Convert(a.At(x, y)).(color.RGBA64)
}
