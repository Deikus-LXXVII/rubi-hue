package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Several accounts, one per Hue Bridge. Each connected account has its own bridge, application key and
// Rubi Home, its own part of the plugin state (the bridge's last known address) and its own cache. The
// plugin works on one account at a time through an *account, which narrows the host to it.

// home is the part of Rubi that reaches Rubi Home (all setup needs).
type home interface {
	HomeDevices() ([]rubiplugin.HomeDevice, error)
	HomeCall(ctx context.Context, device, op string, args, out any) error
}

// base is what the plugin needs from Rubi (*rubiplugin.Host; a fake in tests).
type base interface {
	home
	SettingsFor(account string, v any) error
	SecretFor(account, key string) (string, error)
	Accounts() ([]rubiplugin.AccountInfo, error)
	LoadState(v any) error
	SaveState(v any) error
	Level(kind string) rubiplugin.Level
	Submit(ctx context.Context, r rubiplugin.Request) (map[string]any, error)
	Audit(event string, fields map[string]any)
	Logf(format string, args ...any)
}

// accountIn is the optional account argument every tool takes.
type accountIn struct {
	Account string `json:"account,omitempty" jsonschema:"which connected Hue Bridge (its name, see hue_accounts); default: the default one"`
}

func (a accountIn) ref() string { return a.Account }

// account is the host narrowed to one connected bridge.
type account struct {
	b         base
	x         *integration
	id, label string
	isDefault bool
	several   bool // more than one bridge is connected: name it in approvals
}

// accounts lists the connected accounts, the default one first.
func (x *integration) accounts(b base) ([]*account, error) {
	list, err := b.Accounts()
	if err != nil {
		return nil, err
	}
	def := 0
	for i, a := range list {
		if a.Default {
			def = i
			break
		}
	}
	out := make([]*account, 0, len(list))
	for i, a := range list {
		acc := &account{b: b, x: x, id: a.ID, label: a.Label, isDefault: i == def, several: len(list) > 1}
		if acc.label == "" {
			acc.label = a.ID
		}
		if acc.isDefault {
			out = append([]*account{acc}, out...)
		} else {
			out = append(out, acc)
		}
	}
	return out, nil
}

// accountFor finds a connected account by name or id ("" = the default one).
func (x *integration) accountFor(b base, ref string) (*account, error) {
	list, err := x.accounts(b)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("no Hue Bridge is connected; the user sets one up in the Rubi panel")
	}
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return list[0], nil
	}
	var names []string
	for _, a := range list {
		if ref == strings.ToLower(a.id) || ref == strings.ToLower(a.label) {
			return a, nil
		}
		names = append(names, a.label)
	}
	return nil, fmt.Errorf("no connected Hue Bridge %q; connected: %s", ref, strings.Join(names, ", "))
}

func (a *account) Settings(v any) error               { return a.b.SettingsFor(a.id, v) }
func (a *account) Secret(key string) (string, error)  { return a.b.SecretFor(a.id, key) }
func (a *account) Level(kind string) rubiplugin.Level { return a.b.Level(kind) }
func (a *account) Logf(format string, args ...any)    { a.b.Logf("["+a.label+"] "+format, args...) }

func (a *account) Audit(event string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["account"] = a.id
	a.b.Audit(event, fields)
}

// scoped is an approval payload together with the account it belongs to.
type scoped struct {
	Account string          `json:"rubi_account"`
	Data    json.RawMessage `json:"data"`
}

// Submit asks for approval of an action on this account; the account goes with the payload, and the
// user sees which bridge it is when there are several.
func (a *account) Submit(ctx context.Context, r rubiplugin.Request) (map[string]any, error) {
	data, err := json.Marshal(r.Payload)
	if err != nil {
		return nil, err
	}
	r.Payload = scoped{Account: a.id, Data: data}
	if a.several {
		r.Summary += " (" + a.label + ")"
	}
	return a.b.Submit(ctx, r)
}

// execute runs an approved action on the account it was requested for.
func (x *integration) execute(ctx context.Context, b base, payload json.RawMessage,
	fn func(ctx context.Context, a *account, data json.RawMessage) (any, error)) (any, error) {
	var sc scoped
	if err := json.Unmarshal(payload, &sc); err != nil {
		return nil, err
	}
	a, err := x.accountFor(b, sc.Account)
	if err != nil {
		return nil, err
	}
	return fn(ctx, a, sc.Data)
}

// State. The plugin state holds each account's part under "accounts".

type allState struct {
	Accounts map[string]json.RawMessage `json:"accounts"`
}

// bridgeState is what the plugin remembers about one bridge.
type bridgeState struct {
	// IP is where Rubi Home last found the bridge, if it moved since setup.
	IP string `json:"ip,omitempty"`
}

func (a *account) loadAll() (allState, error) {
	all := allState{}
	if err := a.b.LoadState(&all); err != nil {
		return all, err
	}
	if all.Accounts == nil {
		all.Accounts = map[string]json.RawMessage{}
	}
	return all, nil
}

func (a *account) loadState() (bridgeState, error) {
	a.x.mu.Lock()
	defer a.x.mu.Unlock()
	var st bridgeState
	all, err := a.loadAll()
	if err != nil {
		return st, err
	}
	if s, ok := all.Accounts[a.id]; ok {
		err = json.Unmarshal(s, &st)
	}
	return st, err
}

func (a *account) saveState(st bridgeState) error {
	a.x.mu.Lock()
	defer a.x.mu.Unlock()
	all, err := a.loadAll()
	if err != nil {
		return err
	}
	if all.Accounts[a.id], err = json.Marshal(st); err != nil {
		return err
	}
	return a.b.SaveState(all)
}
