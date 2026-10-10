// Package verify checks that a downloaded camera file is complete and
// computes its SHA-1 (the checksum Immich uses for deduplication).
//
// The caller compares the size against what the camera listed; beyond that
// completeness is checked structurally per format:
//   - JPEG: starts with SOI (FFD8) and has an EOI (FFD9) near the end
//   - ARW/TIFF: valid TIFF header and first IFD offset inside the file
//   - MP4/MOV/HEIF: top-level ISO-BMFF boxes add up exactly to the file size
//
// Unknown formats only need to be non-empty.
package verify

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrIncomplete reports a truncated or malformed file.
var ErrIncomplete = errors.New("file incomplete or corrupt")

// Result of a successful verification.
type Result struct {
	Size int64
	SHA1 string // lowercase hex
}

// File verifies path. formatName is the original camera filename and selects
// the format check by extension (the staged file may have a temporary name).
func File(path, formatName string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Result{}, err
	}
	size := st.Size()
	if size == 0 {
		return Result{}, fmt.Errorf("%w: empty file", ErrIncomplete)
	}

	var check func(io.ReaderAt, int64) error
	switch strings.ToUpper(filepath.Ext(formatName)) {
	case ".JPG", ".JPEG":
		check = checkJPEG
	case ".ARW", ".TIF", ".TIFF", ".DNG":
		check = checkTIFF
	case ".MP4", ".MOV", ".M4V", ".HIF", ".HEIF", ".HEIC":
		check = checkBMFF
	}
	if check != nil {
		if err := check(f, size); err != nil {
			return Result{}, err
		}
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}
	h := sha1.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Result{}, err
	}
	if n != size {
		return Result{}, fmt.Errorf("%w: size changed while hashing", ErrIncomplete)
	}
	return Result{Size: size, SHA1: hex.EncodeToString(h.Sum(nil))}, nil
}

func checkJPEG(r io.ReaderAt, size int64) error {
	head := make([]byte, 2)
	if _, err := r.ReadAt(head, 0); err != nil || head[0] != 0xFF || head[1] != 0xD8 {
		return fmt.Errorf("%w: missing JPEG SOI marker", ErrIncomplete)
	}
	// Some encoders pad after EOI; search the tail.
	tailLen := int64(64 << 10)
	if tailLen > size {
		tailLen = size
	}
	tail := make([]byte, tailLen)
	if _, err := r.ReadAt(tail, size-tailLen); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	i := bytes.LastIndex(tail, []byte{0xFF, 0xD9})
	if i < 0 || len(bytes.Trim(tail[i+2:], "\x00\xff")) != 0 {
		return fmt.Errorf("%w: missing JPEG EOI marker", ErrIncomplete)
	}
	return nil
}

func checkTIFF(r io.ReaderAt, size int64) error {
	head := make([]byte, 8)
	if _, err := r.ReadAt(head, 0); err != nil {
		return fmt.Errorf("%w: short TIFF header", ErrIncomplete)
	}
	var order binary.ByteOrder
	switch {
	case bytes.Equal(head[:4], []byte{'I', 'I', 0x2A, 0x00}):
		order = binary.LittleEndian
	case bytes.Equal(head[:4], []byte{'M', 'M', 0x00, 0x2A}):
		order = binary.BigEndian
	default:
		return fmt.Errorf("%w: bad TIFF header", ErrIncomplete)
	}
	if off := int64(order.Uint32(head[4:])); off < 8 || off >= size {
		return fmt.Errorf("%w: TIFF IFD offset beyond end of file", ErrIncomplete)
	}
	return nil
}

func checkBMFF(r io.ReaderAt, size int64) error {
	var off int64
	hdr := make([]byte, 16)
	for off < size {
		if size-off < 8 {
			return fmt.Errorf("%w: trailing bytes after last box", ErrIncomplete)
		}
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return fmt.Errorf("%w: %v", ErrIncomplete, err)
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		switch boxSize {
		case 0: // box extends to end of file
			return nil
		case 1: // 64-bit size follows the type
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return fmt.Errorf("%w: %v", ErrIncomplete, err)
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			if boxSize < 16 {
				return fmt.Errorf("%w: bad box size", ErrIncomplete)
			}
		default:
			if boxSize < 8 {
				return fmt.Errorf("%w: bad box size", ErrIncomplete)
			}
		}
		if off == 0 && string(hdr[4:8]) != "ftyp" {
			return fmt.Errorf("%w: missing ftyp box", ErrIncomplete)
		}
		off += boxSize
	}
	if off != size {
		return fmt.Errorf("%w: last box ends at %d, file is %d bytes", ErrIncomplete, off, size)
	}
	return nil
}
