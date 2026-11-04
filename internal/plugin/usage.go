package plugin

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Silthus/cliproxy-quota-ledger/internal/ledger"
)

// usageRecord is the part of CPA's pluginapi.UsageRecord the ledger reads. CPA
// sends the record with Go's default field names.
type usageRecord struct {
	AuthID          string
	Provider        string
	Failed          bool
	ResponseHeaders map[string][]string
}

// handleUsage records the quota windows that a completed upstream response
// reported in its rate-limit headers, so the history grows with every request
// instead of only while a dashboard is open.
func (p *Plugin) handleUsage(raw []byte) ([]byte, bool) {
	var record usageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return failure("invalid_request", err.Error())
	}
	store := p.currentStore()
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
	if len(valid) > 0 {
		if _, _, err := store.Append(valid); err != nil {
			return failure("store_failed", err.Error())
		}
	}
	return success(struct{}{})
}

// samplesFromHeaders maps provider rate-limit headers to ledger samples. The
// window ids match the management panel's own, so header samples and panel
// samples extend the same series.
func samplesFromHeaders(record usageRecord, now time.Time) []ledger.Sample {
	auth := strings.TrimSpace(record.AuthID)
	if auth == "" || len(record.ResponseHeaders) == 0 {
		return nil
	}
	headers := lowerKeys(record.ResponseHeaders)
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	at := now.UnixMilli()
	sample := func(window string, remaining float64, resetsAt *int64) ledger.Sample {
		return ledger.Sample{At: at, Auth: auth, Provider: provider, Window: window, Remaining: remaining, ResetsAt: resetsAt}
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
			samples = append(samples, sample(window.id, remainingPercent(utilization*100), unixSecondsMs(headers[prefix+"reset"])))
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
			resetsAt := unixSecondsMs(headers[prefix+"reset-at"])
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

func unixSecondsMs(raw string) *int64 {
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return nil
	}
	ms := seconds * 1000
	return &ms
}
