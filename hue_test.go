package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

func TestManifest(t *testing.T) {
	m := newPlugin(newIntegration()).Manifest()
	if m.ID != "hue" || m.Entry != "hue" || m.Name != "Philips Hue" || m.MinRubi != "v0.6.0" || m.Version != "v1.0.0" ||
		m.Publisher.Key != PublisherKey || m.Publisher.Name != "Rubi-Project" || m.Source != "https://github.com/Deikus-LXXVII/rubi-hue" {
		t.Fatalf("manifest: %+v", m)
	}
	if len(m.Egress) != 0 || len(m.Home) != 1 || m.Home[0] != "hue" || !strings.Contains(m.Needs, "Settings > Rubi Home") {
		t.Fatalf("egress/home/needs: %v %v %q", m.Egress, m.Home, m.Needs)
	}
	if len(m.Fields) != 0 || len(m.Secrets) != 1 || m.Secrets[0].Key != "app_key" || !m.Secrets[0].Internal {
		t.Fatalf("setup inputs: %+v %+v", m.Fields, m.Secrets)
	}
	for _, name := range []string{"hue_lights", "hue_scenes", "hue_set", "hue_scene", "hue_all_off", "hue_accounts"} {
		tool, ok := m.Tool(name)
		if !ok {
			t.Errorf("missing tool %s", name)
			continue
		}
		var schema struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(tool.InputSchema, &schema) != nil || schema.Type != "object" {
			t.Errorf("tool %s schema: %s", name, tool.InputSchema)
		}
	}
	for _, kind := range []string{kindRead, kindControl} {
		if a, ok := m.Action(kind); !ok || a.DefaultLevel != rubiplugin.None || a.Locked {
			t.Errorf("action %s: %+v", kind, a)
		}
	}
	for _, tool := range m.Tools {
		if !strings.HasPrefix(tool.Name, rubiplugin.ToolPrefix(m.ID)) {
			t.Errorf("tool %s outside the namespace", tool.Name)
		}
	}
}

// ---- setup ----

func needMore(t *testing.T, err error) *rubiplugin.NeedMore {
	t.Helper()
	var more *rubiplugin.NeedMore
	if !errors.As(err, &more) {
		t.Fatalf("want NeedMore, got %v", err)
	}
	return more
}

func TestSetupFlow(t *testing.T) {
	x := harness()
	b := oneBridge()
	h := b.fakeHome
	ctx := context.Background()
	secrets := map[string]string{"app_key": ""}

	// 1. The first form is empty: Rubi Home finds the one bridge, and the user is asked to press its button.
	_, _, err := x.validate(ctx, h, map[string]string{}, secrets)
	more := needMore(t, err)
	if more.Message != pressMessage || !strings.HasPrefix(more.Step, linkPrefix) || len(more.Fields) != 0 {
		t.Fatalf("link step: %+v", more)
	}

	// 2. Continue without pressing: the same step again, with a hint.
	fields := map[string]string{rubiplugin.StepField: more.Step}
	_, _, err = x.validate(ctx, h, fields, secrets)
	again := needMore(t, err)
	if again.Step != more.Step || !strings.Contains(again.Message, "wasn't pressed") {
		t.Fatalf("not pressed: %+v", again)
	}
	if h.pairIP != bridgeIP {
		t.Fatalf("paired at %q", h.pairIP)
	}

	// 3. Pressed: connected, with the key kept as an internal secret.
	h.pressed = true
	s, label, err := x.validate(ctx, h, fields, secrets)
	if err != nil {
		t.Fatal(err)
	}
	st := s.(settings)
	if st.Bridge != bridgeID || st.IP != bridgeIP || st.Device != "dev-1" || st.RubiAccountID() != bridgeID {
		t.Fatalf("settings: %+v", st)
	}
	if label != "Hue Bridge (3 rooms)" || secrets["app_key"] != appKey {
		t.Fatalf("label %q, secrets %v", label, secrets)
	}
	if strings.Contains(string(mustJSON(s)), appKey) {
		t.Fatal("the key is in the settings")
	}

	// Without rooms the label falls back to the bridge id's end.
	h.bridges["dev-1"].res["room"] = nil
	_, label, err = x.validate(ctx, h, fields, map[string]string{})
	if err != nil || label != "Hue Bridge 4A5B6C" {
		t.Fatalf("label %q, %v", label, err)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestSetupChoices(t *testing.T) {
	x := harness()
	ctx := context.Background()
	h := &fakeHome{devices: []rubiplugin.HomeDevice{{ID: "dev-1", Name: "Office"}, {ID: "dev-2", Name: "Mac mini"}},
		bridges: map[string]*fakeBridge{"dev-2": standardBridge()},
		extra:   []map[string]string{{"id": "001788FFFE000001", "ip": "192.0.2.77"}, {"id": "not-a-bridge", "ip": "192.0.2.78"}}}

	// Several Rubi Homes: which one?
	_, _, err := x.validate(ctx, h, map[string]string{}, map[string]string{})
	more := needMore(t, err)
	if len(more.Fields) != 1 || more.Fields[0].Key != "device" || more.Fields[0].Type != "choice" || len(more.Fields[0].Options) != 2 {
		t.Fatalf("device step: %+v", more)
	}
	if len(h.calls) != 0 {
		t.Fatalf("called Rubi Home before choosing: %v", h.calls)
	}

	// Several bridges on that network: which one? (Invalid entries are left out.)
	fields := map[string]string{rubiplugin.StepField: more.Step, "device": "dev-2"}
	_, _, err = x.validate(ctx, h, fields, map[string]string{})
	more = needMore(t, err)
	if len(more.Fields) != 1 || more.Fields[0].Key != "bridge" || len(more.Fields[0].Options) != 2 {
		t.Fatalf("bridge step: %+v", more)
	}
	opt := more.Fields[0].Options[0]
	if opt.Key != bridgeID+"@"+bridgeIP || !strings.Contains(opt.Label, "4A5B6C") || h.calls[0] != "dev-2 hue.discover" {
		t.Fatalf("bridge option %+v, calls %v", opt, h.calls)
	}

	// The chosen bridge: press its button.
	fields[rubiplugin.StepField] = more.Step
	fields["bridge"] = opt.Key
	_, _, err = x.validate(ctx, h, fields, map[string]string{})
	more = needMore(t, err)
	p, ok := parseStep(more.Step)
	if !ok || p.Device != "dev-2" || p.Bridge != bridgeID || p.IP != bridgeIP {
		t.Fatalf("link step %+v %v", p, ok)
	}
	h.pressed = true
	fields[rubiplugin.StepField] = more.Step
	s, _, err := x.validate(ctx, h, fields, map[string]string{})
	if err != nil || s.(settings).Device != "dev-2" {
		t.Fatalf("%+v %v", s, err)
	}

	// A tampered step or choice is refused.
	if _, _, err := x.validate(ctx, h, map[string]string{rubiplugin.StepField: "link:garbage"}, map[string]string{}); err == nil {
		t.Fatal("damaged step accepted")
	}
	fields = map[string]string{"device": "dev-2", "bridge": "x@192.0.2.1"}
	if _, _, err := x.validate(ctx, h, fields, map[string]string{}); err == nil || errors.As(err, new(*rubiplugin.NeedMore)) {
		t.Fatalf("bad bridge choice: %v", err)
	}
}

func TestSetupProblems(t *testing.T) {
	x := harness()
	ctx := context.Background()

	_, _, err := x.validate(ctx, &fakeHome{}, map[string]string{}, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "Settings > Rubi Home") {
		t.Fatalf("no Rubi Home: %v", err)
	}

	h := oneBridge().fakeHome
	h.offline = true
	_, _, err = x.validate(ctx, h, map[string]string{}, map[string]string{})
	if err == nil || !strings.HasPrefix(err.Error(), "Rubi Home didn't answer") {
		t.Fatalf("offline: %v", err)
	}

	h.offline = false
	h.bridges = map[string]*fakeBridge{}
	_, _, err = x.validate(ctx, h, map[string]string{}, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "found no Hue Bridge") {
		t.Fatalf("no bridge: %v", err)
	}
}

// ---- reading ----

func TestLights(t *testing.T) {
	x, b := harness(), oneBridge()
	out := call(t, x, b, "lights", nil)

	rooms := get(out, "rooms").([]any)
	if len(rooms) != 3 || get(out, "rooms", 0, "name") != "Bedroom" || get(out, "rooms", 2, "name") != "Living room" {
		t.Fatalf("rooms: %v", rooms)
	}
	living := get(out, "rooms", 2)
	if get(living, "on") != true || get(living, "brightness") != 65.0 {
		t.Fatalf("living room: %v", living)
	}
	ceiling, floor := get(living, "lights", 0), get(living, "lights", 1)
	if get(ceiling, "name") != "Ceiling" || get(ceiling, "brightness") != 80.0 || get(ceiling, "color") != "warm white (2700 K)" ||
		get(ceiling, "reachable") != nil {
		t.Fatalf("ceiling: %v", ceiling)
	}
	if get(floor, "brightness") != 50.0 || !strings.HasPrefix(get(floor, "color").(string), "red #") {
		t.Fatalf("floor lamp: %v", floor)
	}
	bedside := get(out, "rooms", 0, "lights", 0)
	if get(bedside, "reachable") != false || get(bedside, "on") != false || get(bedside, "brightness") != nil {
		t.Fatalf("bedside: %v", bedside)
	}
	if get(out, "rooms", 1, "on") != false || get(out, "rooms", 1, "brightness") != nil {
		t.Fatalf("kitchen: %v", get(out, "rooms", 1))
	}
	if get(out, "zones", 0, "name") != "Downstairs" || len(get(out, "zones", 0, "lights").([]any)) != 2 ||
		get(out, "zones", 0, "lights", 0) != "Ceiling" {
		t.Fatalf("zones: %v", get(out, "zones"))
	}
	if get(out, "lights_in_no_room", 0, "name") != "Plug" || get(out, "lights_in_no_room", 0, "color") != nil {
		t.Fatalf("other lights: %v", get(out, "lights_in_no_room"))
	}

	// One room.
	out = call(t, x, b, "lights", map[string]any{"room": "kitchen"})
	if len(get(out, "rooms").([]any)) != 1 || get(out, "zones") != nil || get(out, "lights_in_no_room") != nil {
		t.Fatalf("kitchen only: %v", out)
	}
}

func TestCache(t *testing.T) {
	x, b := harness(), oneBridge()
	now := time.Unix(1_800_000_000, 0)
	x.cache.now = func() time.Time { return now }

	call(t, x, b, "lights", nil)
	call(t, x, b, "lights", nil)
	call(t, x, b, "set", map[string]any{"target": "kitchen", "on": true}) // name lookup from the cache
	if n := b.gets("dev-1", "light"); n != 1 {
		t.Fatalf("lights fetched %d times", n)
	}
	call(t, x, b, "lights", nil) // the change dropped the cache
	if n := b.gets("dev-1", "light"); n != 2 {
		t.Fatalf("after a change: fetched %d times", n)
	}
	now = now.Add(31 * time.Second)
	call(t, x, b, "lights", nil)
	if n := b.gets("dev-1", "light"); n != 3 {
		t.Fatalf("after 31 s: fetched %d times", n)
	}
}

func TestResolve(t *testing.T) {
	x, b := harness(), oneBridge()
	a, err := x.accountFor(b, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.home(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"Kitchen":            "Kitchen (room)",
		"the kitchen":        "Kitchen (room)",
		"LIVING ROOM":        "Living room (room)",
		"livingroom":         "Living room (room)",
		"living room lights": "Living room (room)",
		"living":             "Living room (room)",
		"downstairs":         "Downstairs (zone)",
		"floor":              "Floor lamp (light in Living room)",
		"lamp":               "Floor lamp (light in Living room)",
		"kitchen ceiling":    "Ceiling (light in Kitchen)",
		"Kitchen / Ceiling":  "Ceiling (light in Kitchen)",
		"bedsdie":            "Bedside (light in Bedroom)",
		"bedroom":            "Bedroom (room)",
		"plug":               "Plug (light)",
		"all":                "all lights",
		uid("light", 2):      "Floor lamp (light in Living room)",
		uid("grouped", 2):    "Kitchen (room)",
		uid("zone", 1):       "Downstairs (zone)",
	} {
		got, err := m.resolve(q)
		if err != nil || got.describe() != want {
			t.Errorf("%q: got %q, %v; want %q", q, got.describe(), err, want)
		}
	}
	for q, want := range map[string]string{
		"ceiling": "could be Ceiling (light in Kitchen), Ceiling (light in Living room)",
		"garage":  "no room, zone or light called",
		"":        "say which",
	} {
		if _, err := m.resolve(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", q, err)
		}
	}
}

// ---- changing ----

func lastPut(t *testing.T, b *fakeBase) put {
	t.Helper()
	puts := b.takePuts("dev-1")
	if len(puts) != 1 {
		t.Fatalf("want 1 PUT, got %+v", puts)
	}
	return puts[0]
}

func TestSet(t *testing.T) {
	x, b := harness(), oneBridge()

	// A room: its grouped light, switched on with the brightness.
	out := call(t, x, b, "set", map[string]any{"target": "living room", "brightness": 40})
	p := lastPut(t, b)
	if p.Path != "/clip/v2/resource/grouped_light/"+uid("grouped", 1) || get(p.Body, "on", "on") != true ||
		get(p.Body, "dimming", "brightness") != 40.0 || len(p.Body) != 2 {
		t.Fatalf("room brightness: %+v", p)
	}
	if out["done"] != true || out["target"] != "Living room (room)" {
		t.Fatalf("result: %v", out)
	}

	// A light: a color name, as xy, with a fade.
	call(t, x, b, "set", map[string]any{"target": "floor lamp", "color": "Blue", "transition": 2.5})
	p = lastPut(t, b)
	if p.Path != "/clip/v2/resource/light/"+uid("light", 2) || math.Abs(get(p.Body, "color", "xy", "x").(float64)-0.15) > 0.001 ||
		math.Abs(get(p.Body, "color", "xy", "y").(float64)-0.06) > 0.001 || get(p.Body, "dynamics", "duration") != 2500.0 ||
		get(p.Body, "on", "on") != true {
		t.Fatalf("light color: %+v", p)
	}

	// A hex color on a zone.
	call(t, x, b, "set", map[string]any{"target": "downstairs", "color": "#ff8800", "on": true})
	p = lastPut(t, b)
	if p.Path != "/clip/v2/resource/grouped_light/"+uid("grouped", 4) || get(p.Body, "color", "xy") == nil {
		t.Fatalf("zone hex: %+v", p)
	}

	// Kelvin, as mirek; kept within what the light supports.
	call(t, x, b, "set", map[string]any{"target": "kitchen ceiling", "color": "3000K"})
	p = lastPut(t, b)
	if get(p.Body, "color_temperature", "mirek") != 333.0 || get(p.Body, "color") != nil {
		t.Fatalf("kelvin: %+v", p)
	}
	call(t, x, b, "set", map[string]any{"target": "kitchen ceiling", "color": "candle"})
	if p = lastPut(t, b); get(p.Body, "color_temperature", "mirek") != 454.0 {
		t.Fatalf("clamped mirek: %+v", p)
	}
	call(t, x, b, "set", map[string]any{"target": "bedroom", "color": "warm white"})
	if p = lastPut(t, b); get(p.Body, "color_temperature", "mirek") != 370.0 {
		t.Fatalf("warm white: %+v", p)
	}

	// Brightness 0 is off; off alone is off.
	call(t, x, b, "set", map[string]any{"target": "kitchen", "brightness": 0})
	if p = lastPut(t, b); get(p.Body, "on", "on") != false || p.Body["dimming"] != nil {
		t.Fatalf("brightness 0: %+v", p)
	}
	call(t, x, b, "set", map[string]any{"target": "all", "on": false})
	if p = lastPut(t, b); p.Path != "/clip/v2/resource/grouped_light/"+uid("grouped", 5) {
		t.Fatalf("all: %+v", p)
	}

	// Refused before anything is sent.
	for args, want := range map[string]string{
		`{"target":"kitchen ceiling","color":"red"}`:     "shows only whites",
		`{"target":"plug","brightness":50}`:              "can't be dimmed",
		`{"target":"kitchen"}`:                           "say what to change",
		`{"target":"kitchen","brightness":150}`:          "0 to 100",
		`{"target":"kitchen","color":"chartreuse-ish"}`:  "unknown color",
		`{"target":"kitchen","on":false,"color":"red"}`:  "can't switch off",
		`{"target":"kitchen","on":true,"transition":-1}`: "transition",
		`{"target":"ceiling","on":true}`:                 "could be",
	} {
		var in map[string]any
		_ = json.Unmarshal([]byte(args), &in)
		if _, err := callErr(x, b, "set", in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", args, err)
		}
	}
	if puts := b.takePuts("dev-1"); len(puts) != 0 {
		t.Fatalf("refused changes were sent: %+v", puts)
	}
}

func TestScenes(t *testing.T) {
	x, b := harness(), oneBridge()
	out := call(t, x, b, "scenes", nil)
	if get(out, "scenes", 0, "name") != "Bedroom" || get(out, "scenes", 0, "scenes", 0) != "Relax" ||
		get(out, "scenes", 2, "name") != "Living room" || get(out, "scenes", 2, "active") != "Read" ||
		len(get(out, "scenes", 2, "scenes").([]any)) != 2 || len(get(out, "scenes").([]any)) != 3 {
		t.Fatalf("scenes: %v", out)
	}

	// Two rooms have a Relax scene.
	if _, err := callErr(x, b, "scene", map[string]any{"name": "relax"}); err == nil ||
		!strings.Contains(err.Error(), "Relax (Bedroom), Relax (Living room)") {
		t.Fatalf("ambiguous: %v", err)
	}
	out = call(t, x, b, "scene", map[string]any{"name": "relax", "room": "bedroom"})
	p := lastPut(t, b)
	if p.Path != "/clip/v2/resource/scene/"+uid("scene", 3) || get(p.Body, "recall", "action") != "active" ||
		get(p.Body, "recall", "duration") != nil || out["scene"] != "Relax (Bedroom)" {
		t.Fatalf("recall: %+v %v", p, out)
	}
	call(t, x, b, "scene", map[string]any{"name": "living room relax", "transition": 1})
	if p = lastPut(t, b); p.Path != "/clip/v2/resource/scene/"+uid("scene", 1) || get(p.Body, "recall", "duration") != 1000.0 {
		t.Fatalf("recall by room and name: %+v", p)
	}
	call(t, x, b, "scene", map[string]any{"name": "energise"})
	if p = lastPut(t, b); p.Path != "/clip/v2/resource/scene/"+uid("scene", 4) {
		t.Fatalf("typo: %+v", p)
	}
	if _, err := callErr(x, b, "scene", map[string]any{"name": "party"}); err == nil {
		t.Fatal("unknown scene recalled")
	}
}

func TestAllOff(t *testing.T) {
	x, b := harness(), oneBridge()
	call(t, x, b, "all_off", nil)
	p := lastPut(t, b)
	if p.Path != "/clip/v2/resource/grouped_light/"+uid("grouped", 5) || get(p.Body, "on", "on") != false || len(p.Body) != 1 {
		t.Fatalf("all off: %+v", p)
	}

	// Without a home-wide group: each room that is on, then the lights in no room that are on.
	br := b.bridges["dev-1"]
	br.res["grouped_light"] = br.res["grouped_light"][:4]
	call(t, x, b, "all_off", map[string]any{"transition": 3})
	puts := b.takePuts("dev-1")
	if len(puts) != 2 || puts[0].Path != "/clip/v2/resource/grouped_light/"+uid("grouped", 1) ||
		puts[1].Path != "/clip/v2/resource/light/"+uid("light", 5) || get(puts[1].Body, "dynamics", "duration") != 3000.0 {
		t.Fatalf("fallback: %+v", puts)
	}
}

func TestApprovals(t *testing.T) {
	x, b := harness(), oneBridge()
	b.levels = map[string]rubiplugin.Level{kindControl: rubiplugin.Chat}

	// Reading still needs nothing.
	call(t, x, b, "lights", nil)

	out := call(t, x, b, "set", map[string]any{"target": "kitchen", "brightness": 30, "color": "warm white"})
	if out["status"] != "awaiting_approval" || len(b.pending) != 1 {
		t.Fatalf("not submitted: %v", out)
	}
	if puts := b.takePuts("dev-1"); len(puts) != 0 {
		t.Fatalf("changed before approval: %+v", puts)
	}
	r := b.pending[0]
	if r.Kind != kindControl || r.Summary != "Change Hue lights: kitchen (30%, warm white)" {
		t.Fatalf("request: %+v", r)
	}

	// Approved: it runs with exactly the submitted arguments.
	payload, _ := json.Marshal(r.Payload)
	res, err := x.executeTool(context.Background(), b, kindControl, payload)
	if err != nil || roundTrip(res)["done"] != true {
		t.Fatalf("execute: %v %v", res, err)
	}
	p := lastPut(t, b)
	if p.Path != "/clip/v2/resource/grouped_light/"+uid("grouped", 2) || get(p.Body, "dimming", "brightness") != 30.0 {
		t.Fatalf("executed: %+v", p)
	}
	// An approval of one kind can't run a tool of another.
	if _, err := x.executeTool(context.Background(), b, kindRead, payload); err == nil {
		t.Fatal("ran under the wrong kind")
	}

	// Raised reading, and the other control tools.
	b.levels[kindRead] = rubiplugin.Strong
	if out := call(t, x, b, "scenes", nil); out["status"] != "awaiting_approval" {
		t.Fatalf("read not gated: %v", out)
	}
	call(t, x, b, "all_off", nil)
	call(t, x, b, "scene", map[string]any{"name": "Read"})
	if len(b.pending) != 4 || b.pending[2].Summary != "Switch off all Hue lights" || b.pending[3].Summary != "Recall the Hue scene Read" {
		t.Fatalf("pending: %+v", b.pending)
	}
	if puts := b.takePuts("dev-1"); len(puts) != 0 {
		t.Fatalf("changed before approval: %+v", puts)
	}
}

func TestBridgeMoved(t *testing.T) {
	x, b := harness(), oneBridge()
	call(t, x, b, "set", map[string]any{"target": "kitchen", "on": true})
	if p := lastPut(t, b); p.IP != bridgeIP {
		t.Fatalf("sent to %s", p.IP)
	}

	// The router gave the bridge a new address. The first request still goes to the old one; Rubi Home
	// finds the bridge again by its id and says where it is now.
	b.bridges["dev-1"].ip = "192.0.2.99"
	a, _ := x.accountFor(b, "")
	if _, _, err := a.request(context.Background(), "GET", resourceBase+"light", nil); err != nil {
		t.Fatal(err)
	}
	var st allState
	_ = json.Unmarshal(b.state, &st)
	if !strings.Contains(string(st.Accounts[bridgeID]), "192.0.2.99") {
		t.Fatalf("new address not saved: %s", b.state)
	}
	// Next time it goes straight to the new address.
	call(t, x, b, "set", map[string]any{"target": "kitchen", "on": true})
	if p := lastPut(t, b); p.IP != "192.0.2.99" {
		t.Fatalf("sent to %s", p.IP)
	}
}

func TestHomeOffline(t *testing.T) {
	x, b := harness(), oneBridge()
	b.offline = true
	for _, tool := range []string{"lights", "scenes", "all_off"} {
		_, err := callErr(x, b, tool, nil)
		if err == nil || !strings.HasPrefix(err.Error(), "Rubi Home didn't answer") {
			t.Errorf("%s: %v", tool, err)
		}
	}
	_, err := callErr(x, b, "set", map[string]any{"target": "kitchen", "on": true})
	if err == nil || !strings.HasPrefix(err.Error(), "Rubi Home didn't answer") {
		t.Errorf("set: %v", err)
	}

	// Back online, and the cached map from before still works.
	b.offline = false
	call(t, x, b, "lights", nil)
}

func TestBridgeErrors(t *testing.T) {
	x, b := harness(), oneBridge()
	b.secrets[bridgeID] = "revoked"
	if _, err := callErr(x, b, "lights", nil); err == nil || !strings.Contains(err.Error(), "no longer accepts Rubi's key") {
		t.Fatalf("revoked key: %v", err)
	}
	b.secrets[bridgeID] = appKey
	b.bridges["dev-1"].status = 503
	if _, err := callErr(x, b, "lights", nil); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy: %v", err)
	}
	b.bridges["dev-1"].status = 0
	b.settings[bridgeID] = settings{Device: "gone", Bridge: bridgeID, IP: bridgeIP}
	if _, err := callErr(x, b, "lights", nil); err == nil || !strings.Contains(err.Error(), "isn't paired anymore") {
		t.Fatalf("removed Rubi Home: %v", err)
	}
}

func TestAccounts(t *testing.T) {
	x, b := harness(), oneBridge()
	second := standardBridge()
	second.id, second.ip, second.key = "001788fffe000002", "192.0.2.20", "key-2"
	second.res["room"] = second.res["room"][:1]
	b.devices = append(b.devices, rubiplugin.HomeDevice{ID: "dev-2", Name: "Cabin"})
	b.bridges["dev-2"] = second
	b.accounts = append(b.accounts, rubiplugin.AccountInfo{ID: second.id, Label: "Cabin bridge"})
	b.settings[second.id] = settings{Device: "dev-2", DeviceName: "Cabin", Bridge: second.id, IP: second.ip}
	b.secrets[second.id] = "key-2"

	res, err := x.listAccounts(b)
	if err != nil {
		t.Fatal(err)
	}
	out := roundTrip(res)
	if get(out, "accounts", 1, "account") != "Cabin bridge" || get(out, "accounts", 0, "default") != true {
		t.Fatalf("accounts: %v", out)
	}
	out = call(t, x, b, "lights", map[string]any{"account": "cabin bridge"})
	if len(get(out, "rooms").([]any)) != 1 {
		t.Fatalf("second bridge: %v", out)
	}
	call(t, x, b, "set", map[string]any{"target": "living room", "on": false, "account": "Cabin bridge"})
	if puts := b.takePuts("dev-2"); len(puts) != 1 || len(b.takePuts("dev-1")) != 0 {
		t.Fatalf("wrong bridge: %+v", puts)
	}

	// An approval names the bridge and runs on it.
	b.levels = map[string]rubiplugin.Level{kindControl: rubiplugin.Chat}
	call(t, x, b, "all_off", map[string]any{"account": "Cabin bridge"})
	r := b.pending[0]
	if r.Summary != "Switch off all Hue lights (Cabin bridge)" {
		t.Fatalf("summary: %q", r.Summary)
	}
	payload, _ := json.Marshal(r.Payload)
	if _, err := x.executeTool(context.Background(), b, kindControl, payload); err != nil {
		t.Fatal(err)
	}
	if puts := b.takePuts("dev-2"); len(puts) != 1 {
		t.Fatalf("approved on the wrong bridge: %+v", puts)
	}
	if _, err := callErr(x, b, "lights", map[string]any{"account": "nope"}); err == nil {
		t.Fatal("unknown account accepted")
	}
}

func TestGroupSpacing(t *testing.T) {
	x, b := harness(), oneBridge()
	x.space = newSpacer(80 * time.Millisecond)
	start := time.Now()
	call(t, x, b, "set", map[string]any{"target": "kitchen", "on": true})
	call(t, x, b, "set", map[string]any{"target": "bedroom", "on": true})
	call(t, x, b, "set", map[string]any{"target": "floor lamp", "on": true}) // a single light isn't held back
	if d := time.Since(start); d < 80*time.Millisecond || d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

// ---- colors ----

func TestColors(t *testing.T) {
	for in, want := range map[string]int{"warm white": 370, "2700K": 370, "6500 kelvin": 154, "daylight": 154, "4000": 250,
		"Soft White": 333, "candle": 500} {
		c, err := parseColor(in)
		if err != nil || c.Mirek == nil || *c.Mirek != want {
			t.Errorf("%q: %+v %v", in, c, err)
		}
	}
	for _, in := range []string{"red", "#00f", "00FF00", "purple light"} {
		if c, err := parseColor(in); err != nil || c.XY == nil {
			t.Errorf("%q: %+v %v", in, c, err)
		}
	}
	for _, in := range []string{"#000000", "500K", "plaid", "#12345"} {
		if _, err := parseColor(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	// Round trips name the color roughly.
	for name, want := range map[string]string{"red": "red #ff0000", "blue": "blue #0000ff", "green": "green #00ff00",
		"purple": "purple", "yellow": "yellow", "orange": "orange", "pink": "pink"} {
		c, _ := parseColor(name)
		got := describeColor(&light{XY: c.XY})
		if !strings.HasPrefix(got, want) {
			t.Errorf("%s: %q", name, got)
		}
	}
	mirek := 222
	if got := describeColor(&light{Mirek: &mirek, XY: &xy{X: 0.3, Y: 0.3}}); got != "cool white (4500 K)" {
		t.Errorf("white: %q", got)
	}
	if got := describeColor(&light{XY: &xy{X: 0.3127, Y: 0.329}}); !strings.HasPrefix(got, "white #") {
		t.Errorf("D65: %q", got)
	}
	if p := whiteXY(370); math.Abs(p.X-0.46) > 0.01 || math.Abs(p.Y-0.41) > 0.01 {
		t.Errorf("2700 K: %+v", p)
	}
}
