package export

import (
	"path/filepath"
	"testing"
	"time"
)

var when = time.Date(2025, 3, 7, 9, 5, 2, 0, time.UTC)

func item(p string) Item {
	return Item{Camera: "ILCE-6700", Serial: "S/N 1", Slot: 2, When: when, Path: p, Seq: 7}
}

func TestExpand(t *testing.T) {
	for _, c := range []struct{ tpl, cas, path, want string }{
		{DefaultTemplate, "keep", `A:\DCIM\100MSDCF\DSC01234.JPG`, "ILCE-6700/2025/2025-03-07/DSC01234.JPG"},
		{DefaultTemplate, "lower", "A:/DCIM/100MSDCF/DSC01234.ARW", "ILCE-6700/2025/2025-03-07/dsc01234.arw"},
		{"{date}/{hour}{minute}{second}_{name}{ext}", "upper", "A:/DCIM/100MSDCF/dsc1.hif", "2025-03-07/090502_DSC1.HIF"},
		{"{camera}/{serial}/slot{slot}/{folder}/{type}/{seq}{ext}", "keep", "A:/DCIM/100MSDCF/DSC1.ARW", "ILCE-6700/S_N_1/slot2/100MSDCF/RAW/0007.ARW"},
		{"{type}-{name}{ext}", "keep", "A:/PRIVATE/M4ROOT/CLIP/C0001.MP4", "VIDEO-C0001.MP4"},
		{"{year}/{month}/{day}/{name}{ext}", "keep", "A:/X/a.TXT", "2025/03/07/a.TXT"},
		// literal parts and values cannot climb out
		{"../x/{name}", "keep", "A:/DCIM/a.JPG", "x/a"},
		{"/abs//{name}{ext}", "keep", "A:/DCIM/a.JPG", "abs/a.JPG"},
		{`a\..\{name}`, "keep", "A:/DCIM/a.JPG", "a/a"},
		{"{name}{ext}", "keep", `A:\..\..\etc\pass:wd?.JPG`, "pass_wd_.JPG"},
		{"{folder}/{name}", "keep", "A:/../..", "_"},
		{"..", "keep", "A:/a.JPG", ""},
	} {
		if got := Expand(c.tpl, c.cas, item(c.path)); got != c.want {
			t.Errorf("%q %s %q: got %q want %q", c.tpl, c.cas, c.path, got, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	for tpl, ok := range map[string]bool{
		DefaultTemplate: true, "{seq}": true, "{date}/{name}": true,
		"{foo}/{name}": false, "{year}": false, "../{name}": false, "/a/{name}": false, `C:\{name}`: false,
		"{name": false, "a/..\\b/{name}": false, "a..b/{name}": true,
	} {
		if err := Validate(tpl); (err == nil) != ok {
			t.Errorf("%q: %v", tpl, err)
		}
	}
}

func TestNew(t *testing.T) {
	c := New(" /x ", "", "weird", "", "", "")
	if c.Dir != "/x" || c.Template != DefaultTemplate || c.Case != "keep" || c.Conflict != Suffix || !c.SetMtime || len(c.Types) != 5 {
		t.Fatalf("%+v", c)
	}
	c = New("", "t", "lower", "jpeg, raw,bogus,JPEG", "overwrite", "false")
	if c.Case != "lower" || c.Conflict != Overwrite || c.SetMtime || len(c.Types) != 2 || !c.Wants("RAW") || c.Wants("VIDEO") {
		t.Fatalf("%+v", c)
	}
}

func TestTarget(t *testing.T) {
	files := map[string]int64{}
	stat := func(p string) (int64, bool) { n, ok := files[p]; return n, ok }
	at := func(s string) string { return filepath.FromSlash("/d/" + s) }
	c := New("/d", "", "", "", "", "")
	if p, skip := c.Target("a/x.JPG", 5, stat); p != at("a/x.JPG") || skip {
		t.Fatal(p, skip)
	}
	files[at("a/x.JPG")] = 5
	if p, skip := c.Target("a/x.JPG", 5, stat); p != at("a/x.JPG") || !skip {
		t.Fatal("same size must skip", p, skip)
	}
	if p, skip := c.Target("a/x.JPG", 6, stat); p != at("a/x_1.JPG") || skip {
		t.Fatal("other size gets a suffix", p, skip)
	}
	files[at("a/x_1.JPG")] = 6
	if p, skip := c.Target("a/x.JPG", 6, stat); p != at("a/x_1.JPG") || !skip {
		t.Fatal("suffix copy of the same size is the file", p, skip)
	}
	if p, skip := c.Target("a/x.JPG", 7, stat); p != at("a/x_2.JPG") || skip {
		t.Fatal(p, skip)
	}
	c.Conflict = AlwaysSuffix
	if p, skip := c.Target("a/x.JPG", 5, stat); p != at("a/x_2.JPG") || skip {
		t.Fatal(p, skip)
	}
	c.Conflict = Overwrite
	if p, skip := c.Target("a/x.JPG", 5, stat); p != at("a/x.JPG") || skip {
		t.Fatal(p, skip)
	}
	files[at("a/x.JPG")] = Taken
	if p, skip := c.Target("a/x.JPG", 5, stat); p != at("a/x_1.JPG") || skip {
		t.Fatal("overwrite must not replace another shot's file", p, skip)
	}
	c.Conflict = Suffix
	if p, skip := c.Target("a/x.JPG", 5, stat); p != at("a/x_2.JPG") || skip {
		t.Fatal("a taken file is never the same shot", p, skip)
	}
}
