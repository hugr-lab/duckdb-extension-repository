package config

import (
	"strings"
	"testing"
	"time"
)

// Spec 0016: gc's defaults and bounds.
func TestGCConfig(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if g := cfg.GCSettings(); g.Interval != 0 || g.Grace != DefaultGCGrace {
		t.Fatalf("defaults: %+v", g)
	}
	for name, c := range map[string]struct{ file, want string }{
		"short grace":    {base + "gc: { grace: 1h }\n", "gc.grace"},
		"short interval": {base + "gc: { interval: 5m }\n", "gc.interval"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if cfg, err := Load(write(t, base+"gc: { interval: 24h, grace: 48h }\n"), nil); err != nil || cfg.GCSettings().Grace != 48*time.Hour {
		t.Fatalf("set: %v", err)
	}
	if _, err := Load(write(t, base), []string{"KISTA_GC__INTERVAL=1h"}); err == nil {
		t.Error("gc from the environment")
	}
}

// Spec 0045: attachments.feed_answer_max, a managed bound of the feed route's answer.
func TestAttachmentsFeedAnswerMax(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil || cfg.Attachments.FeedAnswerMax != 0 || cfg.Attachments.MaxSize != 0 {
		t.Fatalf("defaults (0: 1 MiB, and 4 MiB for a document): %+v %v", cfg.Attachments, err)
	}
	for _, v := range []string{"128KiB", "512KiB", "1MiB", "131072"} {
		if _, err := Load(write(t, base+"attachments: { feed_answer_max: "+v+" }\n"), nil); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	// a size is never written 0: leave the key out for the default
	if _, err := Load(write(t, base+"attachments: { feed_answer_max: 0 }\n"), nil); err == nil {
		t.Error("feed_answer_max: 0 loads")
	}
	for _, v := range []string{"64KiB", "131071", "2MiB", "1048577"} {
		if _, err := Load(write(t, base+"attachments: { feed_answer_max: "+v+" }\n"), nil); err == nil || !strings.Contains(err.Error(), "attachments.feed_answer_max") {
			t.Errorf("%s: %v", v, err)
		}
	}
	if cfg, err := Load(write(t, base+"attachments: { feed_answer_max: 256KiB }\n"), nil); err != nil || cfg.Attachments.FeedAnswerMax != 256<<10 {
		t.Fatalf("set: %+v %v", cfg.Attachments, err)
	}
}
