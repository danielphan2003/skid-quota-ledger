package plugin_test

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Silthus/cliproxy-quota-ledger/internal/plugin"
)

type storedSample struct {
	At        int64   `json:"at"`
	Auth      string  `json:"auth"`
	Provider  string  `json:"provider"`
	Window    string  `json:"window"`
	Remaining float64 `json:"remaining"`
	ResetsAt  *int64  `json:"resets_at"`
}

// usage sends a usage record shaped like CPA's pluginapi.UsageRecord, which
// CPA marshals with Go's default field names.
func usage(t *testing.T, p *plugin.Plugin, authID, provider string, headers map[string][]string) {
	t.Helper()
	record := map[string]any{
		"RequestID":       "req-1",
		"Provider":        provider,
		"Model":           "model",
		"AuthID":          authID,
		"AuthType":        "oauth",
		"RequestedAt":     now.Add(-time.Second),
		"Failed":          false,
		"ResponseHeaders": headers,
	}
	if reply := call(t, p, "usage.handle", record); !reply.OK {
		t.Fatalf("usage.handle failed: %+v", reply.Error)
	}
}

func stored(t *testing.T, p *plugin.Plugin) map[string]storedSample {
	t.Helper()
	page := decode[struct{ Samples []storedSample }](t, manage(t, p, "GET", url.Values{}, ""))
	byWindow := make(map[string]storedSample, len(page.Samples))
	for _, sample := range page.Samples {
		byWindow[sample.Provider+"/"+sample.Window] = sample
	}
	return byWindow
}

func unix(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }

func TestUsageRecordsClaudeWindowsFromHeaders(t *testing.T) {
	p, _ := registeredPlugin(t)
	weekly := now.Add(50 * time.Hour)
	usage(t, p, "claude-a@example.com.json", "claude", map[string][]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":    {"0.42"},
		"Anthropic-Ratelimit-Unified-5h-Reset":          {unix(now.Add(2 * time.Hour))},
		"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0.1"},
		"Anthropic-Ratelimit-Unified-7d-Reset":          {unix(weekly)},
		"anthropic-ratelimit-unified-7d_oi-utilization": {"1.3"},
		"Content-Type": {"application/json"},
	})

	got := stored(t, p)
	if len(got) != 3 {
		t.Fatalf("stored %d windows, want 3: %+v", len(got), got)
	}
	week := got["claude/seven-day"]
	if week.Auth != "claude-a@example.com.json" || week.Remaining != 90 || week.ResetsAt == nil || *week.ResetsAt != weekly.Unix()*1000 {
		t.Fatalf("seven-day = %+v", week)
	}
	if week.At != now.UnixMilli() {
		t.Fatalf("at = %d, want now", week.At)
	}
	if session := got["claude/five-hour"]; session.Remaining != 58 {
		t.Fatalf("five-hour = %+v", session)
	}
	// Over the limit reads as empty, and a missing reset stays unknown.
	if fable := got["claude/seven-day-fable"]; fable.Remaining != 0 || fable.ResetsAt != nil {
		t.Fatalf("seven-day-fable = %+v", fable)
	}
}

func TestUsageRecordsCodexWindowsByLength(t *testing.T) {
	p, _ := registeredPlugin(t)
	usage(t, p, "codex-a.json", "codex", map[string][]string{
		"X-Codex-Primary-Used-Percent":          {"12.5"},
		"X-Codex-Primary-Window-Minutes":        {"300"},
		"X-Codex-Primary-Reset-After-Seconds":   {"3600"},
		"X-Codex-Secondary-Used-Percent":        {"40"},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
		"X-Codex-Secondary-Reset-At":            {unix(now.Add(100 * time.Hour))},
		"X-Codex-Additional-Spark-Used-Percent": {"99"},
	})

	got := stored(t, p)
	if len(got) != 2 {
		t.Fatalf("stored %d windows, want 2: %+v", len(got), got)
	}
	if session := got["codex/five-hour"]; session.Remaining != 87.5 || *session.ResetsAt != now.Add(time.Hour).UnixMilli() {
		t.Fatalf("five-hour = %+v", session)
	}
	if week := got["codex/weekly"]; week.Remaining != 60 || *week.ResetsAt != now.Add(100*time.Hour).Unix()*1000 {
		t.Fatalf("weekly = %+v", week)
	}
}

func TestUsageIgnoresRecordsWithoutQuotaHeaders(t *testing.T) {
	p, _ := registeredPlugin(t)
	usage(t, p, "", "claude", map[string][]string{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.5"}})
	usage(t, p, "gemini.json", "gemini", map[string][]string{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.5"}})
	usage(t, p, "claude.json", "claude", nil)
	usage(t, p, "claude.json", "claude", map[string][]string{"Anthropic-Ratelimit-Unified-7d-Utilization": {"NaN"}})
	usage(t, p, "codex.json", "codex", map[string][]string{
		"X-Codex-Primary-Used-Percent":   {"10"},
		"X-Codex-Primary-Window-Minutes": {"60"},
	})
	if got := stored(t, p); len(got) != 0 {
		t.Fatalf("stored %+v, want nothing", got)
	}
}

func TestUsageRepeatsAreDeduplicated(t *testing.T) {
	p, path := registeredPlugin(t)
	headers := map[string][]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.2"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(now.Add(time.Hour))},
	}
	usage(t, p, "claude.json", "claude", headers)
	usage(t, p, "claude.json", "claude", headers)
	if lines := len(readLines(t, path)); lines != 1 {
		t.Fatalf("ledger has %d lines, want 1", lines)
	}
}

func TestUsageWithoutStoreIsANoop(t *testing.T) {
	p := plugin.New(func() time.Time { return now })
	if reply := call(t, p, "usage.handle", map[string]any{"AuthID": "a", "Provider": "claude"}); !reply.OK {
		t.Fatalf("usage.handle failed: %+v", reply.Error)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.TrimSpace(string(raw)))
}
