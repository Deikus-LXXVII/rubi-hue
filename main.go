// Command hue is the Philips Hue plugin for Rubi: see the lights, rooms, zones and scenes of a Hue Bridge,
// switch and dim them, change their color, and recall scenes.
//
// Rubi runs on the agent's machine and can't reach the user's home network, so the plugin never talks to
// the bridge itself: every request goes through Rubi Home, the helper on a computer at home (manifest
// Home: "hue"). The plugin has no egress of its own. Several bridges can be connected, one account each.
//
// Lights are low-risk, so nothing needs approval by default; the user can raise the levels.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// PublisherKey is the Rubi-Project plugin signing key (public half).
const PublisherKey = "MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="

const (
	kindRead    = "hue.read"
	kindControl = "hue.control"
)

func manifest() rubiplugin.Manifest {
	return rubiplugin.Manifest{
		ID:          "hue",
		Name:        "Philips Hue",
		Version:     "v1.0.0",
		Description: "See and control your Philips Hue lights: rooms, zones, brightness, colors and scenes. Works through Rubi Home on a computer at home.",
		Needs: "Rubi Home on a computer at home, paired with this Rubi first (in the Rubi panel: Settings > Rubi Home), " +
			"on the same network as your Hue Bridge. During setup you press the round link button on the bridge. " +
			"The plugin connects to nothing itself: it reaches the bridge only through Rubi Home.",
		Publisher: rubiplugin.Publisher{Name: "Rubi-Project", Key: PublisherKey, URL: "https://github.com/Deikus-LXXVII/rubi-hue"},
		Source:    "https://github.com/Deikus-LXXVII/rubi-hue",
		MinRubi:   "v0.6.0",
		Entry:     "hue",
		Secrets: []rubiplugin.Secret{{Key: "app_key", Label: "Hue application key", Internal: true,
			Help: "Given by the bridge when you press its link button during setup."}},
		Actions: []rubiplugin.Action{
			{Kind: kindRead, Title: "See your Hue lights, rooms, zones and scenes", DefaultLevel: rubiplugin.None},
			{Kind: kindControl, Title: "Switch, dim and color your Hue lights and recall scenes", DefaultLevel: rubiplugin.None},
		},
		Home: []string{"hue"},
	}
}

func main() { newPlugin(newIntegration()).Main() }

// integration is the plugin's working state.
type integration struct {
	mu    sync.Mutex // guards the plugin state (each bridge's last known address)
	tools map[string]toolFn
	cache *cache
	space *spacer
}

func newIntegration() *integration {
	return &integration{tools: map[string]toolFn{}, cache: newCache(30 * time.Second), space: newSpacer(time.Second)}
}

// toolFn runs a tool on one account. kind is the action whose approval level applies ("" = none).
type toolFn struct {
	kind     string
	describe func(args json.RawMessage) string // the approval summary
	run      func(ctx context.Context, a *account, args json.RawMessage) (any, error)
}

// toolCall is the approval payload of a tool run that waits for the user.
type toolCall struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type refer interface{ ref() string }

// addTool registers a tool that works on one account (one bridge), picked by its account argument.
func addTool[T refer](p *rubiplugin.Plugin, x *integration, kind, name, description string,
	describe func(in T) string, fn func(ctx context.Context, a *account, in T) (any, error)) {
	decode := func(raw json.RawMessage) (T, error) {
		var in T
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &in); err != nil {
				return in, fmt.Errorf("invalid arguments: %w", err)
			}
		}
		return in, nil
	}
	x.tools[name] = toolFn{kind: kind,
		describe: func(raw json.RawMessage) string {
			in, err := decode(raw)
			if err != nil || describe == nil {
				return summaries[kind]
			}
			return describe(in)
		},
		run: func(ctx context.Context, a *account, raw json.RawMessage) (any, error) {
			in, err := decode(raw)
			if err != nil {
				return nil, err
			}
			return fn(ctx, a, in)
		}}
	rubiplugin.AddTool(p, "hue_"+name, description, func(ctx context.Context, h *rubiplugin.Host, in T) (any, error) {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		return x.call(ctx, h, name, in.ref(), raw)
	})
}

var summaries = map[string]string{kindRead: "Look at your Hue lights", kindControl: "Change your Hue lights"}

// call runs a tool, or asks for approval first if the user raised its action's level.
func (x *integration) call(ctx context.Context, b base, name, ref string, args json.RawMessage) (any, error) {
	t, ok := x.tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %s", name)
	}
	a, err := x.accountFor(b, ref)
	if err != nil {
		return nil, err
	}
	if t.kind == "" || a.Level(t.kind) == rubiplugin.None {
		return t.run(ctx, a, args)
	}
	return a.Submit(ctx, rubiplugin.Request{Kind: t.kind, Summary: t.describe(args),
		Preview: map[string]any{"tool": "hue_" + name, "arguments": args},
		Options: []rubiplugin.Option{{Key: "allow", Label: "Allow"}}, Payload: toolCall{Tool: name, Args: args}})
}

// executeTool runs a tool run the user approved.
func (x *integration) executeTool(ctx context.Context, b base, kind string, payload json.RawMessage) (any, error) {
	return x.execute(ctx, b, payload, func(ctx context.Context, a *account, data json.RawMessage) (any, error) {
		var c toolCall
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, err
		}
		t, ok := x.tools[c.Tool]
		if !ok || t.kind != kind {
			return nil, fmt.Errorf("unexpected approved tool %q", c.Tool)
		}
		return t.run(ctx, a, c.Args)
	})
}

func newPlugin(x *integration) *rubiplugin.Plugin {
	p := rubiplugin.New(manifest())
	p.Validate = func(ctx context.Context, fields, secrets map[string]string) (any, string, error) {
		h := p.Host()
		if h == nil {
			return nil, "", fmt.Errorf("not connected to Rubi")
		}
		return x.validate(ctx, h, fields, secrets)
	}
	// Settings or accounts changed (e.g. a bridge was set up again): forget what was cached.
	p.OnConfigChanged = func(*rubiplugin.Host, string) { x.cache.clear() }
	p.Stop = x.cache.clear
	registerTools(p, x)
	for _, kind := range []string{kindRead, kindControl} {
		p.OnExecute(kind, func(ctx context.Context, h *rubiplugin.Host, _ string, payload json.RawMessage) (any, error) {
			return x.executeTool(ctx, h, kind, payload)
		})
	}
	rubiplugin.AddTool(p, "hue_accounts", "The connected Hue Bridges (one account each); the first is the default. Pass one as account to the other tools.",
		func(ctx context.Context, h *rubiplugin.Host, _ struct{}) (any, error) { return x.listAccounts(h) })
	return p
}

func (x *integration) listAccounts(b base) (any, error) {
	list, err := x.accounts(b)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, a := range list {
		var s settings
		if err := a.Settings(&s); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"account": a.label, "default": a.isDefault, "bridge": s.Bridge,
			"rubi_home": s.DeviceName})
	}
	return map[string]any{"accounts": out}, nil
}
