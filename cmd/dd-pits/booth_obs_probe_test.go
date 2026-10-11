package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// TestOBSProbe connects to a LIVE local OBS over obs-websocket v5 and proves the
// booth's delay control works end-to-end: it authenticates, reads the OBS +
// websocket versions and the scene list, then applies a 2s Render Delay
// (gpu_delay) filter to the first scene and removes it again — exactly the
// primitives Auto sync mode uses to drive the video delay.
//
// It only runs when OBS_PROBE=1 so it never touches CI. Override the defaults
// with OBS_URL / OBS_PASSWORD.
//
//	OBS_PROBE=1 OBS_PASSWORD=password go test ./cmd/dd-pits/ -run OBSProbe -v
func TestOBSProbe(t *testing.T) {
	if os.Getenv("OBS_PROBE") != "1" {
		t.Skip("set OBS_PROBE=1 to run the live OBS websocket probe")
	}
	url := envOr("OBS_URL", "ws://127.0.0.1:4455")
	password := envOr("OBS_PASSWORD", "password")

	cfg, err := websocket.NewConfig(url, "http://localhost/")
	if err != nil {
		t.Fatalf("bad url %q: %v", url, err)
	}
	cfg.Protocol = []string{"obswebsocket.json"}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("connect %s: %v  (is OBS running and Tools → WebSocket Server Settings enabled on that port?)", url, err)
	}
	defer ws.Close()
	t.Logf("connected to %s", url)

	// Hello (op 0) → Identify (op 1) → Identified (op 2).
	var hello struct {
		Op int `json:"op"`
		D  struct {
			ObsWebSocketVersion string `json:"obsWebSocketVersion"`
			Authentication      *struct {
				Challenge string `json:"challenge"`
				Salt      string `json:"salt"`
			} `json:"authentication"`
		} `json:"d"`
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := websocket.JSON.Receive(ws, &hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	t.Logf("OBS websocket server v%s (auth %s)", hello.D.ObsWebSocketVersion,
		map[bool]string{true: "required", false: "disabled"}[hello.D.Authentication != nil])

	identD := map[string]any{"rpcVersion": 1, "eventSubscriptions": 0}
	if hello.D.Authentication != nil {
		identD["authentication"] = obsAuth(password, hello.D.Authentication.Salt, hello.D.Authentication.Challenge)
	}
	if err := websocket.JSON.Send(ws, map[string]any{"op": 1, "d": identD}); err != nil {
		t.Fatalf("identify send: %v", err)
	}
	var id struct {
		Op int `json:"op"`
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := websocket.JSON.Receive(ws, &id); err != nil {
		t.Fatalf("identify recv: %v", err)
	}
	if id.Op != 2 {
		t.Fatalf("authentication REJECTED — OBS_PASSWORD %q does not match the OBS WebSocket password", password)
	}
	t.Log("authenticated ✓")

	// GetVersion — proves request/response works.
	if raw, err := obsRequest(ws, "GetVersion", map[string]any{}); err == nil {
		var v struct {
			ObsVersion   string `json:"obsVersion"`
			Platform     string `json:"platform"`
			RpcVersion   int    `json:"negotiatedRpcVersion"`
			SupportedRpc int    `json:"rpcVersion"`
		}
		json.Unmarshal(raw, &v)
		t.Logf("OBS %s on %s (rpc %d)", v.ObsVersion, v.Platform, v.RpcVersion)
	} else {
		t.Fatalf("GetVersion: %v", err)
	}

	// Scene list — what Auto mode would delay ("every scene").
	scenes, err := (&boothOBS{}).sceneList(ws)
	if err != nil {
		t.Fatalf("GetSceneList: %v", err)
	}
	t.Logf("%d scene(s): %v", len(scenes), scenes)
	if len(scenes) == 0 {
		t.Fatal("no scenes to test a delay on")
	}
	target := scenes[0]

	// The real thing: apply a 2s Render Delay filter to the first scene, verify
	// it exists, then remove it. This is exactly delaySource()/apply() in Auto.
	o := &boothOBS{}
	t.Logf("applying a 2.0s Render Delay (gpu_delay) to scene %q …", target)
	if err := o.delaySource(ws, target, splitDelay(2000, obsFilterCapMs)); err != nil {
		t.Fatalf("apply delay: %v", err)
	}
	if ms, ok := readDdsyncDelay(ws, target); !ok {
		t.Fatal("ddsync filter not found after apply — delay control FAILED")
	} else {
		t.Logf("verified: scene %q now carries a %dms ddsync Render Delay ✓", target, ms)
	}

	t.Log("removing the test delay …")
	if err := o.delaySource(ws, target, nil); err != nil {
		t.Fatalf("remove delay: %v", err)
	}
	if _, ok := readDdsyncDelay(ws, target); ok {
		t.Errorf("ddsync filter still present after removal — clean it up in OBS (scene %q, filter ddsync-0)", target)
	} else {
		t.Log("test delay removed — OBS left clean ✓")
	}

	t.Log("RESULT: OBS websocket delay control is WORKING — Auto sync mode will drive the video delay.")
}

func readDdsyncDelay(ws *websocket.Conn, scene string) (int, bool) {
	raw, err := obsRequest(ws, "GetSourceFilterList", map[string]any{"sourceName": scene})
	if err != nil {
		return 0, false
	}
	var lst struct {
		Filters []struct {
			FilterName     string `json:"filterName"`
			FilterSettings struct {
				DelayMs int `json:"delay_ms"`
			} `json:"filterSettings"`
		} `json:"filters"`
	}
	json.Unmarshal(raw, &lst)
	for _, f := range lst.Filters {
		if f.FilterName == obsFilterPrefix+"-0" {
			return f.FilterSettings.DelayMs, true
		}
	}
	return 0, false
}
