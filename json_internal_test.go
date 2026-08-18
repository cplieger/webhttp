package webhttp

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// TestValidErrorCode pins ErrorCode's grammar directly, both halves. The empty
// code is VALID (it means "omit the field", which an app with a bare
// {"error":…} taxonomy relies on), and everything outside [a-z0-9_] is not.
func TestValidErrorCode(t *testing.T) {
	cases := []struct {
		code ErrorCode
		want bool
	}{
		{"", true}, // omit the field
		{"host_not_allowed", true},
		{"rate_limited", true},
		{"http2_upgrade", true},
		{"_leading_underscore", true},
		{"9lives", true},
		{"host not allowed", false},  // a sentence in the code slot
		{"HOST_NOT_ALLOWED", false},  // uppercase
		{"host-not-allowed", false},  // hyphen
		{"host.not.allowed", false},  // dot
		{"host_not_allowed:", false}, // punctuation
		{"host\tnot", false},         // tab
		{"host\nnot", false},         // newline
		{"héllo", false},             // non-ASCII
		{InvalidErrorCode, true},     // the substitute must itself be well-formed
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			if got := validErrorCode(tc.code); got != tc.want {
				t.Errorf("validErrorCode(%q) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// TestCheckedErrorCode covers the substitution and its diagnostic: a well-formed
// code passes through, a malformed one becomes InvalidErrorCode rather than
// being repaired into something no client contract contains, and the offender is
// reported once per process — not per request, which is where this runs.
//
// The package-level warnOnce is reset here so the assertion holds whichever
// tests ran before this one, and restored afterwards.
func TestCheckedErrorCode(t *testing.T) {
	logCap := &captureMessages{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(logCap))
	malformedCodeWarn = warnOnce{}
	t.Cleanup(func() {
		slog.SetDefault(prevLogger)
		malformedCodeWarn = warnOnce{}
	})

	if got := checkedErrorCode("rate_limited"); got != "rate_limited" {
		t.Errorf("checkedErrorCode(rate_limited) = %q, want it unchanged", got)
	}
	if got := checkedErrorCode(""); got != "" {
		t.Errorf("checkedErrorCode(empty) = %q, want empty (the field is omitted)", got)
	}
	if got := logCap.snapshot(); len(got) != 0 {
		t.Errorf("well-formed codes logged %d records, want none", len(got))
	}

	if got := checkedErrorCode("rate limit exceeded"); got != InvalidErrorCode {
		t.Errorf("checkedErrorCode(a sentence) = %q, want %q", got, InvalidErrorCode)
	}
	recs := logCap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d records for one malformed code, want 1", len(recs))
	}
	if recs[0].Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", recs[0].Level)
	}
	attrs := map[string]any{}
	recs[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	if attrs["code"] != "rate limit exceeded" {
		t.Errorf("code attr = %v, want the offending code verbatim (that is what makes it findable)", attrs["code"])
	}
	if attrs["replacement"] != string(InvalidErrorCode) {
		t.Errorf("replacement attr = %v, want %q", attrs["replacement"], InvalidErrorCode)
	}

	if got := checkedErrorCode("another bad one"); got != InvalidErrorCode {
		t.Errorf("second malformed code = %q, want %q", got, InvalidErrorCode)
	}
	if got := logCap.snapshot(); len(got) != 1 {
		t.Errorf("got %d records after a second malformed code, want still 1 (bounded per process)", len(got))
	}
}

// TestWarnOnce pins that the bound is per INSTANCE, so one silenced diagnostic
// cannot silence an unrelated one.
func TestWarnOnce(t *testing.T) {
	logCap := &captureMessages{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(logCap))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	var w warnOnce
	w.warn("first")
	w.warn("second")

	var other warnOnce
	other.warn("other instance")

	var msgs []string
	for _, r := range logCap.snapshot() {
		msgs = append(msgs, r.Message)
	}
	if len(msgs) != 2 || msgs[0] != "first" || msgs[1] != "other instance" {
		t.Errorf("messages = %v, want [first, other instance]", msgs)
	}
}

// captureMessages is a minimal concurrency-safe slog.Handler recording every
// record. The external test package has its own richer captureHandler; this one
// serves the internal package.
type captureMessages struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureMessages) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureMessages) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureMessages) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureMessages) WithGroup(string) slog.Handler      { return h }

func (h *captureMessages) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}
