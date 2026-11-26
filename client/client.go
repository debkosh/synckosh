// Package client is the synckosh client: a Replica keeps one device's copy of a bucket's
// items in a Local store, and syncs it with a server. Signing in is the host's business: it
// gives the Client a bearer token, or an http.Client that adds whatever the server expects.
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

	"github.com/debkosh/synckosh"
)

// Client speaks the protocol, version 1.
type Client struct {
	URL   string       // the server, like https://sync.example.com/
	Token string       // sent as "Authorization: Bearer <Token>" when set
	HTTP  *http.Client // carries any other sign-in; http.DefaultClient if nil
}

// TooLargeError is the server refusing a whole push as over its limits, which it sends along.
type TooLargeError struct {
	Limits synckosh.Limits
	Err    string
}

func (e *TooLargeError) Error() string { return "push: too large for the server: " + e.Err }

// Push sends items, as many as the server's limits allow. Each is stored unless the server
// holds a newer version. A push over the server's limits fails with a *TooLargeError.
func (c *Client) Push(ctx context.Context, items []synckosh.Item) (synckosh.Pushed, error) {
	var p synckosh.Pushed
	if err := c.do(ctx, http.MethodPost, "v1/push", map[string]any{"items": items}, &p, 0); err != nil {
		return synckosh.Pushed{}, err
	}
	if len(p.Results) != len(items) {
		return synckosh.Pushed{}, fmt.Errorf("push: %d results for %d items", len(p.Results), len(items))
	}
	return p, nil
}

// pageSlack is what a page may carry past its byte limit: the rest of the answer, and the one
// item a page always holds however big.
func pageSlack(itemBytes int) int64 { return int64(itemBytes) + 64<<10 }

// Pull returns the items the server stored after the sequence number after that pass the
// filter; the zero Filter is every item. The server stops the page at pageBytes of items,
// or its own limit if lower, but always gives at least one; zero means the default limit. The
// answer is read only that far, with room for one item of the default size.
func (c *Client) Pull(ctx context.Context, f synckosh.Filter, after uint64, pageBytes int) (synckosh.Page, error) {
	limits := synckosh.Limits{PageBytes: pageBytes}.OrDefaults()
	q := url.Values{"after": {strconv.FormatUint(after, 10)}, "bytes": {strconv.Itoa(limits.PageBytes)},
		"collection": f.Collections, "datatype": f.Datatypes}
	var p synckosh.Page
	err := c.do(ctx, http.MethodGet, "v1/pull?"+q.Encode(), nil, &p, int64(limits.PageBytes)+pageSlack(limits.ItemBytes))
	return p, err
}

// do makes a call, reading at most most bytes of the answer, or all of it if most is 0.
func (c *Client) do(ctx context.Context, method, path string, body, out any, most int64) error {
	var rd io.Reader
	if body != nil {
		b, err := synckosh.Marshal(body)
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
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
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
	var answer io.Reader = resp.Body
	if most > 0 {
		answer = io.LimitReader(resp.Body, most+1)
	}
	b, err := io.ReadAll(answer)
	if err != nil {
		return err
	}
	if most > 0 && int64(len(b)) > most {
		return fmt.Errorf("%s %s: the answer is over %d bytes", method, path, most)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error  string           `json:"error"`
			Limits *synckosh.Limits `json:"limits"`
		}
		json.Unmarshal(b, &e)
		if resp.StatusCode == http.StatusRequestEntityTooLarge && e.Limits != nil {
			return &TooLargeError{Limits: *e.Limits, Err: e.Error}
		}
		if e.Error != "" {
			return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return json.Unmarshal(b, out)
}
