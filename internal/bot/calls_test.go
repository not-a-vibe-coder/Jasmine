package bot

import (
	"context"
	"testing"

	"shipp/internal/memory"
)

func TestResolvePerson(t *testing.T) {
	store, _ := memory.NewHybridStore("", "")
	ctx := context.Background()
	_ = store.SaveMessage(ctx, -100, 6837916148, "skipp_dev", "user", "yo jasmine")
	_ = store.SaveMessage(ctx, -100, 8469388263, "jackdotsol", "user", "jasmine call skipp")
	b := &Bot{memory: store}

	for _, name := range []string{"skipp", "@Skipp", "skipp_dev", "SKIPP DEV"} {
		uname, id := b.resolvePerson(ctx, -100, name)
		if uname != "skipp_dev" || id != 6837916148 {
			t.Errorf("resolvePerson(%q) = %q, %d", name, uname, id)
		}
	}
	if uname, _ := b.resolvePerson(ctx, -100, "nobody"); uname != "" {
		t.Errorf("unknown name resolved to %q", uname)
	}
}
