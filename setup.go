package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Setup runs in steps (rubiplugin.NeedMore):
//
//  1. The first form asks nothing. Validate finds the Rubi Home computers; with several, it asks which
//     one is at the bridge's home (step "device").
//  2. Rubi Home looks for bridges on its network; with several, the user picks one (step "bridge").
//  3. The user presses the bridge's link button and Continue (step "link:…"). Rubi Home asks the bridge
//     for an application key; if the button wasn't pressed, the same step comes back.
//
// What a later step needs (the device, the bridge and its address) travels in the step itself, so setup
// survives a plugin restart in between. None of it is secret; the key only exists after the last step.

// settings is what Validate stores for an account.
type settings struct {
	Device     string `json:"device"`                // the Rubi Home computer (its id)
	DeviceName string `json:"device_name,omitempty"` // its name, for display
	Bridge     string `json:"bridge"`                // the bridge id (16 hex digits)
	IP         string `json:"ip"`                    // the bridge's address at setup
}

// RubiAccountID is the bridge id: setting the same bridge up again updates that account.
func (s settings) RubiAccountID() string { return s.Bridge }

// pending is a setup in progress, carried in the step.
type pending struct {
	Device     string `json:"d"`
	DeviceName string `json:"n,omitempty"`
	Bridge     string `json:"b"`
	IP         string `json:"i"`
}

const linkPrefix = "link:"

var bridgeIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (p pending) valid() bool {
	return p.Device != "" && bridgeIDRe.MatchString(p.Bridge) && validIP(p.IP)
}

func validIP(ip string) bool { return net.ParseIP(ip) != nil }

func (p pending) step() string {
	b, _ := json.Marshal(p)
	return linkPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func parseStep(step string) (pending, bool) {
	var p pending
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(step, linkPrefix))
	if err != nil || json.Unmarshal(raw, &p) != nil || !p.valid() {
		return p, false
	}
	return p, true
}

const pressMessage = "Press the round link button on top of your Hue Bridge, then press Continue."

func linkStep(p pending, message string) error {
	if message == "" {
		message = pressMessage
	}
	return &rubiplugin.NeedMore{Message: message, Step: p.step()}
}

func (x *integration) validate(ctx context.Context, h home, fields, secrets map[string]string) (any, string, error) {
	step := fields[rubiplugin.StepField]
	if strings.HasPrefix(step, linkPrefix) {
		p, ok := parseStep(step)
		if !ok {
			return nil, "", errors.New("this setup step is damaged; start the Philips Hue setup again")
		}
		return x.pair(ctx, h, p, secrets)
	}

	// Which Rubi Home.
	devices, err := h.HomeDevices()
	if err != nil {
		return nil, "", homeErr(err)
	}
	if len(devices) == 0 {
		return nil, "", errors.New("no Rubi Home is paired yet. Install Rubi Home on a computer at home (on the same network as " +
			"the Hue Bridge), pair it in the Rubi panel under Settings > Rubi Home, then set up Philips Hue again")
	}
	var dev rubiplugin.HomeDevice
	if want := strings.TrimSpace(fields["device"]); want != "" {
		for _, d := range devices {
			if d.ID == want {
				dev = d
			}
		}
		if dev.ID == "" {
			return nil, "", errors.New("that Rubi Home isn't paired anymore; start the setup again")
		}
	} else if len(devices) == 1 {
		dev = devices[0]
	} else {
		opts := make([]rubiplugin.Option, 0, len(devices))
		for _, d := range devices {
			opts = append(opts, rubiplugin.Option{Key: d.ID, Label: d.Name})
		}
		return nil, "", &rubiplugin.NeedMore{Message: "Which Rubi Home is on the same network as your Hue Bridge?",
			Fields: []rubiplugin.Field{{Key: "device", Label: "Rubi Home", Type: "choice", Required: true, Options: opts}},
			Step:   "device"}
	}
	p := pending{Device: dev.ID, DeviceName: dev.Name}

	// Which bridge.
	if choice := strings.TrimSpace(fields["bridge"]); choice != "" {
		id, ip, _ := strings.Cut(choice, "@")
		p.Bridge, p.IP = id, ip
		if !p.valid() {
			return nil, "", errors.New("pick one of the bridges in the list")
		}
		return nil, "", linkStep(p, "")
	}
	var found struct {
		Bridges []struct {
			ID string `json:"id"`
			IP string `json:"ip"`
		} `json:"bridges"`
	}
	if err := h.HomeCall(ctx, dev.ID, "hue.discover", map[string]any{}, &found); err != nil {
		return nil, "", homeErr(err)
	}
	var bridges []pending
	for _, b := range found.Bridges {
		c := pending{Device: dev.ID, DeviceName: dev.Name, Bridge: strings.ToLower(b.ID), IP: b.IP}
		if c.valid() {
			bridges = append(bridges, c)
		}
	}
	switch len(bridges) {
	case 0:
		return nil, "", fmt.Errorf("Rubi Home on %s found no Hue Bridge on its network. Check that the bridge is powered and "+
			"connected to the router (its lights are on), and that this computer is on the same network, then try again", dev.Name)
	case 1:
		return nil, "", linkStep(bridges[0], "")
	}
	opts := make([]rubiplugin.Option, 0, len(bridges))
	for _, b := range bridges {
		opts = append(opts, rubiplugin.Option{Key: b.Bridge + "@" + b.IP, Label: "Hue Bridge " + suffix(b.Bridge) + " at " + b.IP})
	}
	return nil, "", &rubiplugin.NeedMore{Message: "Rubi Home found several Hue Bridges. Which one do you want to connect? " +
		"(You can add the others later with Add account.)",
		Fields: []rubiplugin.Field{{Key: "bridge", Label: "Hue Bridge", Type: "choice", Required: true, Options: opts,
			Help: "The id is printed on the bottom of the bridge."}},
		Step: "bridge"}
}

// pair asks the bridge for an application key, through Rubi Home.
func (x *integration) pair(ctx context.Context, h home, p pending, secrets map[string]string) (any, string, error) {
	var res struct {
		Bridge string `json:"bridge"`
		IP     string `json:"ip"`
		Key    string `json:"key"`
	}
	if err := h.HomeCall(ctx, p.Device, "hue.pair", map[string]string{"ip": p.IP}, &res); err != nil {
		if strings.Contains(err.Error(), "link button") {
			return nil, "", linkStep(p, "The bridge says its link button wasn't pressed. Press the round link button on top of "+
				"your Hue Bridge (the big one in the middle), then press Continue within 30 seconds.")
		}
		return nil, "", homeErr(err)
	}
	res.Bridge = strings.ToLower(res.Bridge)
	if res.Key == "" || !bridgeIDRe.MatchString(res.Bridge) {
		return nil, "", errors.New("the bridge didn't give Rubi a key; start the setup again")
	}
	s := settings{Device: p.Device, DeviceName: p.DeviceName, Bridge: res.Bridge, IP: p.IP}
	if validIP(res.IP) {
		s.IP = res.IP
	}
	secrets["app_key"] = res.Key

	label := "Hue Bridge " + suffix(s.Bridge)
	var rooms struct {
		Status int `json:"status"`
		Body   struct {
			Data []json.RawMessage `json:"data"`
		} `json:"body"`
		IP string `json:"ip"`
	}
	err := h.HomeCall(ctx, p.Device, "hue.request", hueRequest{Bridge: s.Bridge, IP: s.IP, Key: res.Key, Method: "GET",
		Path: "/clip/v2/resource/room"}, &rooms)
	if err == nil && rooms.Status == 200 && len(rooms.Body.Data) > 0 {
		label = fmt.Sprintf("Hue Bridge (%s)", plural(len(rooms.Body.Data), "room"))
		if validIP(rooms.IP) {
			s.IP = rooms.IP
		}
	}
	return s, label, nil
}

// suffix is the last six digits of a bridge id, as the Hue app shows it.
func suffix(bridge string) string {
	if len(bridge) > 6 {
		return strings.ToUpper(bridge[len(bridge)-6:])
	}
	return strings.ToUpper(bridge)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// homeErr turns what Rubi says about Rubi Home (rubiplugin.CodeHome* errors) into something the user
// can act on. Other errors (e.g. "couldn't reach the Hue Bridge") pass through: they already say why.
func homeErr(err error) error {
	switch rubiplugin.ErrorCode(err) {
	case rubiplugin.CodeHomeOffline:
		return errors.New("Rubi Home didn't answer. Is the computer at home on, awake and online, with Rubi Home running?")
	case rubiplugin.CodeHomeNotPaired:
		if strings.Contains(err.Error(), "no Rubi Home is paired") {
			return errors.New("no Rubi Home is paired. Install Rubi Home on a computer at home and pair it in the Rubi panel " +
				"(Settings > Rubi Home)")
		}
		return errors.New("the Rubi Home this Hue Bridge was set up through isn't paired anymore (or no longer knows this " +
			"Rubi). Pair it again in the Rubi panel (Settings > Rubi Home), then set Philips Hue up again if needed")
	}
	return err
}
