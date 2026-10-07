package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/pion/webrtc/v4"
	"mvp1/internal/store"
	"mvp1/internal/wire"
)

type Client struct{ HTTP *http.Client }

func NewClient(socket string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}}
}
func (c *Client) Call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://runner"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("runner %s: HTTP %d", path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return nil
}
func (c *Client) Available() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return c.Call(ctx, "GET", "/healthz", nil, nil) == nil
}
func (c *Client) Start(ctx context.Context, s store.Session, a store.Application) error {
	return c.Call(ctx, "POST", "/sessions", wire.Start{ID: s.ID, Template: a.Template, Control: a.Control, Print: a.Print}, nil)
}
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.Call(ctx, "DELETE", "/sessions/"+id, nil, nil)
}
func (c *Client) Reset(ctx context.Context) error {
	return c.Call(ctx, "POST", "/reset", struct{}{}, nil)
}
func (c *Client) Offer(ctx context.Context, id string, v wire.Offer) (webrtc.SessionDescription, error) {
	var answer webrtc.SessionDescription
	err := c.Call(ctx, "POST", "/sessions/"+id+"/offer", v, &answer)
	return answer, err
}
func (c *Client) Lease(ctx context.Context, id string, v wire.Lease) error {
	return c.Call(ctx, "POST", "/sessions/"+id+"/lease", v, nil)
}
func (c *Client) Disconnect(ctx context.Context, id, generation string) error {
	return c.Call(ctx, "POST", "/sessions/"+id+"/disconnect", wire.Lease{Generation: generation}, nil)
}
func (c *Client) Permissions(ctx context.Context, id string, a store.Application) error {
	return c.Call(ctx, "POST", "/sessions/"+id+"/permissions", wire.Lease{Control: a.Control, Print: a.Print, PolicyVersion: a.PolicyVersion}, nil)
}

var ErrNotFound = errors.New("worker not found")

func (c *Client) Alive(ctx context.Context, id string) (bool, error) {
	var v struct {
		Alive bool `json:"alive"`
	}
	err := c.Call(ctx, "GET", "/sessions/"+id+"/alive", nil, &v)
	return v.Alive, err
}
