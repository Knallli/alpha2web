package immich

import (
	"sync/atomic"
	"time"
)

// Metrics are process-lifetime counters.
type Metrics struct {
	Downloads        atomic.Int64
	DownloadFailures atomic.Int64
	DownloadBytes    atomic.Int64
	Uploads          atomic.Int64
	UploadDuplicates atomic.Int64
	UploadFailures   atomic.Int64
	UploadBytes      atomic.Int64
	Rejected         atomic.Int64
	Reconciles       atomic.Int64
	Stacks           atomic.Int64
	CameraDeletes    atomic.Int64

	lastDownload atomic.Int64 // unix seconds
	lastUpload   atomic.Int64
}

// LastDownload is the time of the last successful download (zero if none).
func (m *Metrics) LastDownload() time.Time { return unix(m.lastDownload.Load()) }

// LastUpload is the time of the last successful upload (zero if none).
func (m *Metrics) LastUpload() time.Time { return unix(m.lastUpload.Load()) }

func unix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}
