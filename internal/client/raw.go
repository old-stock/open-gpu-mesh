package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/api"
)

// Raw performs a request with a raw body and extra headers and returns the response for
// non-error statuses (caller closes the body). Errors >= 400 become *api.Problem.
func (c *Client) Raw(ctx context.Context, method, path string, body io.Reader, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if c.APIKey != "" {
		req.Header.Set("OGM-Api-Key", c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		p := &api.Problem{}
		if json.Unmarshal(b, p) != nil || p.Title == "" {
			p = &api.Problem{Title: resp.Status, Status: resp.StatusCode, Detail: strings.TrimSpace(string(b))}
		}
		return nil, p
	}
	return resp, nil
}
