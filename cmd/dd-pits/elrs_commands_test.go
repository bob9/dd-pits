package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/bob9/dd-pits/internal/elrs"
)

// fakeCloud serves elrs-config and one batch of commands, recording acks.
type fakeCloud struct {
	mu       sync.Mutex
	commands []elrs.CommandWire
	acks     []elrs.CommandAck
	config   elrs.ConfigResponse
}

func (f *fakeCloud) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/dronedash/v2/elrs-config", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.config)
	})
	mux.HandleFunc("/dronedash/v2/elrs-commands", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		cmds := f.commands
		f.commands = nil
		f.mu.Unlock()
		if len(cmds) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(elrs.CommandsResponse{Commands: cmds})
	})
	mux.HandleFunc("/dronedash/v2/elrs-commands/ack", func(w http.ResponseWriter, r *http.Request) {
		var req elrs.CommandAckRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.acks = append(f.acks, req.Results...)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	return mux
}

func (f *fakeCloud) ackedStatuses() map[int64]elrs.CommandAck {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]elrs.CommandAck{}
	for _, a := range f.acks {
		out[a.ID] = a
	}
	return out
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
