package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	return s, &now
}

func discover(t *testing.T, s *Store, paths ...string) []File {
	t.Helper()
	ctx := context.Background()
	var d []Discovered
	for i, p := range paths {
		d = append(d, Discovered{Camera: "cam", Slot: 1, Path: p, CapturedAt: time.Date(2026, 9, 30, 10, 0, i, 0, time.UTC), GroupKey: "g-" + p})
	}
	if _, err := s.UpsertDiscovered(ctx, d); err != nil {
		t.Fatal(err)
	}
	files, err := s.DuePending(ctx, "cam", 100)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestUpsertIsIdempotent(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d := []Discovered{{Camera: "cam", Slot: 1, Path: "DCIM/100MSDCF/DSC00001.JPG", CapturedAt: time.Unix(100, 0), GroupKey: "g"}}
	n, err := s.UpsertDiscovered(ctx, d)
	if err != nil || n != 1 {
		t.Fatalf("first upsert n=%d err=%v", n, err)
	}
	n, err = s.UpsertDiscovered(ctx, d)
	if err != nil || n != 0 {
		t.Fatalf("second upsert n=%d err=%v", n, err)
	}
	// Same path with a different capture time (card reformatted) is a new file.
	d[0].CapturedAt = time.Unix(200, 0)
	if n, _ := s.UpsertDiscovered(ctx, d); n != 1 {
		t.Fatalf("reused filename should be new, n=%d", n)
	}
}

func TestHappyPath(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	f := discover(t, s, "DCIM/100MSDCF/DSC00001.JPG")[0]

	mustOK(t, s.StartDownload(ctx, f.ID))
	mustOK(t, s.FinishDownload(ctx, f.ID, "/staging/x.jpg", 1234, "abc"))
	mustOK(t, s.StartUpload(ctx, f.ID))
	mustOK(t, s.FinishUpload(ctx, f.ID, "asset-1", false))
	mustOK(t, s.MarkAlbumAdded(ctx, f.ID))
	mustOK(t, s.MarkStacked(ctx, f.ID))
	mustOK(t, s.MarkLocalDeleted(ctx, f.ID))
	mustOK(t, s.Done(ctx, f.ID))

	got, err := s.Get(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.AssetID != "asset-1" || got.SHA1 != "abc" || got.Size != 1234 ||
		!got.AlbumAdded || !got.Stacked || !got.LocalDeleted {
		t.Fatalf("unexpected row %+v", got)
	}
}

func TestInvalidTransitions(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	f := discover(t, s, "a.JPG")[0]
	for name, err := range map[string]error{
		"upload before download": s.StartUpload(ctx, f.ID),
		"finish without start":   s.FinishDownload(ctx, f.ID, "", 0, ""),
		"done before upload":     s.Done(ctx, f.ID),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s: err=%v, want ErrInvalidTransition", name, err)
		}
	}
	mustOK(t, s.StartDownload(ctx, f.ID))
	if err := s.StartDownload(ctx, f.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("double claim: err=%v", err)
	}
}

func TestFailureBackoffAndAttempts(t *testing.T) {
	s, now := openTest(t)
	ctx := context.Background()
	f := discover(t, s, "a.ARW")[0]

	mustOK(t, s.StartDownload(ctx, f.ID))
	mustOK(t, s.FailDownload(ctx, f.ID, "boom", now.Add(time.Minute), true))
	if due, _ := s.DuePending(ctx, "cam", 10); len(due) != 0 {
		t.Fatal("file should be in backoff")
	}
	*now = now.Add(2 * time.Minute)
	due, _ := s.DuePending(ctx, "cam", 10)
	if len(due) != 1 || due[0].DownloadAttempts != 1 || due[0].LastError != "boom" {
		t.Fatalf("due = %+v", due)
	}

	// A disconnect does not count as an attempt.
	mustOK(t, s.StartDownload(ctx, f.ID))
	mustOK(t, s.FailDownload(ctx, f.ID, "disconnected", *now, false))
	got, _ := s.Get(ctx, f.ID)
	if got.DownloadAttempts != 1 {
		t.Fatalf("attempts = %d", got.DownloadAttempts)
	}

	// Backoff reset on reconnect.
	mustOK(t, s.StartDownload(ctx, f.ID))
	mustOK(t, s.FailDownload(ctx, f.ID, "x", now.Add(time.Hour), true))
	mustOK(t, s.ResetBackoff(ctx, "cam"))
	if due, _ := s.DuePending(ctx, "cam", 10); len(due) != 1 {
		t.Fatal("reset backoff should make file due")
	}

	// Upload failure returns to downloaded.
	mustOK(t, s.StartDownload(ctx, f.ID))
	mustOK(t, s.FinishDownload(ctx, f.ID, "/p", 1, "h"))
	mustOK(t, s.StartUpload(ctx, f.ID))
	mustOK(t, s.FailUpload(ctx, f.ID, "immich down", now.Add(time.Minute)))
	got, _ = s.Get(ctx, f.ID)
	if got.Status != StatusDownloaded || got.UploadAttempts != 1 {
		t.Fatalf("after upload failure: %+v", got)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	files := discover(t, s, "a.JPG", "b.JPG")
	mustOK(t, s.StartDownload(ctx, files[0].ID))
	mustOK(t, s.StartDownload(ctx, files[1].ID))
	mustOK(t, s.FinishDownload(ctx, files[1].ID, "/b", 1, "h"))
	mustOK(t, s.StartUpload(ctx, files[1].ID))

	n, err := s.RecoverInterrupted(ctx)
	if err != nil || n != 2 {
		t.Fatalf("recovered %d err=%v", n, err)
	}
	a, _ := s.Get(ctx, files[0].ID)
	b, _ := s.Get(ctx, files[1].ID)
	if a.Status != StatusPending || b.Status != StatusDownloaded {
		t.Fatalf("a=%s b=%s", a.Status, b.Status)
	}
}

func TestRedownloadAndReject(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	files := discover(t, s, "a.JPG", "b.XYZ")
	for _, f := range files {
		mustOK(t, s.StartDownload(ctx, f.ID))
		mustOK(t, s.FinishDownload(ctx, f.ID, "/x", 1, "h"))
		mustOK(t, s.StartUpload(ctx, f.ID))
	}
	mustOK(t, s.Redownload(ctx, files[0].ID, "staged file missing"))
	mustOK(t, s.Reject(ctx, files[1].ID, "unsupported-format"))
	counts, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[StatusPending] != 1 || counts[StatusRejected] != 1 || counts[StatusDone] != 0 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.UpsertDiscovered(ctx, []Discovered{{Camera: "c", Slot: 2, Path: "p", CapturedAt: time.Unix(1, 0), GroupKey: "g"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.DuePending(ctx, "c", 10)
	if err != nil || len(got) != 1 || got[0].Slot != 2 {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecentErrorsAndRetryNow(t *testing.T) {
	s, now := openTest(t)
	ctx := context.Background()
	files := discover(t, s, "a.JPG", "b.JPG")
	mustOK(t, s.StartDownload(ctx, files[0].ID))
	mustOK(t, s.FailDownload(ctx, files[0].ID, "boom", now.Add(time.Hour), true))
	errs, err := s.WithErrors(ctx, 10)
	if err != nil || len(errs) != 1 || errs[0].LastError != "boom" {
		t.Fatalf("errors %+v %v", errs, err)
	}
	if recent, _ := s.Recent(ctx, 1); len(recent) != 1 {
		t.Fatalf("recent %v", recent)
	}
	n, err := s.RetryNow(ctx)
	if err != nil || n != 1 {
		t.Fatalf("retry n=%d err=%v", n, err)
	}
	if due, _ := s.DuePending(ctx, "cam", 10); len(due) != 2 {
		t.Fatalf("due after retry: %d", len(due))
	}
}

func TestSizeIsPartOfIdentityAndCameraDelete(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d := Discovered{Camera: "cam", Slot: 1, Path: "DCIM/A.JPG", CapturedAt: time.Unix(100, 0), Size: 10, GroupKey: "g"}
	if n, _ := s.UpsertDiscovered(ctx, []Discovered{d}); n != 1 {
		t.Fatal("first insert")
	}
	d2 := d
	d2.Size = 11
	if n, _ := s.UpsertDiscovered(ctx, []Discovered{d2}); n != 1 {
		t.Fatal("same path with another size is a new file")
	}
	f, ok, err := s.Lookup(ctx, d)
	if err != nil || !ok || f.Size != 10 {
		t.Fatalf("lookup: %+v %v %v", f, ok, err)
	}
	// A re-download must keep the identity.
	mustOK(t, s.StartDownload(ctx, f.ID))
	mustOK(t, s.FinishDownload(ctx, f.ID, "/x", 10, "h"))
	mustOK(t, s.StartUpload(ctx, f.ID))
	mustOK(t, s.Redownload(ctx, f.ID, "gone"))
	if _, ok, _ := s.Lookup(ctx, d); !ok {
		t.Fatal("identity lost after Redownload")
	}
	mustOK(t, s.StartDownload(ctx, f.ID))
	mustOK(t, s.FinishDownload(ctx, f.ID, "/x", 10, "h"))
	mustOK(t, s.StartUpload(ctx, f.ID))
	mustOK(t, s.FinishUpload(ctx, f.ID, "a", false))
	mustOK(t, s.Done(ctx, f.ID))
	got, _ := s.DoneNotDeleted(ctx, "cam")
	if len(got) != 1 {
		t.Fatalf("done not deleted: %v", got)
	}
	mustOK(t, s.MarkCameraDeleted(ctx, f.ID))
	if got, _ := s.DoneNotDeleted(ctx, "cam"); len(got) != 0 {
		t.Fatalf("still listed: %v", got)
	}
}
