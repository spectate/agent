package version

import "testing"

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"2.1.0", "2.0.9", 1},
		{"v2.0.10", "2.0.9", 1},
		{"2.0.0", "2.0.0", 0},
		{"2.0.0-dev", "2.0.0", -1},
		{"2.0.0-rc.2", "2.0.0-rc.10", -1},
		{"2.0.0-rc.1", "2.0.0-beta", 1},
		{"2.0.0-alpha", "2.0.0-alpha.1", -1},
		{"3.0.0", "2.99.99", 1},
		{"2.0.0+build.1", "2.0.0", 0},
		{"garbage", "2.0.0", -1},
		{"2.0", "2.0.0", -1},
		{"02.0.0", "1.0.0", -1},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := Compare(c.b, c.a); got != -c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}
