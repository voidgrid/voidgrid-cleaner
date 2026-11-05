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

// Do sends method to rawURL with hdr, JSON-encoding body if it isn't nil,
// and decodes a JSON reply into out if out isn't nil.
func Do(ctx context.Context, hc *http.Client, service, method, rawURL string, hdr http.Header, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s: encode: %w", service, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
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
		return fmt.Errorf("%s: %s %s: %w", service, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return &StatusError{Service: service, Method: method, Path: path, Code: resp.StatusCode}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("%s: %s %s: decode: %w", service, method, path, err)
	}
	return nil
}
