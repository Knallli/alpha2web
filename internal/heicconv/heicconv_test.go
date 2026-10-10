package heicconv

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/gen2brain/h265/heic"
)

// Fixtures are encoded at test time with the decoder's own HEIC encoder (MIT,
// github.com/gen2brain/h265), so no binary files are checked in. Sony HIF is
// 10-bit 4:2:2, hence that variant.
func fixture(t testing.TB, w, h int, chroma heic.ChromaFormat, depth int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 255 / w), uint8(y * 255 / h), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := heic.Encode(&buf, img, heic.EncodeOptions{Quality: 80, Chroma: chroma, BitDepth: depth}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestToJPEG(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chroma heic.ChromaFormat
		depth  int
	}{{"8bit420", heic.Chroma420, 8}, {"10bit422", heic.Chroma422, 10}} {
		t.Run(tc.name, func(t *testing.T) {
			data := fixture(t, 320, 200, tc.chroma, tc.depth)
			for _, c := range []struct{ edge, w, h int }{{0, 320, 200}, {1000, 320, 200}, {160, 160, 100}} {
				out, err := ToJPEG(data, c.edge)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
				if err != nil || cfg.Width != c.w || cfg.Height != c.h {
					t.Fatalf("edge %d: %v %dx%d", c.edge, err, cfg.Width, cfg.Height)
				}
			}
			img, _ := jpeg.Decode(bytes.NewReader(mustJPEG(t, data)))
			// gradient: red grows with x, green with y
			r, g, _, _ := img.At(300, 20).RGBA()
			r2, g2, _, _ := img.At(20, 180).RGBA()
			if r>>8 < 200 || g>>8 > 60 || r2>>8 > 60 || g2>>8 < 150 {
				t.Fatalf("colors off: %d %d / %d %d", r>>8, g>>8, r2>>8, g2>>8)
			}
		})
	}
}

func mustJPEG(t *testing.T, data []byte) []byte {
	out, err := ToJPEG(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestToJPEGGarbage(t *testing.T) {
	good := fixture(t, 64, 64, heic.Chroma420, 8)
	for _, in := range [][]byte{nil, []byte("not an image at all"), good[:len(good)/2], append([]byte{0, 0, 0, 24}, good[4:40]...)} {
		if _, err := ToJPEG(in, 100); err == nil {
			t.Fatalf("no error for %d bytes", len(in))
		}
	}
}
