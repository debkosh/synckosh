// Package client is the synckosh client: a Replica keeps one device's copy of a bucket's
// items in a Local store, and syncs it with a server. Signing in is the host's business: it
// gives the Replica an http.Client that adds whatever the server expects.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sudiptadeb/synckosh"
)

// Client speaks the protocol, version 1.
type Client struct {
	URL  string       // the server, like https://sync.example.com/
	HTTP *http.Client // carries the sign-in; http.DefaultClient if nil
}

// Push sends items, as many as the server's limits allow. Each is stored unless the server
// holds a newer version.
func (c *Client) Push(ctx context.Context, items []synckosh.Item) (synckosh.Pushed, error) {
	var p synckosh.Pushed
	if err := c.do(ctx, http.MethodPost, "v1/push", map[string]any{"items": items}, &p); err != nil {
		return synckosh.Pushed{}, err
	}
	if len(p.Results) != len(items) {
		return synckosh.Pushed{}, fmt.Errorf("push: %d results for %d items", len(p.Results), len(items))
	}
	return p, nil
}

// Pull returns the items the server stored after the sequence number after: of one collection, or
// of every collection if collection is empty.
func (c *Client) Pull(ctx context.Context, collection string, after uint64) (synckosh.Page, error) {
	q := url.Values{"after": {strconv.FormatUint(after, 10)}}
	if collection != "" {
		q.Set("collection", collection)
	}
	var p synckosh.Page
	err := c.do(ctx, http.MethodGet, "v1/pull?"+q.Encode(), nil, &p)
	return p, err
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+"/"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return json.Unmarshal(b, out)
}
