//go:build linux

package main

import "testing"

func TestParseXpropWindowID(t *testing.T) {
	cases := map[string]string{
		"_NET_ACTIVE_WINDOW(WINDOW): window id # 0x3a00007\n": "0x3a00007",
		// No focused X window (e.g. a native Wayland app has focus).
		"_NET_ACTIVE_WINDOW(WINDOW): window id # 0x0\n":      "",
		"_NET_ACTIVE_WINDOW:  no such atom on any window.\n": "",
		"": "",
	}
	for in, want := range cases {
		if got := parseXpropWindowID(in); got != want {
			t.Errorf("parseXpropWindowID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseXpropPID(t *testing.T) {
	if got := parseXpropPID("_NET_WM_PID(CARDINAL) = 4242\n"); got != 4242 {
		t.Errorf("got %d, want 4242", got)
	}
	if got := parseXpropPID("_NET_WM_PID:  not found.\n"); got != 0 {
		t.Errorf("missing property should yield 0, got %d", got)
	}
}

func TestParseHyprlandPID(t *testing.T) {
	out := []byte(`{"address":"0x55d","class":"firefox","title":"x","pid":31337,"xwayland":false}`)
	if got := parseHyprlandPID(out); got != 31337 {
		t.Errorf("got %d, want 31337", got)
	}
	if got := parseHyprlandPID([]byte(`{}`)); got != 0 {
		t.Errorf("no active window should yield 0, got %d", got)
	}
	if got := parseHyprlandPID([]byte(`not json`)); got != 0 {
		t.Errorf("garbage should yield 0, got %d", got)
	}
}

func TestParseSwayFocusedPID(t *testing.T) {
	tree := []byte(`{
	  "type": "root", "focused": false,
	  "nodes": [{
	    "type": "output", "focused": false,
	    "nodes": [{
	      "type": "workspace", "focused": false,
	      "nodes": [
	        {"type": "con", "focused": false, "pid": 100, "nodes": []},
	        {"type": "con", "focused": false, "nodes": [
	          {"type": "con", "focused": true, "pid": 200, "nodes": []}
	        ]}
	      ],
	      "floating_nodes": [{"type": "floating_con", "focused": false, "pid": 300}]
	    }]
	  }]
	}`)
	if got := parseSwayFocusedPID(tree); got != 200 {
		t.Errorf("got %d, want 200", got)
	}

	floating := []byte(`{"nodes":[{"nodes":[],"floating_nodes":[{"focused":true,"pid":300}]}]}`)
	if got := parseSwayFocusedPID(floating); got != 300 {
		t.Errorf("floating window: got %d, want 300", got)
	}

	// A focused empty workspace has no pid.
	empty := []byte(`{"nodes":[{"focused":true,"nodes":[]}]}`)
	if got := parseSwayFocusedPID(empty); got != 0 {
		t.Errorf("empty workspace: got %d, want 0", got)
	}
}
