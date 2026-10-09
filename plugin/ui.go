package plugin

import (
	"context"
	"io"
	"time"
)

// FieldKind selects the input a setting gets on the settings page. Values are
// always stored as strings ("true"/"false" for FieldBool).
type FieldKind int

const (
	FieldText FieldKind = iota
	FieldSecret
	FieldBool
	FieldDuration
	FieldInt
)

// ConfigValidator is optional; Validate runs on save with the plugin's new
// settings and a non-nil error rejects the save.
type ConfigValidator interface {
	Validate(cfg map[string]string) error
}

// Row is one label/value line; Link, if set, makes the value a link.
type Row struct{ Label, Value, Link string }

// Count is a number in a "Queue" style card; Bad highlights it.
type Count struct {
	Label string
	N     int64
	Bad   bool
}

// Transfer is the progress of the file currently moving.
type Transfer struct {
	Name        string
	Percent     int
	Done, Size  int64
	BytesPerSec float64
	ETA         time.Duration
	Started     time.Time
}

// FileRow describes one file in the tables. Status is shown as a badge.
type FileRow struct {
	Name, Path string
	Slot       int
	Captured   time.Time
	Status     string
	Size       int64
	Attempts   string
	Updated    time.Time
	Note       string
	Link       string
}

// Status is what a plugin shows on the dashboard.
type Status struct {
	Title    string
	Level    string // "ok", "wait", "bad" or "" for the headline dot
	Headline string
	Rows     []Row
	Counts   []Count
	Totals   []Row
	Transfer *Transfer
	Problems []FileRow
	Recent   []FileRow
	// Notice is a banner such as "not set up yet".
	Notice, NoticeLink string
}

// StatusProvider is optional: the plugin gets a block on the dashboard.
type StatusProvider interface {
	Status(ctx context.Context) Status
}

// FileStateProvider is optional: the plugin gets a table on the files page.
type FileStateProvider interface {
	FileStatuses() []string
	// Files lists the most recently changed files; status "" means all.
	Files(ctx context.Context, status string, limit int) []FileRow
}

// Action is a button on the dashboard.
type Action struct {
	ID, Label string
	Primary   bool
}

// ActionProvider is optional. Do's message (or error) is shown as a banner.
type ActionProvider interface {
	Actions() []Action
	Do(ctx context.Context, id string) (message string, err error)
}

// MetricsProvider is optional: WriteMetrics appends Prometheus text lines.
type MetricsProvider interface {
	WriteMetrics(w io.Writer)
}

// HealthProvider is optional: the map appears under "plugins" in /health.
type HealthProvider interface {
	Health() map[string]any
}
