package subs

import (
	"reflect"
	"testing"
)

func TestParseGroupedFile(t *testing.T) {
	subs, err := ParseFile("../../testdata/subscriptions-grouped.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) < 20 {
		t.Fatalf("expected the grouped file's channels, got %d", len(subs))
	}
	var got *Subscription
	for i := range subs {
		if subs[i].Name == "Solder Hour" {
			got = &subs[i]
		}
	}
	if got == nil {
		t.Fatal("Solder Hour missing")
	}
	if got.URL != "https://www.youtube.com/@solderhour" || got.Presets[0] != "recent" || got.Values[0] != "Tech" || got.Values[1] != "TV-Y" {
		t.Errorf("Solder Hour parsed as %+v", *got)
	}
	if Handle(got.URL) != "@solderhour" {
		t.Errorf("handle %q", Handle(got.URL))
	}
}

func TestParseGenericShapes(t *testing.T) {
	subs, err := ParseFile("../../testdata/subscriptions-generic.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Subscription{
		"Plain Channel":       {URL: "https://www.youtube.com/@plain", Presets: []string{"Plex TV Show by Date"}, Values: []string{"Documentaries"}},
		"Two Values":          {URL: "https://www.youtube.com/@two", Presets: []string{"Plex TV Show by Date"}, Values: []string{"Music", "TV-G"}},
		"Direct Under Preset": {URL: "https://www.youtube.com/@direct", Presets: []string{"Plex TV Show by Date"}},
		"Mapping Form":        {URL: "https://www.youtube.com/@mapping", Presets: []string{"Plex TV Show by Date"}},
		"Overrides URL":       {URL: "https://www.youtube.com/@overrides", Presets: []string{"Plex TV Show by Date"}},
		"Nested Channel":      {URL: "https://www.youtube.com/@nested", Presets: []string{"Plex TV Show by Date", "Nested Preset", "Second Preset"}},
		"List Form":           {URL: "https://www.youtube.com/@list1", Presets: []string{"Plex TV Show by Date"}},
	}
	if len(subs) != len(want) {
		t.Fatalf("got %d subscriptions %v, want %d", len(subs), Names(subs), len(want))
	}
	for _, s := range subs {
		w, ok := want[s.Name]
		if !ok {
			t.Errorf("unexpected %q", s.Name)
			continue
		}
		if s.URL != w.URL || !reflect.DeepEqual(s.Presets, w.Presets) || !reflect.DeepEqual(s.Values, w.Values) {
			t.Errorf("%s: got %+v want %+v", s.Name, s, w)
		}
	}
}
