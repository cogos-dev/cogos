package selfupdate

import "testing"

func TestNormVersion(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"v0.16.4", "v0.16.4"},
		{"0.16.4", "v0.16.4"},
		{"  0.16.4 ", "v0.16.4"},
		{"dev", ""},
		{"DEV", ""},
		{"unknown", ""},
		{"", ""},
		{"not-a-version", ""},
		{"v0.17.0-rc1", "v0.17.0-rc1"},
	}
	for _, c := range cases {
		if got := normVersion(c.in); got != c.want {
			t.Errorf("normVersion(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestVersionAfter(t *testing.T) {
	cases := []struct {
		cand, cur string
		want      bool
	}{
		{"v0.16.5", "v0.16.4", true},
		{"0.16.5", "v0.16.4", true}, // v-prefix normalisation both ways
		{"v0.16.4", "v0.16.4", false},
		{"v0.16.3", "v0.16.4", false}, // downgrade is not "after"
		{"v0.17.0", "v0.17.0-rc1", true},
		{"v0.17.0-rc1", "v0.17.0", false},
		{"dev", "v0.16.4", false}, // dev never after
		{"v0.16.5", "dev", false}, // anything vs dev → false
		{"", "v0.16.4", false},
		{"v0.16.5", "", false},
	}
	for _, c := range cases {
		if got := versionAfter(c.cand, c.cur); got != c.want {
			t.Errorf("versionAfter(%q, %q) = %v; want %v", c.cand, c.cur, got, c.want)
		}
	}
}

func TestVersionEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.16.4", "0.16.4", true},
		{"v0.16.4", "v0.16.4", true},
		{"v0.16.4", "v0.16.5", false},
		{"dev", "dev", true},      // both normalise to ""
		{"dev", "unknown", true},  // both normalise to ""
		{"dev", "v0.16.4", false}, // "" vs real tag
		{"", "", true},
	}
	for _, c := range cases {
		if got := versionEqual(c.a, c.b); got != c.want {
			t.Errorf("versionEqual(%q, %q) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}
