package install

import (
	"encoding/json"
	"testing"
)

func TestMergeHooksIsIdempotentAndPreservesOthers(t *testing.T) {
	var settings map[string]any
	_ = json.Unmarshal([]byte(`{"model":"x","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"my-linter"}]}]}}`), &settings)
	s := MergeHooks(settings, "fleet")
	b, _ := json.Marshal(s)
	var again map[string]any
	_ = json.Unmarshal(b, &again)
	s2 := MergeHooks(again, "fleet")
	pre := s2["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("want user hook + one fleet hook, got %d: %v", len(pre), pre)
	}
	if s2["model"] != "x" {
		t.Fatal("unrelated settings lost")
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
