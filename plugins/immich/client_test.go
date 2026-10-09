package immich_test

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/knallli/alpha2web/plugins/immich"
	"github.com/knallli/alpha2web/plugins/immich/immichtest"
)

func tempFile(t *testing.T, name string, data []byte) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(data)
	return p, hex.EncodeToString(sum[:])
}

func TestURLNormalisation(t *testing.T) {
	srv := immichtest.New(t)
	for _, base := range []string{srv.URL, srv.URL + "/", srv.URL + "/api", srv.URL + "/api/"} {
		v, err := immich.New(base, srv.APIKey, "test").CheckCompatible(context.Background())
		if err != nil || v.Major != 3 {
			t.Fatalf("%s: v=%v err=%v", base, v, err)
		}
	}
}

func TestRejectsOldServer(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"major":2,"minor":7,"patch":0}`))
	}))
	defer old.Close()
	if _, err := immich.New(old.URL, "k", "").CheckCompatible(context.Background()); err == nil {
		t.Fatal("expected incompatibility error for v2")
	}
}

func TestUploadFlowAndDedupe(t *testing.T) {
	srv := immichtest.New(t)
	c := immich.New(srv.URL, srv.APIKey, "test")
	ctx := context.Background()
	path, sum := tempFile(t, "DSC00001.JPG", []byte("jpeg-bytes"))

	res, err := c.BulkUploadCheck(ctx, []immich.CheckItem{{ID: "DSC00001.JPG", Checksum: sum}})
	if err != nil || len(res) != 1 || res[0].Action != "accept" {
		t.Fatalf("check: %+v %v", res, err)
	}
	created := time.Date(2026, 9, 30, 10, 11, 12, 345e6, time.UTC)
	up, err := c.Upload(ctx, immich.UploadRequest{Path: path, Filename: "DSC00001.JPG", SHA1: sum, CreatedAt: created, ModifiedAt: created})
	if err != nil || up.Duplicate || up.ID == "" {
		t.Fatalf("upload: %+v %v", up, err)
	}
	a, _ := srv.AssetByID(up.ID)
	if a.Filename != "DSC00001.JPG" || a.CreatedAt != "2026-09-30T10:11:12.345Z" || a.Checksum != sum {
		t.Fatalf("stored asset %+v", a)
	}

	res, _ = c.BulkUploadCheck(ctx, []immich.CheckItem{{ID: "x", Checksum: sum}})
	if res[0].Action != "reject" || res[0].Reason != "duplicate" || res[0].AssetID != up.ID {
		t.Fatalf("expected duplicate, got %+v", res)
	}
	again, err := c.Upload(ctx, immich.UploadRequest{Path: path, Filename: "DSC00001.JPG", SHA1: sum, CreatedAt: created, ModifiedAt: created})
	if err != nil || !again.Duplicate || again.ID != up.ID {
		t.Fatalf("re-upload: %+v %v", again, err)
	}
}

func TestErrorsClassified(t *testing.T) {
	srv := immichtest.New(t)
	ctx := context.Background()
	_, err := immich.New(srv.URL, "wrong", "").BulkUploadCheck(ctx, []immich.CheckItem{{ID: "a", Checksum: "b"}})
	if !immich.IsAuthError(err) || immich.IsClientError(err) {
		t.Fatalf("want auth error, got %v", err)
	}
	srv.FailNext(1)
	_, err = immich.New(srv.URL, srv.APIKey, "").BulkUploadCheck(ctx, []immich.CheckItem{{ID: "a", Checksum: "b"}})
	if err == nil || immich.IsClientError(err) || immich.IsAuthError(err) {
		t.Fatalf("want 503 server error, got %v", err)
	}
}

func TestAlbumAndStack(t *testing.T) {
	srv := immichtest.New(t)
	c := immich.New(srv.URL, srv.APIKey, "")
	ctx := context.Background()
	id1, err := c.FindOrCreateAlbum(ctx, "Sony a6700")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := c.FindOrCreateAlbum(ctx, "Sony a6700")
	if err != nil || id1 != id2 {
		t.Fatalf("album not reused: %s %s %v", id1, id2, err)
	}
	res, err := c.AddToAlbum(ctx, id1, []string{"a1", "a2"})
	if err != nil || len(res) != 2 || !res[0].Success {
		t.Fatalf("add: %+v %v", res, err)
	}
	res, _ = c.AddToAlbum(ctx, id1, []string{"a1"})
	if res[0].Success || res[0].Error != "duplicate" {
		t.Fatalf("re-add should report duplicate: %+v", res)
	}
	if _, err := c.CreateStack(ctx, []string{"a1", "a2"}); err != nil {
		t.Fatal(err)
	}
	if s := srv.Stacks(); len(s) != 1 || s[0][0] != "a1" {
		t.Fatalf("stacks %v", s)
	}
}

func TestValidateKey(t *testing.T) {
	srv := immichtest.New(t)
	ctx := context.Background()
	if err := immich.New(srv.URL, srv.APIKey, "").ValidateKey(ctx); err != nil {
		t.Fatal(err)
	}
	if err := immich.New(srv.URL, "bad", "").ValidateKey(ctx); !immich.IsAuthError(err) {
		t.Fatalf("bad key should fail, got %v", err)
	}
}
