package config

import (
	"testing"
	"time"
)

func TestCommunitySyncSettings(t *testing.T) {
	var c Config
	if s := c.CommunitySyncSettings(); s.Enabled || s.Interval != 24*time.Hour || s.URL != DefaultCommunitySyncURL {
		t.Fatalf("defaults: %+v", s)
	}
	for name, cs := range map[string]CommunitySync{
		"a short interval": {Interval: time.Minute},
		"a long interval":  {Interval: 8 * 24 * time.Hour},
		"plain http":       {URL: "http://mirror.example/x.tar.gz"},
		"credentials":      {URL: "https://u:p@mirror.example/x.tar.gz"},
		"no host":          {URL: "https:///x"},
	} {
		c := Config{Reserved: Reserved{CommunitySync: cs}}
		var errs []string
		validateReserved(func(f string, _ ...any) { errs = append(errs, f) }, c)
		if len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Config{Reserved: Reserved{CommunitySync: CommunitySync{Enabled: true, Interval: 6 * time.Hour, URL: "https://git.example/mirror.tar.gz"}}}
	validateReserved(func(f string, a ...any) { t.Errorf("refused: "+f, a...) }, ok)
}
