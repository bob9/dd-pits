package main

// A small Socket.IO client: just enough to call a RotorHazard plugin's socket
// handler and wait for its answer, the way FPVTrackside does. It uses Engine.IO
// v4's long-polling transport (plain HTTP), which RotorHazard's server always
// offers.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type sioClient struct {
	base string
	sid  string
	http *http.Client
	mu   sync.Mutex
	ack  int
}

const sioSep = "\x1e"

func (c *sioClient) url() string {
	u := strings.TrimSuffix(c.base, "/") + "/socket.io/?EIO=4&transport=polling&t=" + fmt.Sprint(time.Now().UnixNano())
	if c.sid != "" {
		u += "&sid=" + c.sid
	}
	return u
}

func (c *sioClient) get() ([]string, error) {
	resp, err := c.http.Get(c.url())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("socket.io HTTP %d: %s", resp.StatusCode, shortBody(body))
	}
	return strings.Split(string(body), sioSep), nil
}

func (c *sioClient) post(packet string) error {
	resp, err := c.http.Post(c.url(), "text/plain;charset=UTF-8", bytes.NewBufferString(packet))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("socket.io HTTP %d", resp.StatusCode)
	}
	return nil
}

// dialSIO opens a session and joins the default namespace.
func dialSIO(base string) (*sioClient, error) {
	c := &sioClient{base: base, http: &http.Client{Timeout: 30 * time.Second}}
	pk, err := c.get()
	if err != nil {
		return nil, err
	}
	if len(pk) == 0 || !strings.HasPrefix(pk[0], "0") {
		return nil, errors.New("not a Socket.IO server")
	}
	var open struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal([]byte(pk[0][1:]), &open); err != nil || open.SID == "" {
		return nil, errors.New("Socket.IO handshake failed")
	}
	c.sid = open.SID
	if err := c.post("40"); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pk, err := c.get()
		if err != nil {
			return nil, err
		}
		for _, p := range pk {
			if strings.HasPrefix(p, "40") {
				return c, nil
			}
			if strings.HasPrefix(p, "44") {
				return nil, fmt.Errorf("RotorHazard refused the connection: %s", p[2:])
			}
		}
	}
	return nil, errors.New("Socket.IO connect timed out")
}

// call emits an event and waits for the handler's answer (its ack).
func (c *sioClient) call(event string, arg any, out any, timeout time.Duration) error {
	c.mu.Lock()
	c.ack++
	id := c.ack
	c.mu.Unlock()
	payload, err := json.Marshal([]any{event, arg})
	if err != nil {
		return err
	}
	if err := c.post(fmt.Sprintf("42%d%s", id, payload)); err != nil {
		return err
	}
	want := fmt.Sprintf("43%d", id)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pk, err := c.get()
		if err != nil {
			return err
		}
		for _, p := range pk {
			switch {
			case p == "2":
				_ = c.post("3") // keep the session alive
			case strings.HasPrefix(p, want):
				var args []json.RawMessage
				if err := json.Unmarshal([]byte(p[len(want):]), &args); err != nil {
					return err
				}
				if len(args) == 0 || string(args[0]) == "null" {
					return errNoTraces
				}
				if out == nil {
					return nil
				}
				return json.Unmarshal(args[0], out)
			}
		}
	}
	return fmt.Errorf("%s: no answer from RotorHazard", event)
}

// emit sends an event without waiting for an answer.
func (c *sioClient) emit(event string, arg any) error {
	payload, err := json.Marshal([]any{event, arg})
	if err != nil {
		return err
	}
	return c.post("42" + string(payload))
}

func (c *sioClient) close() { _ = c.post("41") }
