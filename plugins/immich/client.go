// Package immich is a minimal client for the Immich v3 REST API.
//
// Endpoints and fields were checked against the official OpenAPI spec of
// Immich v3.3.1 (open-api/immich-openapi-specs.json):
//
//	GET  /api/server/version          (no auth)
//	POST /api/assets/bulk-upload-check (asset.upload)
//	POST /api/assets                   (asset.upload) multipart: assetData, fileCreatedAt, fileModifiedAt, filename
//	GET  /api/albums?name=             (album.read)
//	POST /api/albums                   (album.create)
//	PUT  /api/albums/{id}/assets       (albumAsset.create)
//	POST /api/stacks                   (stack.create)
//
// Immich v3 removed deviceAssetId/deviceId from uploads; they are not sent.
package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// MinMajorVersion is the oldest supported Immich major version.
const MinMajorVersion = 3

// Client talks to one Immich server.
type Client struct {
	base   string // e.g. http://immich:2283/api
	apiKey string
	http   *http.Client
	ua     string
}

// New creates a client. baseURL may be given with or without the /api suffix.
func New(baseURL, apiKey, userAgent string) *Client {
	b := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(b, "/api") {
		b += "/api"
	}
	return &Client{
		base:   b,
		apiKey: apiKey,
		ua:     userAgent,
		// No overall timeout: large video uploads can take long. Callers use contexts.
		http: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 10 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			MaxIdleConnsPerHost:   4,
		}},
	}
}

// APIError is a non-2xx response.
type APIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("immich %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// IsAuthError reports whether err is a 401/403 (bad API key or missing permission).
func IsAuthError(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden)
}

// IsClientError reports whether err is a 4xx other than auth/rate-limit,
// i.e. retrying the same request is unlikely to help.
func IsClientError(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 &&
		ae.Status != http.StatusUnauthorized && ae.Status != http.StatusForbidden &&
		ae.Status != http.StatusTooManyRequests && ae.Status != http.StatusRequestTimeout
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, header http.Header, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return resp.StatusCode, &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("immich %s %s: decode response: %w", method, path, err)
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	return c.do(ctx, method, path, body, ct, nil, out)
}

// Version is the server version.
type Version struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
	Patch int `json:"patch"`
}

func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

// ServerVersion fetches the Immich version.
func (c *Client) ServerVersion(ctx context.Context) (Version, error) {
	var v Version
	_, err := c.doJSON(ctx, http.MethodGet, "/server/version", nil, &v)
	return v, err
}

// CheckCompatible verifies the server is reachable and new enough.
func (c *Client) CheckCompatible(ctx context.Context) (Version, error) {
	v, err := c.ServerVersion(ctx)
	if err != nil {
		return v, err
	}
	if v.Major < MinMajorVersion {
		return v, fmt.Errorf("immich %s is not supported; alpha2web requires Immich v%d.x or newer", v, MinMajorVersion)
	}
	return v, nil
}

// CheckItem is one entry for BulkUploadCheck.
type CheckItem struct {
	ID       string `json:"id"`
	Checksum string `json:"checksum"` // hex or base64 SHA-1
}

// CheckResult is the server's verdict for one CheckItem.
type CheckResult struct {
	ID        string `json:"id"`
	Action    string `json:"action"`            // "accept" | "reject"
	Reason    string `json:"reason,omitempty"`  // "duplicate" | "unsupported-format"
	AssetID   string `json:"assetId,omitempty"` // existing asset when duplicate
	IsTrashed bool   `json:"isTrashed,omitempty"`
}

// BulkUploadCheck asks which checksums are already in the library.
func (c *Client) BulkUploadCheck(ctx context.Context, items []CheckItem) ([]CheckResult, error) {
	var out struct {
		Results []CheckResult `json:"results"`
	}
	_, err := c.doJSON(ctx, http.MethodPost, "/assets/bulk-upload-check", map[string]any{"assets": items}, &out)
	return out.Results, err
}

// UploadRequest describes an asset upload.
type UploadRequest struct {
	Path       string // local file
	Filename   string // original camera filename
	SHA1       string // hex; sent as x-immich-checksum so the server can short-circuit duplicates
	CreatedAt  time.Time
	ModifiedAt time.Time
}

// UploadResult is the created (or already existing) asset.
type UploadResult struct {
	ID        string
	Duplicate bool
}

// Upload streams a file to POST /api/assets.
func (c *Client) Upload(ctx context.Context, r UploadRequest) (UploadResult, error) {
	f, err := os.Open(r.Path)
	if err != nil {
		return UploadResult{}, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			fields := [][2]string{
				{"fileCreatedAt", r.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")},
				{"fileModifiedAt", r.ModifiedAt.UTC().Format("2006-01-02T15:04:05.000Z")},
				{"filename", r.Filename},
			}
			for _, kv := range fields {
				if err := mw.WriteField(kv[0], kv[1]); err != nil {
					return err
				}
			}
			part, err := mw.CreateFormFile("assetData", r.Filename)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	h := http.Header{}
	if r.SHA1 != "" {
		h.Set("x-immich-checksum", r.SHA1)
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"` // "created" | "duplicate"
	}
	code, err := c.do(ctx, http.MethodPost, "/assets", pr, mw.FormDataContentType(), h, &out)
	pr.CloseWithError(io.ErrClosedPipe) // unblock the writer goroutine on early failure
	if err != nil {
		return UploadResult{}, err
	}
	if out.ID == "" {
		return UploadResult{}, fmt.Errorf("immich POST /assets: HTTP %d without asset id", code)
	}
	return UploadResult{ID: out.ID, Duplicate: out.Status == "duplicate" || code == http.StatusOK}, nil
}

// FindOrCreateAlbum returns the id of the album with exactly this name owned
// by (or shared with) the API key's user, creating it if needed.
func (c *Client) FindOrCreateAlbum(ctx context.Context, name string) (string, error) {
	var albums []struct {
		ID        string `json:"id"`
		AlbumName string `json:"albumName"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, "/albums?name="+url.QueryEscape(name), nil, &albums); err != nil {
		return "", err
	}
	for _, a := range albums {
		if a.AlbumName == name {
			return a.ID, nil
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/albums", map[string]any{"albumName": name}, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", errors.New("immich POST /albums: response without id")
	}
	return created.ID, nil
}

// BulkIDResult is the per-asset result of an album operation.
type BulkIDResult struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"` // e.g. "duplicate" when already in the album
}

// AddToAlbum adds assets to an album. Assets already in the album are reported
// with error "duplicate", which callers treat as success.
func (c *Client) AddToAlbum(ctx context.Context, albumID string, assetIDs []string) ([]BulkIDResult, error) {
	var out []BulkIDResult
	_, err := c.doJSON(ctx, http.MethodPut, "/albums/"+url.PathEscape(albumID)+"/assets", map[string]any{"ids": assetIDs}, &out)
	return out, err
}

// CreateStack stacks assets; the first id becomes the primary asset.
// Existing stacks whose primary is among the ids are merged by the server.
func (c *Client) CreateStack(ctx context.Context, assetIDs []string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	_, err := c.doJSON(ctx, http.MethodPost, "/stacks", map[string]any{"assetIds": assetIDs}, &out)
	return out.ID, err
}

// BaseURL returns the server URL as configured (with the /api suffix).
func (c *Client) BaseURL() string { return c.base }

// RequiredPermissions are the API key permissions alpha2web needs.
var RequiredPermissions = []string{"asset.upload", "album.read", "album.create", "albumAsset.create", "stack.create"}

// ValidateKey checks that the API key is accepted and may upload. It checks
// one checksum that cannot exist (all zeros): an empty list makes Immich
// v3.2.1 build invalid SQL ("checksum" in ()) and answer HTTP 500.
func (c *Client) ValidateKey(ctx context.Context) error {
	_, err := c.BulkUploadCheck(ctx, []CheckItem{{ID: "alpha2web-key-check", Checksum: strings.Repeat("0", 40)}})
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
		return nil // authenticated; the server only disliked the empty list
	}
	return err
}
