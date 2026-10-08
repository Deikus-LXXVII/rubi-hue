package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

type lightsIn struct {
	Room string `json:"room,omitempty" jsonschema:"only this room or zone (its name); default: all"`
	accountIn
}

type scenesIn struct {
	Room string `json:"room,omitempty" jsonschema:"only the scenes of this room or zone (its name); default: all"`
	accountIn
}

type setIn struct {
	Target     string   `json:"target" jsonschema:"a room, zone or light by name (as the user says it, e.g. \"kitchen\" or \"bedroom lamp\") or id; \"all\" for every light"`
	On         *bool    `json:"on,omitempty" jsonschema:"true to switch on, false to switch off"`
	Brightness *float64 `json:"brightness,omitempty" jsonschema:"brightness in percent, 1 to 100 (0 switches off); switches on if needed"`
	Color      string   `json:"color,omitempty" jsonschema:"a color name (warm white, soft white, neutral white, cool white, daylight, candle, red, orange, yellow, green, cyan, blue, purple, pink, …), a hex color like #ff8800, or a white color temperature like 2700K; switches on if needed"`
	Transition *float64 `json:"transition,omitempty" jsonschema:"seconds the change takes (fade), up to 3600; default: the bridge's (about 0.4)"`
	accountIn
}

type sceneIn struct {
	Name       string   `json:"name" jsonschema:"the scene's name, e.g. \"Relax\""`
	Room       string   `json:"room,omitempty" jsonschema:"the room or zone the scene belongs to, when several have a scene of that name"`
	Transition *float64 `json:"transition,omitempty" jsonschema:"seconds the change takes (fade), up to 3600"`
	accountIn
}

type allOffIn struct {
	Transition *float64 `json:"transition,omitempty" jsonschema:"seconds the lights take to fade out, up to 3600"`
	accountIn
}

func registerTools(p *rubiplugin.Plugin, x *integration) {
	addTool(p, x, kindRead, "lights", "The Hue rooms and zones with their lights: on, brightness (percent), color (a rough name with hex, or a white with its kelvin) and reachable:false for lights the bridge can't reach. Zones list their lights' names only.",
		nil, func(ctx context.Context, a *account, in lightsIn) (any, error) { return lights(ctx, a, in) })

	addTool(p, x, kindRead, "scenes", "The Hue scenes of each room and zone, and which one is active.",
		nil, func(ctx context.Context, a *account, in scenesIn) (any, error) { return scenes(ctx, a, in) })

	addTool(p, x, kindControl, "set", "Change a Hue room, zone or light: switch on or off, brightness, color or white temperature, with an optional fade. Rooms and zones change all their lights at once.",
		func(in setIn) string { return "Change Hue lights: " + describeSet(in) },
		func(ctx context.Context, a *account, in setIn) (any, error) { return set(ctx, a, in) })

	addTool(p, x, kindControl, "scene", "Recall a Hue scene by name (optionally of one room or zone).",
		func(in sceneIn) string {
			s := "Recall the Hue scene " + strings.TrimSpace(in.Name)
			if r := strings.TrimSpace(in.Room); r != "" {
				s += " in " + r
			}
			return s
		},
		func(ctx context.Context, a *account, in sceneIn) (any, error) { return recall(ctx, a, in) })

	addTool(p, x, kindControl, "all_off", "Switch off every Hue light of the bridge.",
		func(allOffIn) string { return "Switch off all Hue lights" },
		func(ctx context.Context, a *account, in allOffIn) (any, error) { return allOff(ctx, a, in) })
}

// ---- reading ----

func pct(b *float64) any {
	if b == nil {
		return nil
	}
	return int(math.Round(*b))
}

func lightInfo(l *light) map[string]any {
	out := map[string]any{"name": l.Name, "on": l.On}
	if l.On && l.Brightness != nil {
		out["brightness"] = pct(l.Brightness)
	}
	if c := describeColor(l); c != "" {
		out["color"] = c
	}
	if l.Reachable != nil && !*l.Reachable {
		out["reachable"] = false
	}
	return out
}

func groupInfo(g *group) map[string]any {
	out := map[string]any{"name": g.Name}
	if g.On != nil {
		out["on"] = *g.On
		if *g.On && g.Brightness != nil {
			out["brightness"] = pct(g.Brightness)
		}
	}
	return out
}

func lights(ctx context.Context, a *account, in lightsIn) (any, error) {
	m, err := a.home(ctx)
	if err != nil {
		return nil, err
	}
	only := (*group)(nil)
	if r := strings.TrimSpace(in.Room); r != "" {
		if only, err = m.findGroup(r); err != nil {
			return nil, err
		}
	}
	rooms, zones := []map[string]any{}, []map[string]any{}
	for _, g := range m.groups {
		if only != nil && g != only {
			continue
		}
		info := groupInfo(g)
		if g.Kind == "room" {
			ls := []map[string]any{}
			for _, l := range g.Lights {
				ls = append(ls, lightInfo(l))
			}
			info["lights"] = ls
			rooms = append(rooms, info)
		} else {
			names := []string{}
			for _, l := range g.Lights {
				names = append(names, l.Name)
			}
			info["lights"] = names
			zones = append(zones, info)
		}
	}
	out := map[string]any{"rooms": rooms}
	if len(zones) > 0 {
		out["zones"] = zones
	}
	if only == nil {
		other := []map[string]any{}
		for _, l := range m.lights {
			if l.Room == "" {
				other = append(other, lightInfo(l))
			}
		}
		if len(other) > 0 {
			out["lights_in_no_room"] = other
		}
	}
	return out, nil
}

func scenes(ctx context.Context, a *account, in scenesIn) (any, error) {
	m, err := a.home(ctx)
	if err != nil {
		return nil, err
	}
	list, err := a.scenes(ctx)
	if err != nil {
		return nil, err
	}
	only := (*group)(nil)
	if r := strings.TrimSpace(in.Room); r != "" {
		if only, err = m.findGroup(r); err != nil {
			return nil, err
		}
	}
	type entry struct {
		info   map[string]any
		scenes []string
	}
	var order []*entry
	byGroup := map[string]*entry{}
	for _, g := range m.groups {
		if only != nil && g != only {
			continue
		}
		e := &entry{info: map[string]any{"name": g.Name, "type": g.Kind}}
		byGroup[g.ID] = e
		order = append(order, e)
	}
	for _, s := range list {
		e := byGroup[s.Group.RID]
		if e == nil {
			continue
		}
		e.scenes = append(e.scenes, s.Metadata.Name)
		if s.Status != nil && s.Status.Active != "" && s.Status.Active != "inactive" {
			e.info["active"] = s.Metadata.Name
		}
	}
	out := []map[string]any{}
	for _, e := range order {
		if len(e.scenes) == 0 && only == nil {
			continue
		}
		e.info["scenes"] = append([]string{}, e.scenes...)
		out = append(out, e.info)
	}
	return map[string]any{"scenes": out}, nil
}

// ---- changing ----

func seconds(t *float64) (int, bool, error) {
	if t == nil {
		return 0, false, nil
	}
	if *t < 0 || *t > 3600 || math.IsNaN(*t) {
		return 0, false, errors.New("transition is in seconds, 0 to 3600")
	}
	return int(math.Round(*t * 1000)), true, nil
}

func describeSet(in setIn) string {
	var parts []string
	if in.On != nil {
		parts = append(parts, map[bool]string{true: "on", false: "off"}[*in.On])
	}
	if in.Brightness != nil {
		parts = append(parts, fmt.Sprintf("%v%%", *in.Brightness))
	}
	if c := strings.TrimSpace(in.Color); c != "" {
		parts = append(parts, c)
	}
	if in.Transition != nil {
		parts = append(parts, fmt.Sprintf("over %vs", *in.Transition))
	}
	s := strings.TrimSpace(in.Target)
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s
}

func set(ctx context.Context, a *account, in setIn) (any, error) {
	body := map[string]any{}
	applied := map[string]any{}
	on := in.On
	switchOn := func() {
		if on == nil {
			t := true
			on = &t
		}
	}
	if in.Brightness != nil {
		b := *in.Brightness
		if b < 0 || b > 100 || math.IsNaN(b) {
			return nil, errors.New("brightness is a percentage, 0 to 100")
		}
		if b == 0 {
			if on != nil && *on {
				return nil, errors.New("brightness 0 means off; give a brightness of 1 to 100 to switch on")
			}
			f := false
			on = &f
		} else {
			if on != nil && !*on {
				return nil, errors.New("can't switch off and set a brightness at once")
			}
			body["dimming"] = map[string]any{"brightness": math.Round(b*10) / 10}
			applied["brightness"] = b
			switchOn()
		}
	}
	var spec colorSpec
	if c := strings.TrimSpace(in.Color); c != "" {
		var err error
		if spec, err = parseColor(c); err != nil {
			return nil, err
		}
		if on != nil && !*on {
			return nil, errors.New("can't switch off and set a color at once")
		}
		applied["color"] = c
		switchOn()
	}
	ms, fade, err := seconds(in.Transition)
	if err != nil {
		return nil, err
	}
	if on == nil && len(body) == 0 {
		return nil, errors.New("say what to change: on, brightness, color or several")
	}

	m, err := a.home(ctx)
	if err != nil {
		return nil, err
	}
	t, err := m.resolve(in.Target)
	if err != nil {
		return nil, err
	}
	if t.Light != nil {
		l := t.Light
		if _, ok := body["dimming"]; ok && l.Brightness == nil {
			return nil, fmt.Errorf("%s can't be dimmed; it only switches on and off", l.Name)
		}
		if spec.XY != nil && !l.HasColor {
			if l.HasCT {
				return nil, fmt.Errorf("%s shows only whites; use a white like \"warm white\" or \"4000K\"", l.Name)
			}
			return nil, fmt.Errorf("%s can't change color", l.Name)
		}
		if spec.Mirek != nil && !l.HasCT {
			if !l.HasColor {
				return nil, fmt.Errorf("%s can't change color", l.Name)
			}
			// A color-only light: approximate the white with a color.
			p := whiteXY(*spec.Mirek)
			spec = colorSpec{XY: &p}
		}
		if spec.Mirek != nil && l.MirekMin > 0 && l.MirekMax >= l.MirekMin {
			v := max(l.MirekMin, min(l.MirekMax, *spec.Mirek))
			spec.Mirek = &v
		}
	}
	if spec.XY != nil {
		body["color"] = map[string]any{"xy": map[string]float64{"x": spec.XY.X, "y": spec.XY.Y}}
	}
	if spec.Mirek != nil {
		body["color_temperature"] = map[string]any{"mirek": *spec.Mirek}
	}
	if on != nil {
		body["on"] = map[string]any{"on": *on}
		applied["on"] = *on
	}
	if fade {
		body["dynamics"] = map[string]any{"duration": ms}
		applied["transition_seconds"] = *in.Transition
	}

	var problems []string
	switch {
	case t.Kind == "all":
		problems, err = a.put(ctx, "grouped_light", m.home, body)
	case t.Group != nil && t.Group.Grouped != "":
		problems, err = a.put(ctx, "grouped_light", t.Group.Grouped, body)
	case t.Group != nil: // no grouped light (rare): each light in turn
		if len(t.Group.Lights) == 0 {
			return nil, fmt.Errorf("%s has no lights", t.Group.Name)
		}
		for _, l := range t.Group.Lights {
			p, e := a.put(ctx, "light", l.ID, body)
			problems = append(problems, p...)
			if e != nil {
				err = e
				break
			}
		}
	default:
		problems, err = a.put(ctx, "light", t.Light.ID, body)
	}
	if err != nil {
		return nil, err
	}
	a.Audit("hue.set", map[string]any{"target": t.name(), "kind": t.Kind})
	out := map[string]any{"done": true, "target": t.describe(), "set": applied}
	if len(problems) > 0 {
		out["problems"] = problems
	}
	return out, nil
}

// whiteXY approximates a white of mirek on the black-body curve (for lights without color temperature).
func whiteXY(mirek int) xy {
	t := 1e6 / float64(mirek)
	var x float64
	if t <= 4000 {
		x = -0.2661239e9/(t*t*t) - 0.2343589e6/(t*t) + 0.8776956e3/t + 0.179910
	} else {
		x = -3.0258469e9/(t*t*t) + 2.1070379e6/(t*t) + 0.2226347e3/t + 0.240390
	}
	var y float64
	switch {
	case t <= 2222:
		y = -1.1063814*x*x*x - 1.34811020*x*x + 2.18555832*x - 0.20219683
	case t <= 4000:
		y = -0.9549476*x*x*x - 1.37418593*x*x + 2.09137015*x - 0.16748867
	default:
		y = 3.0817580*x*x*x - 5.87338670*x*x + 3.75112997*x - 0.37001483
	}
	return xy{X: round4(x), Y: round4(y)}
}

func recall(ctx context.Context, a *account, in sceneIn) (any, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, errors.New("say which scene")
	}
	ms, fade, err := seconds(in.Transition)
	if err != nil {
		return nil, err
	}
	m, err := a.home(ctx)
	if err != nil {
		return nil, err
	}
	list, err := a.scenes(ctx)
	if err != nil {
		return nil, err
	}
	groupName := map[string]string{}
	for _, g := range m.groups {
		groupName[g.ID] = g.Name
	}
	cands := list
	if r := strings.TrimSpace(in.Room); r != "" {
		g, err := m.findGroup(r)
		if err != nil {
			return nil, err
		}
		cands = nil
		for _, s := range list {
			if s.Group.RID == g.ID {
				cands = append(cands, s)
			}
		}
	}
	var found []rawScene
	for _, s := range cands {
		if s.ID == name {
			found = []rawScene{s}
		}
	}
	if found == nil {
		found = best(name, cands, func(s rawScene) []string {
			return []string{s.Metadata.Name, groupName[s.Group.RID] + " " + s.Metadata.Name}
		})
	}
	label := func(s rawScene) string {
		if g := groupName[s.Group.RID]; g != "" {
			return s.Metadata.Name + " (" + g + ")"
		}
		return s.Metadata.Name
	}
	switch len(found) {
	case 0:
		where := ""
		if in.Room != "" {
			where = " in " + strings.TrimSpace(in.Room)
		}
		return nil, fmt.Errorf("no scene called %q%s; see hue_scenes for the names", name, where)
	case 1:
	default:
		var names []string
		for _, s := range found {
			names = append(names, label(s))
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%q could be %s; give the room too", name, strings.Join(names, ", "))
	}
	s := found[0]
	rc := map[string]any{"action": "active"}
	if fade {
		rc["duration"] = ms
	}
	problems, err := a.put(ctx, "scene", s.ID, map[string]any{"recall": rc})
	if err != nil {
		return nil, err
	}
	a.x.cache.drop(a.id)
	a.Audit("hue.scene", map[string]any{"scene": s.Metadata.Name})
	out := map[string]any{"done": true, "scene": label(s)}
	if len(problems) > 0 {
		out["problems"] = problems
	}
	return out, nil
}

func allOff(ctx context.Context, a *account, in allOffIn) (any, error) {
	ms, fade, err := seconds(in.Transition)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"on": map[string]any{"on": false}}
	if fade {
		body["dynamics"] = map[string]any{"duration": ms}
	}
	m, err := a.home(ctx)
	if err != nil {
		return nil, err
	}
	var problems []string
	if m.home != "" {
		problems, err = a.put(ctx, "grouped_light", m.home, body)
	} else {
		// No home-wide group: each room that is on, then lights in no room.
		for _, g := range m.groups {
			if g.Kind != "room" || g.Grouped == "" || g.On != nil && !*g.On {
				continue
			}
			p, e := a.put(ctx, "grouped_light", g.Grouped, body)
			problems = append(problems, p...)
			if e != nil {
				err = e
				break
			}
		}
		for _, l := range m.lights {
			if err != nil {
				break
			}
			if l.Room == "" && l.On {
				p, e := a.put(ctx, "light", l.ID, body)
				problems, err = append(problems, p...), e
			}
		}
	}
	if err != nil {
		return nil, err
	}
	a.Audit("hue.all_off", nil)
	out := map[string]any{"done": true, "switched_off": "all lights"}
	if len(problems) > 0 {
		out["problems"] = problems
	}
	return out, nil
}
