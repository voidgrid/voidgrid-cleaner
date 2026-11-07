// Package httpjson is the one JSON-over-HTTP call every API client uses.
package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxBody caps a response; a big Plex library listing is tens of MB.
const maxBody = 256 << 20

// StatusError is a non-2xx reply. Path never includes the query string, so
// a key passed as a query parameter can't end up in a log.
type StatusError struct {
	Service string
	Method  string
	Path    string
	Code    int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: %s %s: HTTP %d", e.Service, e.Method, e.Path, e.Code)
}

// Call is one request, for the audit log. Target is the path and query with
// anything that looks like a credential taken out.
type Call struct {
	Service, Method, Target string
	Status                  int // 0 if no reply came
}

func (c Call) String() string {
	return fmt.Sprintf("%s %s %s: %d", c.Service, c.Method, c.Target, c.Status)
}

type recorderKey struct{}

// WithRecorder makes every call on ctx report to fn.
func WithRecorder(ctx context.Context, fn func(Call)) context.Context {
	return context.WithValue(ctx, recorderKey{}, fn)
}

// target drops query values whose names suggest a secret. Keys travel in
// headers today; this holds if a client ever puts one in a URL.
func target(u *url.URL) string {
	q := u.Query()
	for name := range q {
		if n := strings.ToLower(name); strings.Contains(n, "key") || strings.Contains(n, "token") || strings.Contains(n, "secret") {
			q.Del(name)
		}
	}
	if len(q) == 0 {
		return u.Path
	}
	return u.Path + "?" + q.Encode()
}

// Do sends method to rawURL with hdr, JSON-encoding body if it isn't nil,
// and decodes a JSON reply into out if out isn't nil.
func Do(ctx context.Context, hc *http.Client, service, method, rawURL string, hdr http.Header, body, out any) error {
	status, err := do(ctx, hc, service, method, rawURL, hdr, body, out)
	if fn, ok := ctx.Value(recorderKey{}).(func(Call)); ok {
		if u, perr := url.Parse(rawURL); perr == nil {
			fn(Call{Service: service, Method: method, Target: target(u), Status: status})
		}
	}
	return err
}

func do(ctx context.Context, hc *http.Client, service, method, rawURL string, hdr http.Header, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("%s: encode: %w", service, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", service, err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	path := req.URL.Path
	resp, err := hc.Do(req)
	if err != nil {
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err // drop the URL, which may carry a key
		}
		return 0, fmt.Errorf("%s: %s %s: %w", service, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, &StatusError{Service: service, Method: method, Path: path, Code: resp.StatusCode}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("%s: %s %s: decode: %w", service, method, path, err)
	}
	return resp.StatusCode, nil
}
