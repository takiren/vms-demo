// Package mediamtx keeps MediaMTX's path configuration in sync with the
// cameras in the database, through the MediaMTX Control API (v3).
//
// Path configs carry the camera's RTSP URL with the credentials in it, so
// nothing in this package logs a source or a raw response body. Errors from
// MediaMTX are passed through Redact before they are returned.
package mediamtx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// PathConf is the subset of MediaMTX's PathConf that the API manages. Every
// other field is left at the pathDefaults of mediamtx.yml (paths are written
// with replace, which resets unspecified fields to the defaults).
type PathConf struct {
	Name           string `json:"name"`
	Source         string `json:"source"`
	SourceOnDemand bool   `json:"sourceOnDemand"`
	Record         bool   `json:"record"`
}

// String hides the source, which contains the camera's credentials.
func (p PathConf) String() string {
	return fmt.Sprintf("{name:%s source:[REDACTED] sourceOnDemand:%t record:%t}", p.Name, p.SourceOnDemand, p.Record)
}

// GoString hides the source from %#v.
func (p PathConf) GoString() string { return p.String() }

// LogValue hides the source from slog.
func (p PathConf) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", p.Name),
		slog.Bool("source_on_demand", p.SourceOnDemand),
		slog.Bool("record", p.Record),
	)
}

// ErrNotFound is returned when MediaMTX answers 404 (no such path).
var ErrNotFound = errors.New("mediamtx: path not found")

// Client calls the MediaMTX Control API. It authenticates with HTTP Basic;
// MediaMTX forwards the credentials to the API's auth endpoint (action
// "api"), which allows exactly these credentials.
type Client struct {
	base     string
	user     string
	password string
	http     *http.Client
}

// requestTimeout bounds every Control API call.
const requestTimeout = 10 * time.Second

// maxResponseBytes caps how much of a response is read.
const maxResponseBytes = 8 << 20

// itemsPerPage is the page size used to list path configs.
const itemsPerPage = 100

// NewClient returns a Client for the Control API at baseURL
// (e.g. http://127.0.0.1:9997).
func NewClient(baseURL, user, password string) *Client {
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		user:     user,
		password: password,
		http: &http.Client{
			Timeout: requestTimeout,
			// Never follow redirects: the Control API has none, and a redirect
			// must not carry the credentials (or the path config) elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// ListPaths returns every path config, following pagination.
func (c *Client) ListPaths(ctx context.Context) ([]PathConf, error) {
	var out []PathConf
	for page := 0; ; page++ {
		var res struct {
			PageCount int64      `json:"pageCount"`
			Items     []PathConf `json:"items"`
		}
		q := url.Values{"page": {fmt.Sprint(page)}, "itemsPerPage": {fmt.Sprint(itemsPerPage)}}
		if err := c.do(ctx, http.MethodGet, "/v3/config/paths/list?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if int64(page+1) >= res.PageCount || len(res.Items) == 0 {
			return out, nil
		}
		if page > 1000 {
			return nil, errors.New("mediamtx: too many pages of path configs")
		}
	}
}

// AddPath adds a path config. It fails if the path exists.
func (c *Client) AddPath(ctx context.Context, p PathConf) error {
	return c.do(ctx, http.MethodPost, "/v3/config/paths/add/"+url.PathEscape(p.Name), p, nil)
}

// ReplacePath replaces all values of an existing path config.
func (c *Client) ReplacePath(ctx context.Context, p PathConf) error {
	return c.do(ctx, http.MethodPost, "/v3/config/paths/replace/"+url.PathEscape(p.Name), p, nil)
}

// DeletePath removes a path config.
func (c *Client) DeletePath(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v3/config/paths/delete/"+url.PathEscape(name), nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mediamtx: encode request: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return fmt.Errorf("mediamtx: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(c.user, c.password)
	// The operation is logged by name only: the URL is safe, bodies are not.
	op := method + " " + strings.SplitN(path, "?", 2)[0]
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mediamtx: %s: %s", op, Redact(err.Error()))
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("mediamtx: %s: read response: %s", op, Redact(err.Error()))
	}
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, op)
	}
	if res.StatusCode != http.StatusOK {
		return &StatusError{Op: op, Status: res.StatusCode, Message: errorMessage(data)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		// The JSON error may quote part of the body (sources): do not include it.
		return fmt.Errorf("mediamtx: %s: invalid JSON response", op)
	}
	return nil
}

// StatusError is a non-2xx answer from the Control API.
type StatusError struct {
	Op      string
	Status  int
	Message string // already redacted
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("mediamtx: %s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("mediamtx: %s: HTTP %d: %s", e.Op, e.Status, e.Message)
}

// errorMessage extracts {"error": "..."} from a response, redacted and
// shortened.
func errorMessage(data []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return ""
	}
	msg := Redact(e.Error)
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}

// userinfoPattern matches the userinfo of URLs embedded in text.
var userinfoPattern = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/?#\s"'@]*@`)

// Redact removes credentials (userinfo) from any URLs in s.
func Redact(s string) string {
	return userinfoPattern.ReplaceAllString(s, "${1}[REDACTED]@")
}
