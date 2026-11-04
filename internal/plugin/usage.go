package plugin

import (
	"encoding/json"
	"math"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Silthus/cliproxy-quota-ledger/internal/ledger"
)

const (
	// A header sample is written only when its series moved by at least
	// headerMinMove points, its reset changed, or headerMaxQuiet passed since
	// the series' last header write. Every request carries the headers, so
	// without this a busy account would add a line per request and push older
	// history out of the newest-first page the panel reads.
	headerMinMove  = 0.5
	headerMaxQuiet = 5 * time.Minute
)

// usageRecord is the part of CPA's pluginapi.UsageRecord the ledger reads. CPA
// sends the record with Go's default field names.
type usageRecord struct {
	AuthID          string
	AuthType        string
	Provider        string
	Failed          bool
	ResponseHeaders map[string][]string
}

type seriesKey struct{ auth, provider, window string }

// headerThrottle remembers the last header sample written per series.
type headerThrottle struct {
	mu   sync.Mutex
	last map[seriesKey]ledger.Sample
}

// admit returns the samples worth writing and records them as written. A
// sample is worth writing when it is the series' first, its remaining moved
// by headerMinMove or more, its reset changed, or headerMaxQuiet has passed.
func (t *headerThrottle) admit(samples []ledger.Sample) []ledger.Sample {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = make(map[seriesKey]ledger.Sample)
	}
	admitted := samples[:0]
	for _, sample := range samples {
		key := seriesKey{sample.Auth, sample.Provider, sample.Window}
		previous, known := t.last[key]
		if known && !worthWriting(previous, sample) {
			continue
		}
		t.last[key] = sample
		admitted = append(admitted, sample)
	}
	return admitted
}

// forget drops what admit recorded for samples that were never stored, so the
// next request retries them.
func (t *headerThrottle) forget(samples []ledger.Sample) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, sample := range samples {
		key := seriesKey{sample.Auth, sample.Provider, sample.Window}
		if t.last[key].At == sample.At {
			delete(t.last, key)
		}
	}
}

func worthWriting(previous, next ledger.Sample) bool {
	quiet := time.Duration(next.At-previous.At) * time.Millisecond
	return quiet >= headerMaxQuiet || quiet < 0 ||
		math.Abs(next.Remaining-previous.Remaining) >= headerMinMove ||
		!resetsMatch(previous.ResetsAt, next.ResetsAt)
}

func resetsMatch(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return math.Abs(float64(*a-*b)) <= float64(ledger.ResetTolerance.Milliseconds())
}

// handleUsage records the quota windows that a completed upstream response
// reported in its rate-limit headers, so the history grows with every request
// instead of only while a dashboard is open.
func (p *Plugin) handleUsage(raw []byte) ([]byte, bool) {
	var record usageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return failure("invalid_request", err.Error())
	}
	store, throttle := p.currentRecorder()
	if store == nil {
		return success(struct{}{})
	}
	now := p.now()
	samples := samplesFromHeaders(record, now)
	valid := samples[:0]
	for _, sample := range samples {
		if sample.Validate(now) == nil {
			valid = append(valid, sample)
		}
	}
	admitted := throttle.admit(valid)
	if len(admitted) > 0 {
		if _, _, err := store.Append(admitted); err != nil {
			throttle.forget(admitted)
			return failure("store_failed", err.Error())
		}
	}
	return success(struct{}{})
}

// samplesFromHeaders maps provider rate-limit headers to ledger samples. The
// window ids match the management panel's own, so header samples and panel
// samples extend the same series.
func samplesFromHeaders(record usageRecord, now time.Time) []ledger.Sample {
	// CPA's AuthID is the auth file's path relative to the auth directory; the
	// panel keys its samples by the file name alone.
	auth := strings.TrimSpace(record.AuthID)
	if auth != "" {
		auth = path.Base(strings.ReplaceAll(auth, "\\", "/"))
	}
	if auth == "" || auth == "." || auth == "/" || len(record.ResponseHeaders) == 0 {
		return nil
	}
	// Quota windows belong to subscription (OAuth) logins. An API key reports
	// its own rate limits, which are not a quota to pool.
	switch strings.ToLower(strings.TrimSpace(record.AuthType)) {
	case "apikey", "api_key", "api-key":
		return nil
	}
	headers := lowerKeys(record.ResponseHeaders)
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	at := now.UnixMilli()
	sample := func(window string, remaining float64, resetsAt *int64) ledger.Sample {
		return ledger.Sample{At: at, Auth: auth, Provider: provider, Window: window, Remaining: remaining, ResetsAt: roundToMinute(resetsAt)}
	}

	var samples []ledger.Sample
	switch provider {
	case "claude":
		for _, window := range []struct{ header, id string }{
			{"5h", "five-hour"},
			{"7d", "seven-day"},
			{"7d_oi", "seven-day-fable"},
		} {
			prefix := "anthropic-ratelimit-unified-" + window.header + "-"
			utilization, ok := parseFloat(headers[prefix+"utilization"])
			if !ok {
				continue
			}
			samples = append(samples, sample(window.id, remainingPercent(utilization*100), resetMs(headers[prefix+"reset"])))
		}
	case "codex":
		for _, window := range []string{"primary", "secondary"} {
			prefix := "x-codex-" + window + "-"
			used, okUsed := parseFloat(headers[prefix+"used-percent"])
			minutes, okMinutes := parseFloat(headers[prefix+"window-minutes"])
			if !okUsed || !okMinutes {
				continue
			}
			var id string
			switch minutes {
			case 300:
				id = "five-hour"
			case 10080:
				id = "weekly"
			default:
				continue
			}
			resetsAt := resetMs(headers[prefix+"reset-at"])
			if resetsAt == nil {
				if after, ok := parseFloat(headers[prefix+"reset-after-seconds"]); ok && after >= 0 {
					ms := now.Add(time.Duration(after * float64(time.Second))).UnixMilli()
					resetsAt = &ms
				}
			}
			samples = append(samples, sample(id, remainingPercent(used), resetsAt))
		}
	}
	return samples
}

func lowerKeys(headers map[string][]string) map[string]string {
	out := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) > 0 {
			out[strings.ToLower(key)] = strings.TrimSpace(values[0])
		}
	}
	return out
}

func parseFloat(raw string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// remainingPercent turns a used percentage into the remaining one, clamped to
// 0..100: a window past its limit reports more than 100% used.
func remainingPercent(used float64) float64 {
	return math.Round(math.Min(100, math.Max(0, 100-used))*100) / 100
}

// resetMs reads a reset time given as unix seconds or as RFC 3339, the two
// forms the providers use, and returns it in unix milliseconds.
func resetMs(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var at time.Time
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds <= 0 {
			return nil
		}
		at = time.Unix(seconds, 0)
	} else if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		at = parsed
	} else {
		return nil
	}
	ms := at.UnixMilli()
	if ms <= 0 {
		return nil
	}
	return &ms
}

// roundToMinute rounds a reset to the nearest minute, so resets derived from
// a relative countdown do not differ on every request.
func roundToMinute(ms *int64) *int64 {
	if ms == nil {
		return nil
	}
	rounded := time.UnixMilli(*ms).Round(time.Minute).UnixMilli()
	return &rounded
}
