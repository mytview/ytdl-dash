// Package subs reads a ytdl-sub subscriptions file into a flat list of subscriptions. It follows ytdl-sub's
// rules loosely enough to survive the file shapes in the wild: top-level and nested keys are presets, a key
// starting with "=" carries subscription values ("= Tech | = TV-Y"), a key starting with "~" holds overrides,
// "__preset__" and "__value__" are ytdl-sub's own, a string leaf is a subscription (key = name, value = URL)
// and a mapping leaf with ytdl-sub subscription keys is one too.
package subs

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Subscription is one entry: its name, URL, the preset chain above it and the "=" values on the way down.
type Subscription struct {
	Name    string
	URL     string
	Presets []string // outermost first
	Values  []string // from "= a | = b" keys, outermost first
}

// ParseFile reads a subscriptions file.
func ParseFile(path string) ([]Subscription, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse reads subscriptions from YAML text.
func Parse(b []byte) ([]Subscription, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("subscriptions: %w", err)
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = *root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil
	}
	var out []Subscription
	walk(&root, nil, nil, &out)
	return out, nil
}

var subscriptionKeys = map[string]bool{"download": true, "overrides": true, "preset": true, "output_options": true, "ytdl_options": true, "url": true}

func walk(m *yaml.Node, presets, values []string, out *[]Subscription) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		key := strings.TrimSpace(k.Value)
		switch {
		case key == "__preset__" || key == "__value__" || strings.HasPrefix(key, "~"):
			continue // ytdl-sub's own sections and override maps: not subscriptions
		case strings.HasPrefix(key, "="):
			// "= Tech | = TV-Y": subscription values for everything below
			var vals []string
			for _, part := range strings.Split(key, "|") {
				vals = append(vals, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "=")))
			}
			if v.Kind == yaml.MappingNode {
				walk(v, presets, append(append([]string{}, values...), vals...), out)
			}
		case v.Kind == yaml.ScalarNode:
			if looksLikeURL(v.Value) {
				*out = append(*out, Subscription{Name: key, URL: v.Value, Presets: copyOf(presets), Values: copyOf(values)})
			}
		case v.Kind == yaml.SequenceNode:
			// a list of URLs under one name: ytdl-sub allows it; keep the first as the URL
			for _, item := range v.Content {
				if item.Kind == yaml.ScalarNode && looksLikeURL(item.Value) {
					*out = append(*out, Subscription{Name: key, URL: item.Value, Presets: copyOf(presets), Values: copyOf(values)})
					break
				}
			}
		case v.Kind == yaml.MappingNode:
			if isSubscriptionMapping(v) {
				*out = append(*out, Subscription{Name: key, URL: urlOf(v), Presets: copyOf(presets), Values: copyOf(values)})
				continue
			}
			// a preset (or several, "a | b") grouping what is below
			var ps []string
			for _, part := range strings.Split(key, "|") {
				ps = append(ps, strings.TrimSpace(part))
			}
			walk(v, append(append([]string{}, presets...), ps...), values, out)
		}
	}
}

func isSubscriptionMapping(m *yaml.Node) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if subscriptionKeys[m.Content[i].Value] {
			return true
		}
	}
	return false
}

func urlOf(m *yaml.Node) string {
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i].Value, m.Content[i+1]
		switch {
		case (k == "url" || k == "download") && v.Kind == yaml.ScalarNode && looksLikeURL(v.Value):
			return v.Value
		case k == "download" && v.Kind == yaml.MappingNode:
			if u := urlOf(v); u != "" {
				return u
			}
		case k == "overrides" && v.Kind == yaml.MappingNode:
			if u := urlOf(v); u != "" {
				return u
			}
		}
	}
	return ""
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func copyOf(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string{}, s...)
}

// Handle shortens a YouTube URL to what a person recognises: "@mkbhd", "c/TomBilyeu", "channel/UC...".
func Handle(u string) string {
	s := u
	for _, p := range []string{"https://www.youtube.com/", "https://youtube.com/", "http://www.youtube.com/", "http://youtube.com/"} {
		s = strings.TrimPrefix(s, p)
	}
	if i := strings.Index(s, "?"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, "/")
}

// Names returns the subscription names sorted, handy for tests and logs.
func Names(subs []Subscription) []string {
	var n []string
	for _, s := range subs {
		n = append(n, s.Name)
	}
	sort.Strings(n)
	return n
}
