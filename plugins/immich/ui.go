package immich

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/knallli/alpha2web/plugin"
	"github.com/knallli/alpha2web/plugins/immich/store"
)

var (
	_ plugin.StatusProvider    = (*Plugin)(nil)
	_ plugin.FileStateProvider = (*Plugin)(nil)
	_ plugin.ActionProvider    = (*Plugin)(nil)
	_ plugin.MetricsProvider   = (*Plugin)(nil)
	_ plugin.HealthProvider    = (*Plugin)(nil)
)

const uiTimeout = 5 * time.Second

// requiredPermissions are the API key permissions the current settings need.
func requiredPermissions(s settings) []string {
	out := []string{"asset.upload"}
	if s.Album != "" {
		out = append(out, "album.read", "album.create", "albumAsset.create")
	}
	if s.Stack {
		out = append(out, "stack.create")
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02")
}

func (p *Plugin) Status(ctx context.Context) plugin.Status {
	st := plugin.Status{Title: "Immich", Headline: "Not set up"}
	pl := p.pipe.Load()
	if pl == nil {
		st.Headline = "Starting"
		return st
	}
	s := pl.cfg()
	configured := s.URL != "" && s.APIKey != ""
	keyProblem := pl.keyProblem.Load().(string)
	switch {
	case !configured:
		st.Level = "wait"
		st.Notice, st.NoticeLink = "Immich is not set up yet. Synced or pulled files wait and are uploaded once it is.", "/settings"
	case pl.immichOK.Load() && keyProblem == "":
		st.Level, st.Headline = "ok", "Connected"
	case pl.immichOK.Load() || pl.immichErr.Load().(string) != "":
		st.Level, st.Headline = "bad", "Not reachable"
	default:
		st.Level, st.Headline = "wait", "Connecting"
	}
	if configured && keyProblem != "" {
		st.Notice, st.NoticeLink = keyProblem, "/settings"
	}
	if configured {
		st.Rows = []plugin.Row{
			{Label: "Version", Value: pl.immichVersion.Load().(string)},
			{Label: "Address", Value: s.URL, Link: s.URL},
		}
	}

	pl.mu.Lock()
	lastSync, lastResult := pl.lastSync, pl.lastResult
	pl.mu.Unlock()
	if configured {
		auto := "off"
		if s.Auto {
			auto = "every " + s.AutoEvery.String() + " and on every camera connect"
		}
		st.Rows = append(st.Rows, plugin.Row{Label: "Automatic sync", Value: auto})
	}
	if pl.syncing.Load() {
		st.Rows = append(st.Rows, plugin.Row{Label: "Sync running", Value: "yes"})
	}
	if !lastSync.IsZero() {
		st.Rows = append(st.Rows, plugin.Row{Label: "Last camera sync", Value: ago(lastSync) + ": " + lastResult})
	}

	ctx, cancel := context.WithTimeout(ctx, uiTimeout)
	defer cancel()
	if c, err := pl.store.Counts(ctx); err == nil {
		for _, x := range []struct {
			label string
			st    store.Status
			bad   bool
		}{
			{"On camera", store.StatusPending, false}, {"Downloading", store.StatusDownloading, false},
			{"To upload", store.StatusDownloaded, false}, {"Uploading", store.StatusUploading, false},
			{"Finishing", store.StatusUploaded, false}, {"Done", store.StatusDone, false},
			{"Rejected", store.StatusRejected, true},
		} {
			st.Counts = append(st.Counts, plugin.Count{Label: x.label, N: int64(c[x.st]), Bad: x.bad})
		}
	}
	m := &pl.Metrics
	st.Totals = []plugin.Row{
		{Label: "Downloaded", Value: fmt.Sprintf("%d · %s", m.Downloads.Load(), humanBytes(m.DownloadBytes.Load()))},
		{Label: "Uploaded", Value: fmt.Sprintf("%d · %s", m.Uploads.Load(), humanBytes(m.UploadBytes.Load()))},
	}
	if n := m.UploadDuplicates.Load(); n > 0 {
		st.Totals = append(st.Totals, plugin.Row{Label: "Already in Immich", Value: fmt.Sprint(n)})
	}
	st.Totals = append(st.Totals,
		plugin.Row{Label: "Failed tries", Value: fmt.Sprint(m.DownloadFailures.Load() + m.UploadFailures.Load())},
		plugin.Row{Label: "Last download", Value: ago(m.LastDownload())},
		plugin.Row{Label: "Last upload", Value: ago(m.LastUpload())})
	st.Transfer = pl.CurrentTransfer()
	if fs, err := pl.store.WithErrors(ctx, 10); err == nil {
		st.Problems = fileRows(s.URL, fs)
	}
	if fs, err := pl.store.Recent(ctx, 10); err == nil {
		st.Recent = fileRows(s.URL, fs)
	}
	return st
}

func fileRows(immichURL string, fs []store.File) []plugin.FileRow {
	out := make([]plugin.FileRow, 0, len(fs))
	for _, f := range fs {
		r := plugin.FileRow{
			Path: f.Path, Slot: f.Slot, Captured: f.CapturedAt, Status: string(f.Status), Size: f.Size,
			Attempts: fmt.Sprintf("%d/%d", f.DownloadAttempts, f.UploadAttempts), Updated: f.UpdatedAt, Note: f.LastError,
		}
		if f.Duplicate {
			r.Note = strings.TrimSpace("already in Immich " + f.LastError)
		}
		if f.AssetID != "" && immichURL != "" {
			r.Link = strings.TrimRight(immichURL, "/") + "/photos/" + f.AssetID
		}
		out = append(out, r)
	}
	return out
}

func (*Plugin) FileStatuses() []string {
	out := make([]string, len(store.AllStatuses))
	for i, s := range store.AllStatuses {
		out[i] = string(s)
	}
	return out
}

func (p *Plugin) Files(ctx context.Context, status string, limit int) []plugin.FileRow {
	pl := p.pipe.Load()
	if pl == nil {
		return nil
	}
	fs, err := pl.store.List(ctx, store.Status(status), limit)
	if err != nil {
		return nil
	}
	return fileRows(pl.cfg().URL, fs)
}

func (*Plugin) Actions() []plugin.Action {
	return []plugin.Action{
		{ID: "sync", Label: "Sync new files from camera", Primary: true},
		{ID: "retry", Label: "Retry waiting files now"},
	}
}

func (p *Plugin) Do(ctx context.Context, id string) (string, error) {
	pl := p.pipe.Load()
	if pl == nil {
		return "", fmt.Errorf("not running yet")
	}
	switch id {
	case "retry":
		if _, err := pl.store.RetryNow(ctx); err != nil {
			return "", err
		}
		pl.WakeAll()
		return "Waiting files will be retried now.", nil
	case "sync":
		if !pl.StartSync() {
			return "A sync is already running.", nil
		}
		return "Sync started.", nil
	}
	return "", fmt.Errorf("unknown action %q", id)
}

func (p *Plugin) counts() map[store.Status]int {
	pl := p.pipe.Load()
	if pl == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), uiTimeout)
	defer cancel()
	c, _ := pl.store.Counts(ctx)
	return c
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (p *Plugin) WriteMetrics(w io.Writer) {
	pl := p.pipe.Load()
	if pl == nil {
		return
	}
	m := &pl.Metrics
	for _, c := range []struct {
		name, help string
		v          int64
	}{
		{"downloads_total", "Files downloaded from the camera.", m.Downloads.Load()},
		{"download_bytes_total", "Bytes downloaded from the camera.", m.DownloadBytes.Load()},
		{"download_failures_total", "Failed download attempts.", m.DownloadFailures.Load()},
		{"uploads_total", "Files uploaded to Immich.", m.Uploads.Load()},
		{"upload_bytes_total", "Bytes uploaded to Immich.", m.UploadBytes.Load()},
		{"upload_duplicates_total", "Uploads Immich already had.", m.UploadDuplicates.Load()},
		{"upload_failures_total", "Failed upload attempts.", m.UploadFailures.Load()},
		{"camera_deletes_total", "Contents deleted from the camera.", m.CameraDeletes.Load()},
	} {
		fmt.Fprintf(w, "# HELP alpha2web_immich_%s %s\n# TYPE alpha2web_immich_%s counter\nalpha2web_immich_%s %d\n", c.name, c.help, c.name, c.name, c.v)
	}
	fmt.Fprint(w, "# HELP alpha2web_immich_files Files per status.\n# TYPE alpha2web_immich_files gauge\n")
	c := p.counts()
	for _, s := range store.AllStatuses {
		fmt.Fprintf(w, "alpha2web_immich_files{status=%q} %d\n", s, c[s])
	}
	fmt.Fprintf(w, "# HELP alpha2web_immich_reachable 1 when Immich answers the health check.\n# TYPE alpha2web_immich_reachable gauge\nalpha2web_immich_reachable %d\n", b2i(pl.immichOK.Load()))
	for _, g := range []struct {
		name string
		t    time.Time
	}{{"last_download_timestamp_seconds", m.LastDownload()}, {"last_upload_timestamp_seconds", m.LastUpload()}} {
		var v int64
		if !g.t.IsZero() {
			v = g.t.Unix()
		}
		fmt.Fprintf(w, "# TYPE alpha2web_immich_%s gauge\nalpha2web_immich_%s %d\n", g.name, g.name, v)
	}
}

func (p *Plugin) Health() map[string]any {
	pl := p.pipe.Load()
	if pl == nil {
		return map[string]any{"immich_reachable": false, "immich_version": "", "queue": map[string]int{}}
	}
	q := map[string]int{}
	for s, n := range p.counts() {
		q[string(s)] = n
	}
	return map[string]any{"immich_reachable": pl.immichOK.Load(), "immich_version": pl.immichVersion.Load().(string), "queue": q}
}
