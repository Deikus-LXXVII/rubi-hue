package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// The bridge, through Rubi Home. Every request is a "hue.request" to the account's Rubi Home, which
// passes it to the bridge (CLIP v2 resource API only) and spaces requests 100 ms apart per bridge. The
// plugin sends one request at a time, keeps group commands (grouped_light) a second apart as Signify
// asks, and caches the room and light map for 30 seconds so name lookups don't cost a round trip each.

// hueRequest is the "hue.request" operation's argument.
type hueRequest struct {
	Bridge string          `json:"bridge"`
	IP     string          `json:"ip"`
	Key    string          `json:"key"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// hueResponse is its result.
type hueResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
	IP     string          `json:"ip"`
}

// clipBody is the envelope of every CLIP v2 answer.
type clipBody struct {
	Errors []struct {
		Description string `json:"description"`
	} `json:"errors"`
	Data json.RawMessage `json:"data"`
}

const resourceBase = "/clip/v2/resource/"

// request sends one request to the account's bridge and returns the answer's data, plus the bridge's
// complaints about parts of a change it couldn't make (e.g. a light that is unreachable).
func (a *account) request(ctx context.Context, method, path string, body any) (json.RawMessage, []string, error) {
	var s settings
	if err := a.Settings(&s); err != nil {
		return nil, nil, err
	}
	key, err := a.Secret("app_key")
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't read the bridge key: %w", err)
	}
	if key == "" || s.Bridge == "" {
		return nil, nil, errors.New("this Hue Bridge isn't fully set up; set Philips Hue up again in the Rubi panel")
	}
	st, err := a.loadState()
	if err != nil {
		return nil, nil, err
	}
	ip := s.IP
	if st.IP != "" {
		ip = st.IP
	}
	req := hueRequest{Bridge: s.Bridge, IP: ip, Key: key, Method: method, Path: path}
	if body != nil {
		if req.Body, err = json.Marshal(body); err != nil {
			return nil, nil, err
		}
	}
	if method == "PUT" && strings.HasPrefix(path, resourceBase+"grouped_light/") {
		if err := a.x.space.wait(ctx, s.Bridge); err != nil {
			return nil, nil, err
		}
	}
	var res hueResponse
	if err := a.b.HomeCall(ctx, s.Device, "hue.request", req, &res); err != nil {
		return nil, nil, homeErr(err)
	}
	if res.IP != "" && res.IP != ip && validIP(res.IP) {
		// The bridge moved (a new address from the router); Rubi Home found it again by its id.
		if err := a.saveState(bridgeState{IP: res.IP}); err != nil {
			a.Logf("couldn't save the bridge's new address: %v", err)
		} else {
			a.Logf("the bridge moved from %s to %s", ip, res.IP)
		}
	}
	var cb clipBody
	_ = json.Unmarshal(res.Body, &cb)
	var problems []string
	for _, e := range cb.Errors {
		if d := strings.TrimSpace(e.Description); d != "" {
			problems = append(problems, d)
		}
	}
	switch {
	case res.Status >= 200 && res.Status < 300:
		return cb.Data, problems, nil
	case res.Status == 401 || res.Status == 403:
		return nil, nil, errors.New("the Hue Bridge no longer accepts Rubi's key (the bridge may have been reset, or Rubi " +
			"removed in the Hue app). Set Philips Hue up again in the Rubi panel")
	case res.Status == 404:
		return nil, nil, errors.New("the bridge doesn't have that any more (it may have been deleted in the Hue app); try again")
	case res.Status == 429 || res.Status == 503:
		return nil, nil, errors.New("the Hue Bridge is busy; try again in a few seconds")
	}
	msg := fmt.Sprintf("the Hue Bridge answered %d", res.Status)
	if len(problems) > 0 {
		msg += ": " + strings.Join(problems, "; ")
	}
	return nil, nil, errors.New(msg)
}

// get lists one resource type.
func (a *account) get(ctx context.Context, rtype string, out any) error {
	data, _, err := a.request(ctx, "GET", resourceBase+rtype, nil)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		data = json.RawMessage("[]")
	}
	return json.Unmarshal(data, out)
}

// put changes one resource. The cached state is dropped, since it no longer holds.
func (a *account) put(ctx context.Context, rtype, id string, body any) ([]string, error) {
	_, problems, err := a.request(ctx, "PUT", resourceBase+rtype+"/"+id, body)
	a.x.cache.drop(a.id)
	return problems, err
}

// ---- cache and spacing ----

type cache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	at time.Time
	v  any
}

func newCache(ttl time.Duration) *cache {
	return &cache{ttl: ttl, now: time.Now, entries: map[string]cacheEntry{}}
}

func (c *cache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().Sub(e.at) > c.ttl {
		return nil, false
	}
	return e.v, true
}

func (c *cache) put(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{at: c.now(), v: v}
}

// drop forgets everything cached for one account.
func (c *cache) drop(account string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, account+"|") {
			delete(c.entries, k)
		}
	}
}

func (c *cache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]cacheEntry{}
}

// spacer keeps a gap between requests of one kind to one bridge.
type spacer struct {
	gap  time.Duration
	mu   sync.Mutex
	next map[string]time.Time
}

func newSpacer(gap time.Duration) *spacer { return &spacer{gap: gap, next: map[string]time.Time{}} }

func (s *spacer) wait(ctx context.Context, bridge string) error {
	s.mu.Lock()
	now := time.Now()
	at := s.next[bridge]
	if at.Before(now) {
		at = now
	}
	s.next[bridge] = at.Add(s.gap)
	s.mu.Unlock()
	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ---- the home: rooms, zones, lights ----

type ref struct {
	RID   string `json:"rid"`
	RType string `json:"rtype"`
}

type named struct {
	Name string `json:"name"`
}

type xy struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type rawGroup struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Children []ref  `json:"children"`
	Services []ref  `json:"services"`
	Metadata named  `json:"metadata"`
}

type rawLight struct {
	ID       string `json:"id"`
	Owner    ref    `json:"owner"`
	Metadata named  `json:"metadata"`
	On       struct {
		On bool `json:"on"`
	} `json:"on"`
	Dimming *struct {
		Brightness float64 `json:"brightness"`
	} `json:"dimming"`
	ColorTemperature *struct {
		Mirek       *int `json:"mirek"`
		MirekValid  bool `json:"mirek_valid"`
		MirekSchema struct {
			Min int `json:"mirek_minimum"`
			Max int `json:"mirek_maximum"`
		} `json:"mirek_schema"`
	} `json:"color_temperature"`
	Color *struct {
		XY xy `json:"xy"`
	} `json:"color"`
}

type rawGrouped struct {
	ID    string `json:"id"`
	Owner ref    `json:"owner"`
	On    *struct {
		On bool `json:"on"`
	} `json:"on"`
	Dimming *struct {
		Brightness float64 `json:"brightness"`
	} `json:"dimming"`
}

type rawConnectivity struct {
	Owner  ref    `json:"owner"`
	Status string `json:"status"`
}

type rawScene struct {
	ID       string `json:"id"`
	Metadata named  `json:"metadata"`
	Group    ref    `json:"group"`
	Status   *struct {
		Active string `json:"active"`
	} `json:"status"`
}

// model is one bridge's rooms, zones and lights.
type model struct {
	groups []*group // rooms, then zones, each by name
	lights []*light // by name
	byID   map[string]*light
	home   string // the grouped_light of all the bridge's lights
}

type group struct {
	ID, Kind, Name string
	Grouped        string // its grouped_light
	On             *bool
	Brightness     *float64
	Lights         []*light
}

type light struct {
	ID, Name, Device string
	Room             string // the room it is in, if any
	On               bool
	Brightness       *float64
	XY               *xy
	Mirek            *int // set when the light shows a white color temperature
	HasColor, HasCT  bool
	MirekMin         int
	MirekMax         int
	Reachable        *bool
}

// home returns the bridge's rooms, zones and lights, from the cache when it is fresh.
func (a *account) home(ctx context.Context) (*model, error) {
	if v, ok := a.x.cache.get(a.id + "|model"); ok {
		return v.(*model), nil
	}
	var rooms, zones []rawGroup
	var lights []rawLight
	var grouped []rawGrouped
	for _, r := range []struct {
		rtype string
		out   any
	}{{"room", &rooms}, {"zone", &zones}, {"light", &lights}, {"grouped_light", &grouped}} {
		if err := a.get(ctx, r.rtype, r.out); err != nil {
			return nil, err
		}
	}
	var conn []rawConnectivity
	if err := a.get(ctx, "zigbee_connectivity", &conn); err != nil {
		a.Logf("couldn't read whether lights are reachable: %v", err)
		conn = nil
	}
	m := buildModel(rooms, zones, lights, grouped, conn)
	a.x.cache.put(a.id+"|model", m)
	return m, nil
}

func buildModel(rooms, zones []rawGroup, lights []rawLight, grouped []rawGrouped, conn []rawConnectivity) *model {
	m := &model{byID: map[string]*light{}}
	byDevice := map[string][]*light{}
	reach := map[string]bool{}
	for _, c := range conn {
		if c.Owner.RID != "" {
			reach[c.Owner.RID] = c.Status == "connected"
		}
	}
	for _, rl := range lights {
		l := &light{ID: rl.ID, Name: strings.TrimSpace(rl.Metadata.Name), Device: rl.Owner.RID, On: rl.On.On}
		if l.Name == "" {
			l.Name = "Light " + shortID(rl.ID)
		}
		if rl.Dimming != nil {
			b := rl.Dimming.Brightness
			l.Brightness = &b
		}
		if rl.Color != nil {
			l.HasColor = true
			c := rl.Color.XY
			l.XY = &c
		}
		if ct := rl.ColorTemperature; ct != nil {
			l.HasCT = true
			l.MirekMin, l.MirekMax = ct.MirekSchema.Min, ct.MirekSchema.Max
			if ct.MirekValid && ct.Mirek != nil {
				v := *ct.Mirek
				l.Mirek = &v
			}
		}
		if r, ok := reach[l.Device]; ok {
			l.Reachable = &r
		}
		m.lights = append(m.lights, l)
		m.byID[l.ID] = l
		byDevice[l.Device] = append(byDevice[l.Device], l)
	}
	groupedByID := map[string]rawGrouped{}
	for _, g := range grouped {
		groupedByID[g.ID] = g
		if g.Owner.RType == "bridge_home" {
			m.home = g.ID
		}
	}
	add := func(list []rawGroup, kind string) {
		var gs []*group
		for _, rg := range list {
			g := &group{ID: rg.ID, Kind: kind, Name: strings.TrimSpace(rg.Metadata.Name)}
			seen := map[string]bool{}
			for _, c := range rg.Children {
				var ls []*light
				switch c.RType {
				case "device":
					ls = byDevice[c.RID]
				case "light":
					if l := m.byID[c.RID]; l != nil {
						ls = []*light{l}
					}
				}
				for _, l := range ls {
					if !seen[l.ID] {
						seen[l.ID] = true
						g.Lights = append(g.Lights, l)
						if kind == "room" && l.Room == "" {
							l.Room = g.Name
						}
					}
				}
			}
			for _, s := range rg.Services {
				if s.RType != "grouped_light" {
					continue
				}
				g.Grouped = s.RID
				if gl, ok := groupedByID[s.RID]; ok {
					if gl.On != nil {
						on := gl.On.On
						g.On = &on
					}
					if gl.Dimming != nil {
						b := gl.Dimming.Brightness
						g.Brightness = &b
					}
				}
			}
			sort.Slice(g.Lights, func(i, j int) bool { return lessFold(g.Lights[i].Name, g.Lights[j].Name) })
			gs = append(gs, g)
		}
		sort.Slice(gs, func(i, j int) bool { return lessFold(gs[i].Name, gs[j].Name) })
		m.groups = append(m.groups, gs...)
	}
	add(rooms, "room")
	add(zones, "zone")
	sort.Slice(m.lights, func(i, j int) bool { return lessFold(m.lights[i].Name, m.lights[j].Name) })
	return m
}

// scenes returns the bridge's scenes, from the cache when it is fresh.
func (a *account) scenes(ctx context.Context) ([]rawScene, error) {
	if v, ok := a.x.cache.get(a.id + "|scenes"); ok {
		return v.([]rawScene), nil
	}
	var list []rawScene
	if err := a.get(ctx, "scene", &list); err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return lessFold(list[i].Metadata.Name, list[j].Metadata.Name) })
	a.x.cache.put(a.id+"|scenes", list)
	return list, nil
}

func lessFold(a, b string) bool { return strings.ToLower(a) < strings.ToLower(b) }

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
