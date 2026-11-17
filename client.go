package synckosh

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
	"time"
)

// Client speaks the server's protocol, version 1. One client is one device's token.
type Client struct {
	URL   string // the server, like https://sync.example.com
	Token string
	HTTP  *http.Client
}

// Account is what signing up returns: the new account, this device, and its token.
type Account struct {
	Account string `json:"account"`
	Device  string `json:"device"`
	Token   string `json:"token"`
}

// Device is one device on an account.
type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
}

// Result is the server's answer for one pushed item. Either it was stored, with its change
// number, or the server holds a version the edit wasn't made on, returned in Current.
type Result struct {
	OK      bool   `json:"ok"`
	Seq     int64  `json:"seq,omitempty"`
	Current *Item  `json:"current,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Page is one page of the changes feed.
type Page struct {
	Items []Item `json:"items"`
	Next  int64  `json:"next"`
	More  bool   `json:"more"`
}

// Signup makes a new account on the server, with this device in it.
func Signup(ctx context.Context, server, deviceName string) (*Client, Account, error) {
	c := &Client{URL: strings.TrimRight(server, "/")}
	var a Account
	err := c.do(ctx, http.MethodPost, "/v1/accounts", map[string]string{"name": deviceName}, &a)
	if err != nil {
		return nil, Account{}, err
	}
	c.Token = a.Token
	return c, a, nil
}

// AddDevice makes a token for another device on this account. The new device signs in with it.
func (c *Client) AddDevice(ctx context.Context, name string) (Account, error) {
	var a Account
	err := c.do(ctx, http.MethodPost, "/v1/devices", map[string]string{"name": name}, &a)
	return a, err
}

func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var r struct {
		Devices []Device `json:"devices"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/devices", nil, &r)
	return r.Devices, err
}

// RemoveDevice ends a device's token. It can no longer read or write.
func (c *Client) RemoveDevice(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/devices/"+url.PathEscape(id), nil, nil)
}

// Push sends items. The server keeps each one made on the version it holds, the one the
// item's Base names; for the others it returns the version it holds, to merge with.
func (c *Client) Push(ctx context.Context, items []Item) ([]Result, error) {
	var r struct {
		Results []Result `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/items", map[string]any{"items": items}, &r); err != nil {
		return nil, err
	}
	if len(r.Results) != len(items) {
		return nil, fmt.Errorf("push: %d results for %d items", len(r.Results), len(items))
	}
	return r.Results, nil
}

// Pull returns the items changed after change number since, of one type or all if typ is
// empty. With wait, the server holds the request until something changes or wait passes.
func (c *Client) Pull(ctx context.Context, since int64, typ string, wait time.Duration) (Page, error) {
	q := url.Values{"since": {strconv.FormatInt(since, 10)}}
	if typ != "" {
		q.Set("type", typ)
	}
	if wait > 0 {
		q.Set("wait", strconv.Itoa(int(wait/time.Second)))
	}
	var p Page
	err := c.do(ctx, http.MethodGet, "/v1/items?"+q.Encode(), nil, &p)
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
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, rd)
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
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}
