package verify

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.part")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func box(typ string, payload int) []byte {
	b := make([]byte, 8+payload)
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	return b
}

func TestFormats(t *testing.T) {
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE1}, make([]byte, 100)...)
	jpeg = append(jpeg, 0xFF, 0xD9)
	mp4 := append(append(box("ftyp", 16), box("moov", 40)...), box("mdat", 1000)...)
	tiff := append([]byte{'I', 'I', 0x2A, 0, 8, 0, 0, 0}, make([]byte, 64)...)

	cases := []struct {
		name, ext string
		data      []byte
		ok        bool
	}{
		{"jpeg ok", "DSC1.JPG", jpeg, true},
		{"jpeg padded", "DSC1.JPG", append(append([]byte{}, jpeg...), 0, 0, 0), true},
		{"jpeg truncated", "DSC1.JPG", jpeg[:50], false},
		{"jpeg no soi", "DSC1.JPG", append([]byte{0, 0}, jpeg...), false},
		{"mp4 ok", "C0001.MP4", mp4, true},
		{"mp4 truncated", "C0001.MP4", mp4[:len(mp4)-10], false},
		{"mp4 no ftyp", "C0001.MP4", box("mdat", 10), false},
		{"heif ok", "DSC1.HIF", mp4, true},
		{"arw ok", "DSC1.ARW", tiff, true},
		{"arw bad offset", "DSC1.ARW", []byte{'I', 'I', 0x2A, 0, 0xFF, 0xFF, 0, 0, 1, 2}, false},
		{"arw not tiff", "DSC1.ARW", []byte("hello world"), false},
		{"unknown ok", "notes.BIN", []byte("x"), true},
		{"empty", "DSC1.JPG", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := File(write(t, c.data), c.ext)
			if c.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				sum := sha1.Sum(c.data)
				if res.SHA1 != hex.EncodeToString(sum[:]) || res.Size != int64(len(c.data)) {
					t.Fatalf("bad result %+v", res)
				}
			} else if !errors.Is(err, ErrIncomplete) {
				t.Fatalf("want ErrIncomplete, got %v", err)
			}
		})
	}
}

func TestBMFFLargeBox(t *testing.T) {
	ftyp := box("ftyp", 8)
	large := make([]byte, 16+32)
	binary.BigEndian.PutUint32(large, 1)
	copy(large[4:], "mdat")
	binary.BigEndian.PutUint64(large[8:], uint64(len(large)))
	if _, err := File(write(t, append(ftyp, large...)), "a.mp4"); err != nil {
		t.Fatal(err)
	}
}
