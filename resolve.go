package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Name resolution. The agent names a room, zone, light or scene the way the user said it ("kitchen",
// "the living room lights", "bedroom lamp", "Readng"). Matching is case-insensitive and tries, in order:
// the exact name, the name without spaces and punctuation, a prefix, a part of the name, then a name one
// or two typos away. The first step that finds anything decides; several finds there are ambiguous,
// except that a room or zone wins over a light of the same name. An id always works.

// target is what hue_set acts on.
type target struct {
	Kind  string // "room", "zone", "light" or "all"
	Group *group
	Light *light
}

func (t target) name() string {
	switch {
	case t.Group != nil:
		return t.Group.Name
	case t.Light != nil:
		return t.Light.Name
	}
	return "all lights"
}

func (t target) describe() string {
	switch {
	case t.Group != nil:
		return t.Group.Name + " (" + t.Kind + ")"
	case t.Light != nil && t.Light.Room != "":
		return t.Light.Name + " (light in " + t.Light.Room + ")"
	case t.Light != nil:
		return t.Light.Name + " (light)"
	}
	return "all lights"
}

var allNames = map[string]bool{"all": true, "all lights": true, "everything": true, "everywhere": true, "home": true,
	"house": true, "the house": true, "whole house": true, "the whole house": true}

// resolve finds a room, zone or light by name or id.
func (m *model) resolve(q string) (target, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return target{}, fmt.Errorf("say which room, zone or light")
	}
	for _, g := range m.groups {
		if q == g.ID || q == g.Grouped {
			return target{Kind: g.Kind, Group: g}, nil
		}
	}
	if l := m.byID[q]; l != nil {
		return target{Kind: "light", Light: l}, nil
	}
	if allNames[norm(q)] && m.home != "" {
		return target{Kind: "all"}, nil
	}
	var cands []target
	for _, g := range m.groups {
		cands = append(cands, target{Kind: g.Kind, Group: g})
	}
	for _, l := range m.lights {
		cands = append(cands, target{Kind: "light", Light: l})
	}
	names := func(t target) []string {
		if t.Group != nil {
			return []string{t.Group.Name}
		}
		if t.Light.Room != "" {
			return []string{t.Light.Name, t.Light.Room + " " + t.Light.Name, t.Light.Room + " / " + t.Light.Name}
		}
		return []string{t.Light.Name}
	}
	found := best(q, cands, names)
	if len(found) > 1 {
		var groups []target
		for _, t := range found {
			if t.Group != nil {
				groups = append(groups, t)
			}
		}
		if len(groups) == 1 {
			found = groups
		}
	}
	switch len(found) {
	case 0:
		return target{}, fmt.Errorf("no room, zone or light called %q; see hue_lights for the names", q)
	case 1:
		return found[0], nil
	}
	var list []string
	for _, t := range found {
		list = append(list, t.describe())
	}
	sort.Strings(list)
	return target{}, fmt.Errorf("%q could be %s; say which (for a light, you can add its room, e.g. \"Kitchen Ceiling\")",
		q, strings.Join(list, ", "))
}

// findGroup finds a room or zone by name or id.
func (m *model) findGroup(q string) (*group, error) {
	for _, g := range m.groups {
		if q == g.ID || q == g.Grouped {
			return g, nil
		}
	}
	found := best(q, m.groups, func(g *group) []string { return []string{g.Name} })
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no room or zone called %q; see hue_lights for the names", q)
	case 1:
		return found[0], nil
	}
	var list []string
	for _, g := range found {
		list = append(list, g.Name+" ("+g.Kind+")")
	}
	sort.Strings(list)
	return nil, fmt.Errorf("%q could be %s; say which", q, strings.Join(list, ", "))
}

// best returns the items whose names match q at the best matching step.
func best[T any](q string, items []T, names func(T) []string) []T {
	query := norm(q)
	queries := []string{query}
	for _, cut := range []string{" lights", " light", " lamps", " lamp"} {
		if s, ok := strings.CutSuffix(query, cut); ok && s != "" {
			queries = append(queries, s)
		}
	}
	steps := []func(q, name string) bool{
		func(q, name string) bool { return q == name },
		func(q, name string) bool { return compact(q) == compact(name) },
		func(q, name string) bool { return strings.HasPrefix(name, q) },
		func(q, name string) bool { return strings.Contains(compact(name), compact(q)) },
		func(q, name string) bool {
			a, b := compact(q), compact(name)
			allowed := 1
			if len(a) > 5 {
				allowed = 2
			}
			return len(a) >= 3 && levenshtein(a, b) <= allowed
		},
	}
	// The whole query first at each step ("bedroom lamp" is a light), then without "lights" ("kitchen
	// lights" is the room).
	for _, step := range steps {
		for _, qq := range queries {
			var out []T
			for _, it := range items {
				hit := false
				for _, n := range names(it) {
					hit = hit || step(qq, norm(n))
				}
				if hit {
					out = append(out, it)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

// norm lowercases, turns punctuation into spaces, collapses spaces and drops a leading "the".
func norm(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimPrefix(s, "the ")
}

func compact(s string) string { return strings.ReplaceAll(s, " ", "") }

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
