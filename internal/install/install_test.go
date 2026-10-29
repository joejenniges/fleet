package install

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMergeHooksIsIdempotentAndPreservesOthers(t *testing.T) {
	var settings map[string]any
	_ = json.Unmarshal([]byte(`{"model":"x","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"my-linter"}]}]}}`), &settings)
	s := MergeHooks(settings, "fleet", WakeAsync)
	b, _ := json.Marshal(s)
	var again map[string]any
	_ = json.Unmarshal(b, &again)
	s2 := MergeHooks(again, "fleet", WakeAsync)
	pre := s2["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("want user hook + one fleet hook, got %d: %v", len(pre), pre)
	}
	if s2["model"] != "x" {
		t.Fatal("unrelated settings lost")
	}
	stop := s2["hooks"].(map[string]any)["Stop"].([]any)
	h := stop[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if h["asyncRewake"] != true || !strings.Contains(h["command"].(string), "--wake async") {
		t.Fatalf("stop hook should be asyncRewake: %v", h)
	}
	// Claude Code enforces timeout on asyncRewake hooks (600 s default); the waiter
	// must be allowed to live as long as a wait can.
	if to, _ := h["timeout"].(int); to < 24*60*60 {
		t.Fatalf("async stop hook timeout must cover signals.max_wait: %v", h["timeout"])
	}
	blk := MergeHooks(map[string]any{}, "fleet", WakeBlock)["hooks"].(map[string]any)["Stop"].([]any)
	bh := blk[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if _, ok := bh["asyncRewake"]; ok || bh["timeout"] != 600 {
		t.Fatalf("block mode stop hook: %v", bh)
	}
	removed := RemoveHooks(s2)
	pre = removed["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 1 {
		t.Fatalf("remove should keep the user hook only: %v", pre)
	}
	if _, ok := removed["hooks"].(map[string]any)["SessionStart"]; ok {
		t.Fatal("empty event lists should be removed")
	}
}
