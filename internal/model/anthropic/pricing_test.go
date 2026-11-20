package anthropic

import "testing"

func TestCostMicros(t *testing.T) {
	cases := []struct {
		model   string
		in, out int64
		want    int64
	}{
		{"claude-opus-5-5", 1_000_000, 0, 4_000_000},
		{"claude-opus-5-5", 0, 1_000_000, 20_000_000},
		{"claude-opus-5-5", 120, 40, 1280},
		{"claude-haiku-5-5", 1000, 1000, 600},
		{"claude-haiku-5-5", 200_000, 0, 100_000},
		{"claude-sonnet-5-5", 1, 0, 2},
		{"claude-some-future-model", 1000, 0, 10_000},
	}
	for _, c := range cases {
		if got := CostMicros(c.model, c.in, c.out); got != c.want {
			t.Errorf("CostMicros(%s, %d, %d) = %d, want %d", c.model, c.in, c.out, got, c.want)
		}
	}
}
