package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Colors. Hue lights take a color as a CIE xy point (color.xy) or a white as a color temperature in
// mirek (color_temperature.mirek, 1,000,000 / kelvin, 153 to 500). The plugin accepts color names, hex
// colors and kelvin values, and describes a light's color with a rough name and a hex value.

// colorSpec is a parsed color: an xy point or a mirek value.
type colorSpec struct {
	XY    *xy
	Mirek *int
}

// whites are named color temperatures, in kelvin.
var whites = map[string]int{
	"candle": 2000, "candlelight": 2000, "warm": 2700, "warm white": 2700, "soft white": 3000,
	"white": 4000, "neutral": 4000, "neutral white": 4000, "natural white": 4000,
	"cool": 5000, "cool white": 5000, "daylight": 6500, "cold white": 6500,
}

// named colors, as sRGB hex.
var colorNames = map[string]string{
	"red": "ff0000", "orange": "ff8000", "amber": "ffbf00", "yellow": "ffff00", "lime": "bfff00",
	"green": "00ff00", "mint": "00ff80", "teal": "008080", "cyan": "00ffff", "turquoise": "40e0d0",
	"sky blue": "87ceeb", "blue": "0000ff", "indigo": "4b0082", "purple": "8000ff", "violet": "8f00ff",
	"lavender": "b57edc", "magenta": "ff00ff", "pink": "ff69b4", "hot pink": "ff1493", "rose": "ff007f",
	"coral": "ff7f50", "salmon": "fa8072", "gold": "ffd700",
}

var (
	hexRe    = regexp.MustCompile(`^#?([0-9a-f]{6}|[0-9a-f]{3})$`)
	kelvinRe = regexp.MustCompile(`^(\d{4,5})\s*(k|kelvin)?$`)
)

// parseColor reads a color name ("warm white", "blue"), a hex color ("#ff8800") or a color temperature
// ("2700K", "2700 kelvin", or a bare number of 1000 to 10000).
func parseColor(s string) (colorSpec, error) {
	c := strings.ToLower(strings.Join(strings.Fields(s), " "))
	c = strings.TrimSuffix(strings.TrimSuffix(c, " light"), " color")
	if k, ok := whites[c]; ok {
		m := mirekOf(k)
		return colorSpec{Mirek: &m}, nil
	}
	if h, ok := colorNames[c]; ok {
		p := hexToXY(h)
		return colorSpec{XY: &p}, nil
	}
	if m := hexRe.FindStringSubmatch(c); m != nil {
		h := m[1]
		if len(h) == 3 {
			h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
		}
		if h == "000000" {
			return colorSpec{}, errors.New("black isn't a light color; turn the light off instead")
		}
		p := hexToXY(h)
		return colorSpec{XY: &p}, nil
	}
	if m := kelvinRe.FindStringSubmatch(c); m != nil {
		k, _ := strconv.Atoi(m[1])
		if k < 1000 || k > 10000 {
			return colorSpec{}, fmt.Errorf("%d K is out of range; Hue whites go from about 2000 K (warm) to 6500 K (cool)", k)
		}
		mi := mirekOf(k)
		return colorSpec{Mirek: &mi}, nil
	}
	return colorSpec{}, fmt.Errorf("unknown color %q: use a name (warm white, cool white, daylight, red, blue, purple, …), "+
		"a hex color like #ff8800, or a color temperature like 2700K", s)
}

// mirekOf converts kelvin to mirek, within what Hue accepts.
func mirekOf(kelvin int) int {
	m := int(math.Round(1e6 / float64(kelvin)))
	return max(153, min(500, m))
}

func kelvinOf(mirek int) int {
	k := 1e6 / float64(mirek)
	return int(math.Round(k/100) * 100)
}

// hexToXY converts an sRGB color to a CIE xy point (sRGB primaries, D65).
func hexToXY(h string) xy {
	v, _ := strconv.ParseUint(h, 16, 32)
	lin := func(c uint64) float64 {
		f := float64(c) / 255
		if f > 0.04045 {
			return math.Pow((f+0.055)/1.055, 2.4)
		}
		return f / 12.92
	}
	r, g, b := lin(v>>16&0xff), lin(v>>8&0xff), lin(v&0xff)
	X := r*0.4124 + g*0.3576 + b*0.1805
	Y := r*0.2126 + g*0.7152 + b*0.0722
	Z := r*0.0193 + g*0.1192 + b*0.9505
	sum := X + Y + Z
	if sum == 0 {
		return xy{X: 0.3127, Y: 0.3290}
	}
	return xy{X: round4(X / sum), Y: round4(Y / sum)}
}

func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }

// xyToRGB converts a CIE xy point to sRGB at full brightness (0..1 each).
func xyToRGB(p xy) (r, g, b float64) {
	if p.Y <= 0 {
		return 1, 1, 1
	}
	Y := 1.0
	X := Y / p.Y * p.X
	Z := Y / p.Y * (1 - p.X - p.Y)
	r = X*3.2406 - Y*1.5372 - Z*0.4986
	g = -X*0.9689 + Y*1.8758 + Z*0.0415
	b = X*0.0557 - Y*0.2040 + Z*1.0570
	r, g, b = max(r, 0), max(g, 0), max(b, 0)
	if m := max(r, g, b); m > 0 {
		r, g, b = r/m, g/m, b/m
	}
	gamma := func(c float64) float64 {
		if c <= 0.0031308 {
			return 12.92 * c
		}
		return 1.055*math.Pow(c, 1/2.4) - 0.055
	}
	return gamma(r), gamma(g), gamma(b)
}

// describeColor names a light's current color: a white with its temperature, or a rough color name with
// its hex value. It is "" when the light has no color.
func describeColor(l *light) string {
	if l.Mirek != nil {
		k := kelvinOf(*l.Mirek)
		return fmt.Sprintf("%s (%d K)", whiteName(k), k)
	}
	if l.XY == nil {
		return ""
	}
	r, g, b := xyToRGB(*l.XY)
	hex := fmt.Sprintf("#%02x%02x%02x", int(math.Round(r*255)), int(math.Round(g*255)), int(math.Round(b*255)))
	return roughName(r, g, b) + " " + hex
}

func whiteName(k int) string {
	switch {
	case k < 2400:
		return "candle"
	case k < 2900:
		return "warm white"
	case k < 3500:
		return "soft white"
	case k < 4500:
		return "neutral white"
	case k < 5800:
		return "cool white"
	}
	return "daylight"
}

// roughName names an RGB color (0..1) by its hue, or "white" when it is barely saturated.
func roughName(r, g, b float64) string {
	hi, lo := max(r, g, b), min(r, g, b)
	if hi == 0 {
		return "off"
	}
	sat := (hi - lo) / hi
	if sat < 0.18 {
		return "white"
	}
	var hue float64
	d := hi - lo
	switch hi {
	case r:
		hue = math.Mod((g-b)/d, 6)
	case g:
		hue = (b-r)/d + 2
	default:
		hue = (r-g)/d + 4
	}
	hue *= 60
	if hue < 0 {
		hue += 360
	}
	name := ""
	switch {
	case hue < 15 || hue >= 345:
		name = "red"
	case hue < 45:
		name = "orange"
	case hue < 70:
		name = "yellow"
	case hue < 160:
		name = "green"
	case hue < 200:
		name = "cyan"
	case hue < 255:
		name = "blue"
	case hue < 290:
		name = "purple"
	case hue < 330:
		name = "magenta"
	default:
		name = "pink"
	}
	if sat < 0.45 {
		if name == "orange" || name == "yellow" {
			return "warm white"
		}
		return "pale " + name
	}
	return name
}
