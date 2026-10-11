// Package store persists the per-file transfer state machine in SQLite.
//
// Every file seen on the camera gets one row. Rows move through:
//
//	pending -> downloading -> downloaded -> uploading -> uploaded -> done
//	                                                 \-> rejected (terminal)
//
// Failures never leave a row in an in-flight state: a failed download goes
// back to pending, a failed upload back to downloaded, each with an attempt
// counter, the last error and a next_attempt_at for backoff. After a crash,
// RecoverInterrupted resets rows that were in flight.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Status is the lifecycle state of a camera file.
type Status string

const (
	StatusPending     Status = "pending"     // seen on camera, not downloaded yet
	StatusDownloading Status = "downloading" // transfer from camera in progress
	StatusDownloaded  Status = "downloaded"  // verified and checksummed in staging
	StatusUploading   Status = "uploading"   // upload to Immich in progress
	StatusUploaded    Status = "uploaded"    // in Immich; album/stack/cleanup pending
	StatusDone        Status = "done"        // nothing left to do
	StatusRejected    Status = "rejected"    // Immich refused the file (terminal)
)

// AllStatuses lists every status, in lifecycle order.
var AllStatuses = []Status{StatusPending, StatusDownloading, StatusDownloaded, StatusUploading, StatusUploaded, StatusDone, StatusRejected}

// ErrInvalidTransition is returned when a row is not in the expected state.
var ErrInvalidTransition = errors.New("invalid state transition")

// File is one row of the files table.
type File struct {
	ID               int64
	Camera           string
	Slot             int
	Path             string // path on the camera card, e.g. DCIM/100MSDCF/DSC01234.JPG
	CapturedAt       time.Time
	GroupKey         string // files of one shot (RAW+JPEG) share a key
	Status           Status
	Size             int64
	SHA1             string
	LocalPath        string
	AssetID          string
	Duplicate        bool
	DownloadAttempts int
	UploadAttempts   int
	PostAttempts     int
	LastError        string
	NextAttemptAt    time.Time
	AlbumAdded       bool
	Stacked          bool
	LocalDeleted     bool
	CameraDeleted    bool
	SourcePath       string // library copy this file was imported from; cleared once removed
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Discovered describes a file listed on the camera.
type Discovered struct {
	Camera     string
	Slot       int
	Path       string
	CapturedAt time.Time
	Size       int64
	GroupKey   string
}

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

// SetClock overrides the time source (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is usable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

var migrations = []string{
	`CREATE TABLE files (
		id                INTEGER PRIMARY KEY,
		camera            TEXT    NOT NULL,
		slot              INTEGER NOT NULL,
		path              TEXT    NOT NULL,
		captured_at       INTEGER NOT NULL,
		group_key         TEXT    NOT NULL,
		status            TEXT    NOT NULL,
		size              INTEGER NOT NULL DEFAULT 0,
		sha1              TEXT    NOT NULL DEFAULT '',
		local_path        TEXT    NOT NULL DEFAULT '',
		asset_id          TEXT    NOT NULL DEFAULT '',
		duplicate         INTEGER NOT NULL DEFAULT 0,
		download_attempts INTEGER NOT NULL DEFAULT 0,
		upload_attempts   INTEGER NOT NULL DEFAULT 0,
		post_attempts     INTEGER NOT NULL DEFAULT 0,
		last_error        TEXT    NOT NULL DEFAULT '',
		next_attempt_at   INTEGER NOT NULL DEFAULT 0,
		album_added       INTEGER NOT NULL DEFAULT 0,
		stacked           INTEGER NOT NULL DEFAULT 0,
		local_deleted     INTEGER NOT NULL DEFAULT 0,
		camera_deleted    INTEGER NOT NULL DEFAULT 0,
		created_at        INTEGER NOT NULL,
		updated_at        INTEGER NOT NULL,
		UNIQUE (camera, slot, path, captured_at, size)
	);
	CREATE INDEX files_status_due ON files (status, next_attempt_at);
	CREATE INDEX files_group ON files (group_key);`,
	`CREATE INDEX files_sha1 ON files (sha1);`,
	`ALTER TABLE files ADD COLUMN source_path TEXT NOT NULL DEFAULT '';`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

const fileColumns = `id, camera, slot, path, captured_at, group_key, status, size, sha1, local_path,
	asset_id, duplicate, download_attempts, upload_attempts, post_attempts, last_error,
	next_attempt_at, album_added, stacked, local_deleted, camera_deleted, created_at, updated_at, source_path`

func scanFile(sc interface{ Scan(...any) error }) (File, error) {
	var f File
	var captured, next, created, updated int64
	var status string
	err := sc.Scan(&f.ID, &f.Camera, &f.Slot, &f.Path, &captured, &f.GroupKey, &status, &f.Size, &f.SHA1,
		&f.LocalPath, &f.AssetID, &f.Duplicate, &f.DownloadAttempts, &f.UploadAttempts, &f.PostAttempts,
		&f.LastError, &next, &f.AlbumAdded, &f.Stacked, &f.LocalDeleted, &f.CameraDeleted, &created, &updated, &f.SourcePath)
	f.Status = Status(status)
	f.CapturedAt, f.NextAttemptAt, f.CreatedAt, f.UpdatedAt = fromMS(captured), fromMS(next), fromMS(created), fromMS(updated)
	return f, err
}

func (s *Store) query(ctx context.Context, where string, args ...any) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+fileColumns+` FROM files `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Get returns one file by id.
func (s *Store) Get(ctx context.Context, id int64) (File, error) {
	files, err := s.query(ctx, `WHERE id = ?`, id)
	if err != nil {
		return File{}, err
	}
	if len(files) == 0 {
		return File{}, sql.ErrNoRows
	}
	return files[0], nil
}

// RecoverInterrupted resets rows left in flight by a crash or hard stop.
func (s *Store) RecoverInterrupted(ctx context.Context) (int64, error) {
	now := ms(s.now())
	var total int64
	for _, q := range []string{
		`UPDATE files SET status = 'pending', updated_at = ? WHERE status = 'downloading'`,
		`UPDATE files SET status = 'downloaded', updated_at = ? WHERE status = 'uploading'`,
	} {
		res, err := s.db.ExecContext(ctx, q, now)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// UpsertDiscovered records files seen on the camera. Known files are left
// untouched. It returns the number of newly inserted rows.
func (s *Store) UpsertDiscovered(ctx context.Context, files []Discovered) (int, error) {
	if len(files) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO files (camera, slot, path, captured_at, size, group_key, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?) ON CONFLICT (camera, slot, path, captured_at, size) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	now := ms(s.now())
	inserted := 0
	for _, f := range files {
		res, err := stmt.ExecContext(ctx, f.Camera, f.Slot, f.Path, ms(f.CapturedAt), f.Size, f.GroupKey, now, now)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	return inserted, tx.Commit()
}

// Lookup returns the row for a file seen on the camera.
func (s *Store) Lookup(ctx context.Context, d Discovered) (File, bool, error) {
	files, err := s.query(ctx, `WHERE camera = ? AND slot = ? AND path = ? AND captured_at = ? AND size = ?`,
		d.Camera, d.Slot, d.Path, ms(d.CapturedAt), d.Size)
	if err != nil || len(files) == 0 {
		return File{}, false, err
	}
	return files[0], true, nil
}

// DoneNotDeleted returns finished files of a camera that are still on its card.
func (s *Store) DoneNotDeleted(ctx context.Context, camera string) ([]File, error) {
	return s.query(ctx, `WHERE status = 'done' AND camera_deleted = 0 AND camera = ?`, camera)
}

// DuePending returns pending files of a camera whose backoff has expired, oldest capture first.
func (s *Store) DuePending(ctx context.Context, camera string, limit int) ([]File, error) {
	return s.query(ctx, `WHERE status = 'pending' AND camera = ? AND next_attempt_at <= ? ORDER BY captured_at, path LIMIT ?`,
		camera, ms(s.now()), limit)
}

// DueDownloaded returns verified files ready for upload.
func (s *Store) DueDownloaded(ctx context.Context, limit int) ([]File, error) {
	return s.query(ctx, `WHERE status = 'downloaded' AND next_attempt_at <= ? ORDER BY captured_at, path LIMIT ?`, ms(s.now()), limit)
}

// DueUploaded returns uploaded files that still need post-processing.
func (s *Store) DueUploaded(ctx context.Context, limit int) ([]File, error) {
	return s.query(ctx, `WHERE status = 'uploaded' AND next_attempt_at <= ? ORDER BY captured_at, path LIMIT ?`, ms(s.now()), limit)
}

// GroupMembers returns all files sharing a group key.
func (s *Store) GroupMembers(ctx context.Context, groupKey string) ([]File, error) {
	return s.query(ctx, `WHERE group_key = ? ORDER BY path`, groupKey)
}

// ResetBackoff makes all pending files of a camera due immediately (on reconnect).
func (s *Store) ResetBackoff(ctx context.Context, camera string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET next_attempt_at = 0 WHERE status = 'pending' AND camera = ?`, camera)
	return err
}

// transition moves a row from one of the allowed states to `to`, applying extra SET clauses.
func (s *Store) transition(ctx context.Context, id int64, from []Status, to Status, set string, args ...any) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	q := `UPDATE files SET status = ?, updated_at = ?`
	if set != "" {
		q += ", " + set
	}
	q += ` WHERE id = ? AND status IN (` + placeholders + `)`
	all := []any{string(to), ms(s.now())}
	all = append(all, args...)
	all = append(all, id)
	for _, f := range from {
		all = append(all, string(f))
	}
	res, err := s.db.ExecContext(ctx, q, all...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: file %d -> %s", ErrInvalidTransition, id, to)
	}
	return nil
}

// StartDownload claims a pending file for download.
func (s *Store) StartDownload(ctx context.Context, id int64) error {
	return s.transition(ctx, id, []Status{StatusPending}, StatusDownloading, "")
}

// FinishDownload records a verified download in staging.
func (s *Store) FinishDownload(ctx context.Context, id int64, localPath string, size int64, sha1 string) error {
	return s.transition(ctx, id, []Status{StatusDownloading}, StatusDownloaded,
		`local_path = ?, size = ?, sha1 = ?, last_error = '', next_attempt_at = 0, local_deleted = 0`, localPath, size, sha1)
}

// FailDownload returns a file to pending. If countAttempt is false (e.g. the
// camera simply went away) the attempt counter is not increased.
func (s *Store) FailDownload(ctx context.Context, id int64, cause string, next time.Time, countAttempt bool) error {
	inc := 0
	if countAttempt {
		inc = 1
	}
	return s.transition(ctx, id, []Status{StatusDownloading}, StatusPending,
		`download_attempts = download_attempts + ?, last_error = ?, next_attempt_at = ?`, inc, cause, ms(next))
}

// StartUpload claims a downloaded file for upload. It fails with
// ErrInvalidTransition when another worker already claimed it.
func (s *Store) StartUpload(ctx context.Context, id int64) error {
	return s.transition(ctx, id, []Status{StatusDownloaded}, StatusUploading, "")
}

// FinishUpload records the Immich asset id.
func (s *Store) FinishUpload(ctx context.Context, id int64, assetID string, duplicate bool) error {
	return s.transition(ctx, id, []Status{StatusUploading}, StatusUploaded,
		`asset_id = ?, duplicate = ?, last_error = '', next_attempt_at = 0`, assetID, duplicate)
}

// UploadedSHA1 returns the asset id and camera path of another file with the
// same content that is already in Immich, or "" if there is none.
func (s *Store) UploadedSHA1(ctx context.Context, sha1 string, exclude int64) (assetID, path string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT asset_id, path FROM files WHERE sha1 = ? AND asset_id != '' AND id != ? LIMIT 1`,
		sha1, exclude).Scan(&assetID, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return assetID, path, err
}

// FailUpload returns a file to downloaded for a later retry.
func (s *Store) FailUpload(ctx context.Context, id int64, cause string, next time.Time) error {
	return s.transition(ctx, id, []Status{StatusUploading}, StatusDownloaded,
		`upload_attempts = upload_attempts + 1, last_error = ?, next_attempt_at = ?`, cause, ms(next))
}

// Redownload sends a file whose staged copy vanished back to pending.
func (s *Store) Redownload(ctx context.Context, id int64, cause string) error {
	return s.transition(ctx, id, []Status{StatusUploading, StatusDownloaded}, StatusPending,
		`last_error = ?, next_attempt_at = 0, local_path = '', sha1 = ''`, cause)
}

// Reject marks a file Immich refused to accept (terminal).
func (s *Store) Reject(ctx context.Context, id int64, cause string) error {
	return s.transition(ctx, id, []Status{StatusUploading}, StatusRejected, `last_error = ?`, cause)
}

func (s *Store) setFlag(ctx context.Context, column string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := []any{ms(s.now())}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE files SET `+column+` = 1, updated_at = ? WHERE id IN (`+placeholders+`)`, args...)
	return err
}

// MarkAlbumAdded flags files as added to the configured album.
func (s *Store) MarkAlbumAdded(ctx context.Context, ids ...int64) error {
	return s.setFlag(ctx, "album_added", ids)
}

// MarkStacked flags files whose stack has been handled.
func (s *Store) MarkStacked(ctx context.Context, ids ...int64) error {
	return s.setFlag(ctx, "stacked", ids)
}

// MarkLocalDeleted flags files whose staged copy was removed.
func (s *Store) MarkLocalDeleted(ctx context.Context, ids ...int64) error {
	return s.setFlag(ctx, "local_deleted", ids)
}

// SetSourcePath records (or, with "", clears) the library copy of an imported file.
func (s *Store) SetSourcePath(ctx context.Context, id int64, path string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET source_path = ? WHERE id = ?`, path, id)
	return err
}

// MarkCameraDeleted flags files whose content was deleted on the camera.
func (s *Store) MarkCameraDeleted(ctx context.Context, ids ...int64) error {
	return s.setFlag(ctx, "camera_deleted", ids)
}

// Done finishes post-processing for an uploaded file.
func (s *Store) Done(ctx context.Context, id int64) error {
	return s.transition(ctx, id, []Status{StatusUploaded}, StatusDone, `last_error = '', next_attempt_at = 0`)
}

// DeferPost postpones post-processing; countAttempt increases post_attempts.
func (s *Store) DeferPost(ctx context.Context, id int64, cause string, next time.Time, countAttempt bool) error {
	inc := 0
	if countAttempt {
		inc = 1
	}
	return s.transition(ctx, id, []Status{StatusUploaded}, StatusUploaded,
		`post_attempts = post_attempts + ?, last_error = ?, next_attempt_at = ?`, inc, cause, ms(next))
}

// Counts returns the number of rows per status.
func (s *Store) Counts(ctx context.Context) (map[Status]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM files GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[Status]int, len(AllStatuses))
	for _, st := range AllStatuses {
		out[st] = 0
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[Status(st)] = n
	}
	return out, rows.Err()
}

// Recent returns the most recently changed files.
func (s *Store) Recent(ctx context.Context, limit int) ([]File, error) {
	return s.query(ctx, `ORDER BY updated_at DESC, id DESC LIMIT ?`, limit)
}

// List returns the most recently changed files with this status ("" = any).
func (s *Store) List(ctx context.Context, status Status, limit int) ([]File, error) {
	if status == "" {
		return s.Recent(ctx, limit)
	}
	return s.query(ctx, `WHERE status = ? ORDER BY updated_at DESC, id DESC LIMIT ?`, string(status), limit)
}

// WithErrors returns unfinished files whose last attempt failed, plus rejected files.
func (s *Store) WithErrors(ctx context.Context, limit int) ([]File, error) {
	return s.query(ctx, `WHERE last_error != '' AND status != 'done' ORDER BY updated_at DESC LIMIT ?`, limit)
}

// RetryNow clears the backoff of every waiting file so it is retried immediately.
func (s *Store) RetryNow(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE files SET next_attempt_at = 0 WHERE next_attempt_at > 0 AND status IN ('pending','downloaded','uploaded')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
