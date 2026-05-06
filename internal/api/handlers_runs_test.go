package api

import (
	"strings"
	"testing"
)

func TestSanitizeDecodeErr(t *testing.T) {
	t.Run("strips CR and LF", func(t *testing.T) {
		got := sanitizeDecodeErr("bad\r\ninjected: header")
		if strings.ContainsAny(got, "\r\n") {
			t.Fatalf("expected no CR/LF, got %q", got)
		}
		if got != "bad  injected: header" {
			t.Fatalf("unexpected output: %q", got)
		}
	})

	t.Run("caps length at 256", func(t *testing.T) {
		in := strings.Repeat("a", 1024)
		got := sanitizeDecodeErr(in)
		if len([]rune(got)) != 256 {
			t.Fatalf("expected 256 runes, got %d", len([]rune(got)))
		}
	})

	t.Run("short messages pass through unchanged", func(t *testing.T) {
		const in = "json: unknown field \"foo\""
		if got := sanitizeDecodeErr(in); got != in {
			t.Fatalf("expected %q, got %q", in, got)
		}
	})
}
