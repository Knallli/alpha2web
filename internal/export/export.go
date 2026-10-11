// Package export decides where gallery files are copied: the path template,
// file name case, file type filter and conflict handling. It is pure; the
// gallery does the I/O.
package export

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultTemplate = "{camera}/{year}/{year}-{month}-{day}/{name}{ext}"

// Conflict modes: what to do when the target file exists.
const (
	Suffix       = "suffix"        // same size = already there, else add _1, _2 ...
	AlwaysSuffix = "always-suffix" // never touch an existing file
	Overwrite    = "overwrite"
)

var (
	Types = []string{"JPEG", "HEIF", "RAW", "VIDEO", "OTHER"}
	Cases = []string{"keep", "lower", "upper"}
)

// Config is the effective export setup.
type Config struct {
	Dir, Template, Case, Conflict string
	Types                         []string // exported file types, see Type
	SetMtime                      bool     // set the modification time to the capture time
}

// New builds a Config from raw setting strings; empty or unknown values give
// the defaults. types is a comma list, mtime "false"/"0"/"off" turns it off.
func New(dir, template, cas, types, conflict, mtime string) Config {
	c := Config{Dir: strings.TrimSpace(dir), Template: strings.TrimSpace(template), Case: cas, Conflict: conflict, SetMtime: true}
	if c.Template == "" {
		c.Template = DefaultTemplate
	}
	if !contains(Cases, c.Case) {
		c.Case = "keep"
	}
	if c.Conflict != AlwaysSuffix && c.Conflict != Overwrite {
		c.Conflict = Suffix
	}
	for _, t := range strings.Split(types, ",") {
		if t = strings.ToUpper(strings.TrimSpace(t)); contains(Types, t) && !contains(c.Types, t) {
			c.Types = append(c.Types, t)
		}
	}
	if len(c.Types) == 0 {
		c.Types = Types
	}
	switch strings.ToLower(strings.TrimSpace(mtime)) {
	case "false", "0", "off", "no":
		c.SetMtime = false
	}
	return c
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Wants reports whether files of the type are exported.
func (c Config) Wants(typ string) bool { return contains(c.Types, typ) }

// Type classifies a file extension (any case, with or without dot).
func Type(ext string) string {
	switch strings.ToUpper(strings.TrimPrefix(ext, ".")) {
	case "JPG", "JPEG":
		return "JPEG"
	case "HIF", "HEIF", "HEIC":
		return "HEIF"
	case "ARW", "SR2", "SRF", "DNG", "RAW":
		return "RAW"
	case "MP4", "MOV", "MXF", "XAVC", "MTS":
		return "VIDEO"
	}
	return "OTHER"
}

// Item is what a template can refer to.
type Item struct {
	Camera, Serial string
	Slot           int
	When           time.Time // camera wall-clock time, stored as UTC
	Path           string    // camera path, e.g. "A:\DCIM\100MSDCF\DSC00001.JPG"
	Seq            int
}

var token = regexp.MustCompile(`\{([a-z]+)\}`)

var known = map[string]bool{"camera": true, "serial": true, "slot": true, "year": true, "month": true, "day": true, "hour": true,
	"minute": true, "second": true, "date": true, "folder": true, "name": true, "ext": true, "type": true, "seq": true}

// Validate checks a template: known tokens only, a file name token, and no
// absolute path or ".." that could leave the target folder.
func Validate(tpl string) error {
	for _, m := range token.FindAllStringSubmatch(tpl, -1) {
		if !known[m[1]] {
			return fmt.Errorf("unknown token {%s}", m[1])
		}
	}
	if rest := token.ReplaceAllString(tpl, ""); strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("unbalanced { }")
	}
	if !strings.Contains(tpl, "{name}") && !strings.Contains(tpl, "{seq}") {
		return fmt.Errorf("must contain {name} or {seq}")
	}
	lit := strings.ReplaceAll(tpl, "\\", "/")
	if strings.HasPrefix(lit, "/") || regexp.MustCompile(`^[A-Za-z]:`).MatchString(lit) {
		return fmt.Errorf("must be relative to the folder")
	}
	for _, seg := range strings.Split(lit, "/") {
		if seg == ".." {
			return fmt.Errorf("must not contain ..")
		}
	}
	return nil
}

var (
	illegal = regexp.MustCompile(`[\x00-\x1f<>:"|?*/\\]`)
	idChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

// Expand fills the template and returns a clean relative slash path that
// cannot leave the target folder; empty if nothing usable is left.
func Expand(tpl, cas string, it Item) string {
	p := strings.ReplaceAll(it.Path, "\\", "/")
	p = regexp.MustCompile(`^[A-Za-z]:`).ReplaceAllString(p, "")
	base := path.Base(p)
	ext := path.Ext(base)
	name := strings.TrimSuffix(base, ext)
	d2 := func(n int) string { return fmt.Sprintf("%02d", n) }
	w := it.When
	vals := map[string]string{
		"camera": idChars.ReplaceAllString(it.Camera, "_"), "serial": idChars.ReplaceAllString(it.Serial, "_"), "slot": strconv.Itoa(it.Slot),
		"year": fmt.Sprintf("%04d", w.Year()), "month": d2(int(w.Month())), "day": d2(w.Day()), "hour": d2(w.Hour()), "minute": d2(w.Minute()), "second": d2(w.Second()),
		"date": w.Format("2006-01-02"), "folder": illegal.ReplaceAllString(path.Base(path.Dir(p)), "_"), "name": illegal.ReplaceAllString(name, "_"),
		"ext": illegal.ReplaceAllString(ext, "_"), "type": Type(ext), "seq": fmt.Sprintf("%04d", it.Seq),
	}
	out := token.ReplaceAllStringFunc(strings.ReplaceAll(tpl, "\\", "/"), func(s string) string { return vals[s[1:len(s)-1]] })
	var segs []string
	for _, s := range strings.Split(out, "/") {
		if s = strings.TrimSpace(s); s != "" && s != "." && s != ".." {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return ""
	}
	last := &segs[len(segs)-1]
	switch cas {
	case "lower":
		*last = strings.ToLower(*last)
	case "upper":
		*last = strings.ToUpper(*last)
	}
	return strings.Join(segs, "/")
}

// Taken is what stat reports for a path that belongs to another shot: it is
// never skipped onto or overwritten.
const Taken = -1

// Target picks the file to write for rel under dir. stat returns the size of
// an existing file, Taken, or another value that never equals size when the
// file is not known to be this shot. skip means the target already holds this
// file.
func (c Config) Target(rel string, size int64, stat func(string) (int64, bool)) (target string, skip bool) {
	target = filepath.Join(c.Dir, filepath.FromSlash(rel))
	ext := filepath.Ext(target)
	stem := strings.TrimSuffix(target, ext)
	for i := 0; ; i++ {
		cand := target
		if i > 0 {
			cand = stem + "_" + strconv.Itoa(i) + ext
		}
		n, ok := stat(cand)
		switch {
		case !ok:
			return cand, false
		case n == Taken:
		case c.Conflict == Overwrite:
			return cand, false
		case c.Conflict == Suffix && n == size:
			return cand, true
		}
	}
}
