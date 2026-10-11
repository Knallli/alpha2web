package immich

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/knallli/alpha2web/internal/backoff"
)

// settings is the plugin configuration with defaults applied.
type settings struct {
	URL, APIKey, Album string
	Stack              bool
	DeleteLocal        bool // remove the staged copy once the asset is safe in Immich
	DeleteOnCamera     bool // remove a content from the card once all its files are done
	Skip               []string
	Concurrency        int
	Stall              time.Duration
	MinFreeBytes       int64
	Retry              backoff.Policy
}

// parseSettings applies defaults for missing keys. Invalid values also fall
// back to the default; the first one is reported so Validate can reject it.
func parseSettings(cfg map[string]string) (settings, error) {
	s := settings{
		URL: strings.TrimSpace(cfg["url"]), APIKey: strings.TrimSpace(cfg["api_key"]), Album: strings.TrimSpace(cfg["album"]),
		Stack: true, DeleteLocal: true, Concurrency: 2,
		Stall: 3 * time.Minute, MinFreeBytes: 1024 << 20,
		Retry: backoff.Policy{Base: 5 * time.Second, Max: 15 * time.Minute, Jitter: 0.2},
	}
	var first error
	bad := func(key string, err error) {
		if first == nil {
			first = fmt.Errorf("%s: %w", key, err)
		}
	}
	boolean := func(key string, dst *bool) {
		if v := strings.TrimSpace(cfg[key]); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				bad(key, err)
				return
			}
			*dst = b
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v := strings.TrimSpace(cfg[key]); v != "" {
			d, err := time.ParseDuration(v)
			if err == nil && d <= 0 {
				err = fmt.Errorf("must be positive")
			}
			if err != nil {
				bad(key, err)
				return
			}
			*dst = d
		}
	}
	num := func(key string, lo int64, dst *int64) {
		if v := strings.TrimSpace(cfg[key]); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil && n < lo {
				err = fmt.Errorf("must be at least %d", lo)
			}
			if err != nil {
				bad(key, err)
				return
			}
			*dst = n
		}
	}
	boolean("stack_raw_jpeg", &s.Stack)
	boolean("delete_local", &s.DeleteLocal)
	boolean("delete_on_camera", &s.DeleteOnCamera)
	dur("stall_timeout", &s.Stall)
	dur("retry_base_delay", &s.Retry.Base)
	dur("retry_max_delay", &s.Retry.Max)
	conc, free := int64(s.Concurrency), s.MinFreeBytes>>20
	num("upload_concurrency", 1, &conc)
	num("min_free_space_mb", 0, &free)
	s.Concurrency, s.MinFreeBytes = int(min(conc, 16)), free<<20
	s.Retry.Max = max(s.Retry.Max, s.Retry.Base)

	skip := ".XML,.THM"
	if v, ok := cfg["skip_extensions"]; ok && strings.TrimSpace(v) != "" {
		skip = v
	}
	if !strings.EqualFold(strings.TrimSpace(skip), "none") {
		for _, e := range strings.Split(skip, ",") {
			if e = strings.ToUpper(strings.TrimSpace(e)); e != "" {
				if !strings.HasPrefix(e, ".") {
					e = "." + e
				}
				s.Skip = append(s.Skip, e)
			}
		}
	}
	return s, first
}

func (s settings) skipped(camPath string) bool {
	ext := strings.ToUpper(path.Ext(camPath))
	for _, e := range s.Skip {
		if ext == e {
			return true
		}
	}
	return false
}
