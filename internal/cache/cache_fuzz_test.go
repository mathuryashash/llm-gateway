package cache

import (
	"testing"
)

// FuzzCacheKey tests that the cache key function never panics and always
// returns a non-empty, deterministic 64-char hex string.
func FuzzCacheKey(f *testing.F) {
	f.Add("gpt-4", "user", "hello", 0.0, 0.0, 100)
	f.Add("", "", "", 0.0, 0.0, 0)
	f.Add("model-with-special-chars!@#", "system", "prompt\n\t\r", 1.0, 0.5, 2048)

	f.Fuzz(func(t *testing.T, model, role, content string, temp, topP float64, maxTokens int) {
		rk := RequestKey{
			Model:       model,
			Messages:    []msgKey{{Role: role, Content: content}},
			Temperature: temp,
			TopP:        topP,
			MaxTokens:   maxTokens,
		}
		k1 := Key(rk)
		k2 := Key(rk)

		if len(k1) == 0 {
			t.Fatal("key is empty")
		}
		if k1 != k2 {
			t.Fatal("key is not deterministic")
		}
		// Key should be prefixed.
		if len(k1) < len(keyPrefix)+64 {
			t.Fatalf("key too short: %s", k1)
		}
	})
}
