package thumbcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHitMiss(t *testing.T) {
	c := &Cache{Dir: filepath.Join(t.TempDir(), "sub")}
	k := Key(1, "A:/x.JPG", 10, time.UnixMilli(5), "thumb")
	if _, ok := c.Get(k); ok {
		t.Fatal("unexpected hit")
	}
	if err := c.Put(k, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if b, ok := c.Get(k); !ok || string(b) != "abc" {
		t.Fatalf("%q %v", b, ok)
	}
	if k == Key(2, "A:/x.JPG", 10, time.UnixMilli(5), "thumb") || k == Key(1, "A:/x.JPG", 10, time.UnixMilli(5), "file") {
		t.Fatal("key collision")
	}
	ents, _ := os.ReadDir(c.Dir)
	if len(ents) != 1 {
		t.Fatalf("leftover temp files: %v", ents)
	}
	var nilc *Cache
	if err := nilc.Put(k, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := (&Cache{}).Get(k); ok {
		t.Fatal("empty dir must not cache")
	}
}

func TestEvict(t *testing.T) {
	c := &Cache{Dir: t.TempDir(), Max: 25}
	old := time.Now().Add(-time.Hour)
	for i, k := range []string{"a", "b"} {
		c.Put(k, make([]byte, 10))
		os.Chtimes(filepath.Join(c.Dir, k), old.Add(time.Duration(i)*time.Minute), old.Add(time.Duration(i)*time.Minute))
	}
	c.Get("a") // refreshes a, so b is now the oldest
	c.Put("c", make([]byte, 10))
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be evicted")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s evicted", k)
		}
	}
}
