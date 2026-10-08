package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// The tests never touch a network: a fake Rubi (fakeBase) answers the host calls, and a fake Rubi Home
// (fakeHome) answers the hue.* operations with canned CLIP v2 data, as a bridge would.

const (
	bridgeID = "001788fffe4a5b6c"
	bridgeIP = "192.0.2.10" // TEST-NET-1: never a real device
	appKey   = "fake-app-key"
)

// uid makes a CLIP v2 style resource id.
func uid(kind string, n int) string {
	h := map[string]int{"light": 1, "device": 2, "room": 3, "zone": 4, "grouped": 5, "scene": 6, "zigbee": 7, "home": 8}[kind]
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", h, n)
}

// fakeBridge is one bridge's resources and what was sent to it.
type fakeBridge struct {
	id, ip, key string
	res         map[string][]map[string]any // rtype → resources
	puts        []put
	gets        map[string]int // rtype → GETs
	status      int            // answer every request with this status (0 = normal)
}

type put struct {
	Path string
	Body map[string]any
	IP   string
}

// standardBridge has three rooms (Living room, Kitchen, Bedroom), a zone (Downstairs), five lights, a
// home-wide group and four scenes.
func standardBridge() *fakeBridge {
	b := &fakeBridge{id: bridgeID, ip: bridgeIP, key: appKey, gets: map[string]int{}}
	dev := func(n int) map[string]any { return map[string]any{"rid": uid("device", n), "rtype": "device"} }
	ct := func(mirek any, valid bool) map[string]any {
		return map[string]any{"mirek": mirek, "mirek_valid": valid,
			"mirek_schema": map[string]any{"mirek_minimum": 153, "mirek_maximum": 454}}
	}
	color := func(x, y float64) map[string]any {
		return map[string]any{"xy": map[string]any{"x": x, "y": y}, "gamut_type": "C"}
	}
	light := func(n int, name string, on bool, extra map[string]any) map[string]any {
		l := map[string]any{"id": uid("light", n), "id_v1": fmt.Sprintf("/lights/%d", n), "type": "light", "owner": dev(n),
			"metadata": map[string]any{"name": name, "archetype": "sultan_bulb", "function": "mixed"},
			"on":       map[string]any{"on": on}, "mode": "normal", "identify": map[string]any{}, "service_id": 0}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}
	dim := func(b float64) map[string]any { return map[string]any{"brightness": b, "min_dim_level": 0.2} }
	b.res = map[string][]map[string]any{
		"light": {
			light(1, "Ceiling", true, map[string]any{"dimming": dim(80), "color_temperature": ct(370, true), "color": color(0.4578, 0.41)}),
			light(2, "Floor lamp", true, map[string]any{"dimming": dim(50.4), "color_temperature": ct(nil, false), "color": color(0.675, 0.322)}),
			light(3, "Ceiling", false, map[string]any{"dimming": dim(100), "color_temperature": ct(250, true)}),
			light(4, "Bedside", false, map[string]any{"dimming": dim(30), "color_temperature": ct(300, true), "color": color(0.44, 0.40)}),
			light(5, "Plug", true, nil),
		},
		"room": {
			groupRes("room", 1, "Living room", []map[string]any{dev(1), dev(2)}, 1),
			groupRes("room", 2, "Kitchen", []map[string]any{dev(3)}, 2),
			groupRes("room", 3, "Bedroom", []map[string]any{dev(4)}, 3),
		},
		"zone": {
			groupRes("zone", 1, "Downstairs", []map[string]any{{"rid": uid("light", 1), "rtype": "light"},
				{"rid": uid("light", 3), "rtype": "light"}}, 4),
		},
		"grouped_light": {
			grouped(1, "room", uid("room", 1), true, 65),
			grouped(2, "room", uid("room", 2), false, 100),
			grouped(3, "room", uid("room", 3), false, 30),
			grouped(4, "zone", uid("zone", 1), true, 80),
			grouped(5, "bridge_home", uid("home", 1), true, 70),
		},
		"zigbee_connectivity": {
			zigbee(1, "connected"), zigbee(2, "connected"), zigbee(3, "connected"), zigbee(4, "connectivity_issue"), zigbee(5, "connected"),
		},
		"scene": {
			scene(1, "Relax", "room", uid("room", 1), "inactive"),
			scene(2, "Read", "room", uid("room", 1), "static"),
			scene(3, "Relax", "room", uid("room", 3), "inactive"),
			scene(4, "Energize", "room", uid("room", 2), "inactive"),
		},
	}
	return b
}

func groupRes(kind string, n int, name string, children []map[string]any, grouped int) map[string]any {
	return map[string]any{"id": uid(kind, n), "type": kind, "children": children,
		"services": []map[string]any{{"rid": uid("grouped", grouped), "rtype": "grouped_light"}},
		"metadata": map[string]any{"name": name, "archetype": "living_room"}}
}

func grouped(n int, ownerType, owner string, on bool, bri float64) map[string]any {
	return map[string]any{"id": uid("grouped", n), "type": "grouped_light", "owner": map[string]any{"rid": owner, "rtype": ownerType},
		"on": map[string]any{"on": on}, "dimming": map[string]any{"brightness": bri}}
}

func zigbee(n int, status string) map[string]any {
	return map[string]any{"id": uid("zigbee", n), "type": "zigbee_connectivity",
		"owner": map[string]any{"rid": uid("device", n), "rtype": "device"}, "status": status, "mac_address": "00:17:88:01:00:00:00:0" + fmt.Sprint(n)}
}

func scene(n int, name, groupType, groupID, active string) map[string]any {
	return map[string]any{"id": uid("scene", n), "type": "scene", "metadata": map[string]any{"name": name},
		"group": map[string]any{"rid": groupID, "rtype": groupType}, "actions": []any{}, "speed": 0.5, "auto_dynamic": false,
		"status": map[string]any{"active": active}}
}

// fakeHome is Rubi Home with bridges on its network.
type fakeHome struct {
	mu      sync.Mutex
	devices []rubiplugin.HomeDevice
	bridges map[string]*fakeBridge // by device id
	extra   []map[string]string    // more bridges that discovery finds (not paired)
	offline bool
	pressed bool
	calls   []string // "device op"
	pairIP  string
}

var resourcePath = regexp.MustCompile(`^/clip/v2/resource(/[a-z_]+(/[0-9a-f-]{36})?)?$`)

func (h *fakeHome) handle(device, op string, args json.RawMessage) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.offline {
		return nil, &rubiplugin.Error{Code: rubiplugin.CodeHomeOffline, Message: "Mac mini: Rubi Home didn't answer; is the computer at home on and online?"}
	}
	var dev *rubiplugin.HomeDevice
	for i, d := range h.devices {
		if device == "" && dev == nil || d.ID == device || strings.EqualFold(d.Name, device) {
			dev = &h.devices[i]
		}
	}
	if dev == nil {
		if len(h.devices) == 0 {
			return nil, &rubiplugin.Error{Code: rubiplugin.CodeHomeNotPaired, Message: "no Rubi Home is paired: the user installs Rubi Home on a computer at home and pairs it in the Rubi panel (Settings > Rubi Home)"}
		}
		return nil, &rubiplugin.Error{Code: rubiplugin.CodeHomeNotPaired, Message: fmt.Sprintf("no Rubi Home called %q; paired: Mac mini", device)}
	}
	h.calls = append(h.calls, dev.ID+" "+op)
	b := h.bridges[dev.ID]
	switch op {
	case "hue.discover":
		list := []map[string]string{}
		if b != nil {
			list = append(list, map[string]string{"id": b.id, "ip": b.ip})
		}
		list = append(list, h.extra...)
		return map[string]any{"bridges": list}, nil
	case "hue.pair":
		var in struct {
			IP string `json:"ip"`
		}
		_ = json.Unmarshal(args, &in)
		h.pairIP = in.IP
		if b == nil || in.IP != b.ip {
			return nil, fmt.Errorf("no Hue Bridge at %s: dial tcp: connection refused", in.IP)
		}
		if !h.pressed {
			return nil, errors.New("press the round link button on the Hue Bridge, then try again within 30 seconds")
		}
		return map[string]any{"bridge": b.id, "ip": b.ip, "key": b.key}, nil
	case "hue.request":
		var in hueRequest
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, errors.New("bad arguments")
		}
		if b == nil || in.Bridge != b.id {
			return nil, errors.New("this Hue Bridge wasn't paired through this Rubi Home; set Hue up again")
		}
		if !resourcePath.MatchString(in.Path) || in.Method != "GET" && in.Method != "PUT" {
			return nil, errors.New("only the bridge's resource API (/clip/v2/resource/…) is allowed")
		}
		// Like Rubi Home: at the old address nobody answers, so it finds the bridge again by id.
		ip := b.ip
		if in.Key != b.key {
			return map[string]any{"status": 403, "body": map[string]any{"errors": []any{map[string]any{"description": "unauthorized user"}}, "data": []any{}}, "ip": ip}, nil
		}
		if b.status != 0 {
			return map[string]any{"status": b.status, "body": map[string]any{"errors": []any{map[string]any{"description": "busy"}}, "data": []any{}}, "ip": ip}, nil
		}
		parts := strings.Split(strings.TrimPrefix(in.Path, "/clip/v2/resource/"), "/")
		if in.Method == "GET" {
			b.gets[parts[0]]++
			data := b.res[parts[0]]
			if data == nil {
				data = []map[string]any{}
			}
			return map[string]any{"status": 200, "body": map[string]any{"errors": []any{}, "data": data}, "ip": ip}, nil
		}
		if len(parts) != 2 {
			return map[string]any{"status": 405, "body": map[string]any{"errors": []any{map[string]any{"description": "method not allowed"}}}, "ip": ip}, nil
		}
		var body map[string]any
		_ = json.Unmarshal(in.Body, &body)
		b.puts = append(b.puts, put{Path: in.Path, Body: body, IP: in.IP})
		found := false
		for _, r := range b.res[parts[0]] {
			found = found || r["id"] == parts[1]
		}
		if !found {
			return map[string]any{"status": 404, "body": map[string]any{"errors": []any{map[string]any{"description": "Not Found"}}, "data": []any{}}, "ip": ip}, nil
		}
		return map[string]any{"status": 200, "body": map[string]any{"errors": []any{},
			"data": []any{map[string]any{"rid": parts[1], "rtype": parts[0]}}}, "ip": ip}, nil
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", op)
}

func (h *fakeHome) takePuts(device string) []put {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.bridges[device]
	p := b.puts
	b.puts = nil
	return p
}

func (h *fakeHome) gets(device, rtype string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bridges[device].gets[rtype]
}

// HomeCall goes through JSON both ways, like the real one.
func (h *fakeHome) HomeCall(_ context.Context, device, op string, args, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := h.handle(device, op, raw)
	if err != nil {
		// Like Rubi: Rubi Home's own errors carry a code; what the helper refused is CodeHomeFailed.
		var e *rubiplugin.Error
		if errors.As(err, &e) {
			return e
		}
		return &rubiplugin.Error{Code: rubiplugin.CodeHomeFailed, Message: "Mac mini: " + err.Error()}
	}
	b, _ := json.Marshal(res)
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (h *fakeHome) HomeDevices() ([]rubiplugin.HomeDevice, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]rubiplugin.HomeDevice{}, h.devices...), nil
}

// fakeBase stands in for Rubi with connected bridges.
type fakeBase struct {
	*fakeHome
	mu       sync.Mutex
	accounts []rubiplugin.AccountInfo
	settings map[string]settings
	secrets  map[string]string // account → app key
	state    json.RawMessage
	levels   map[string]rubiplugin.Level
	pending  []rubiplugin.Request
	audits   []string
}

// oneBridge is a fake Rubi with one bridge, set up through the Rubi Home "dev-1".
func oneBridge() *fakeBase {
	h := &fakeHome{devices: []rubiplugin.HomeDevice{{ID: "dev-1", Name: "Mac mini"}},
		bridges: map[string]*fakeBridge{"dev-1": standardBridge()}}
	s := settings{Device: "dev-1", DeviceName: "Mac mini", Bridge: bridgeID, IP: bridgeIP}
	return &fakeBase{fakeHome: h, accounts: []rubiplugin.AccountInfo{{ID: bridgeID, Label: "Hue Bridge (3 rooms)", Default: true}},
		settings: map[string]settings{bridgeID: s}, secrets: map[string]string{bridgeID: appKey}}
}

func (b *fakeBase) ref(account string) string {
	if account == "" {
		for _, a := range b.accounts {
			if a.Default {
				return a.ID
			}
		}
	}
	return account
}

func (b *fakeBase) SettingsFor(account string, v any) error {
	s, ok := b.settings[b.ref(account)]
	if !ok {
		return errors.New("unknown account")
	}
	raw, _ := json.Marshal(s)
	return json.Unmarshal(raw, v)
}

func (b *fakeBase) SecretFor(account, key string) (string, error) {
	if key != "app_key" {
		return "", errors.New("undeclared secret")
	}
	return b.secrets[b.ref(account)], nil
}

func (b *fakeBase) Accounts() ([]rubiplugin.AccountInfo, error) { return b.accounts, nil }

func (b *fakeBase) LoadState(v any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == nil {
		return nil
	}
	return json.Unmarshal(b.state, v)
}

func (b *fakeBase) SaveState(v any) error {
	raw, err := json.Marshal(v)
	b.mu.Lock()
	b.state = raw
	b.mu.Unlock()
	return err
}

func (b *fakeBase) Level(kind string) rubiplugin.Level {
	if l, ok := b.levels[kind]; ok {
		return l
	}
	return rubiplugin.None
}

func (b *fakeBase) Submit(_ context.Context, r rubiplugin.Request) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, r)
	return map[string]any{"status": "awaiting_approval", "approval_id": fmt.Sprint(len(b.pending))}, nil
}

func (b *fakeBase) Audit(event string, _ map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.audits = append(b.audits, event)
}

func (b *fakeBase) Logf(string, ...any) {}

// harness is the plugin with its tools registered, as Rubi would run it (without group spacing).
func harness() *integration {
	x := newIntegration()
	x.space = newSpacer(0)
	newPlugin(x)
	return x
}

func call(t *testing.T, x *integration, b base, name string, args map[string]any) map[string]any {
	t.Helper()
	out, err := callErr(x, b, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

func callErr(x *integration, b base, name string, args map[string]any) (map[string]any, error) {
	raw, _ := json.Marshal(args)
	ref, _ := args["account"].(string)
	res, err := x.call(context.Background(), b, name, ref, raw)
	if err != nil {
		return nil, err
	}
	return roundTrip(res), nil
}

func roundTrip(v any) map[string]any {
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// get walks a decoded JSON value by keys and indexes, e.g. get(m, "rooms", 0, "name").
func get(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			l, _ := v.([]any)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}
