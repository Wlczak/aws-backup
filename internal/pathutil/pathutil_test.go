package pathutil

import "testing"

func TestHasPrefixPath(t *testing.T) {
	cases := []struct {
		full, prefix string
		want         bool
	}{
		{"photos/2024/a.jpg", "photos", true},
		{"photos/2024/a.jpg", "photos/", true},
		{"photos", "photos/", true},
		{"photos", "photos", true},
		{"photosphere.jpg", "photos", false},
		{"photosphere.jpg", "photos/", false},
		{"photos/2024/", "photos/2024/", true},
		{"photos/2024/a.jpg", "photos/2024/", true},
		{"a", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		if got := HasPrefixPath(c.full, c.prefix); got != c.want {
			t.Errorf("HasPrefixPath(%q,%q)=%v want %v", c.full, c.prefix, got, c.want)
		}
	}
}

func TestNormalizeS3ListPrefix(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"backups":   "backups/",
		"backups/":  "backups/",
		"a/b":       "a/b/",
		"a/b/":      "a/b/",
	}
	for in, want := range cases {
		if got := NormalizeS3ListPrefix(in); got != want {
			t.Errorf("NormalizeS3ListPrefix(%q)=%q want %q", in, got, want)
		}
	}
}
