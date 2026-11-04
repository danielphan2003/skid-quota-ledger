package ledger_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Silthus/cliproxy-quota-ledger/internal/ledger"
)

const day = 24 * time.Hour

type clock struct{ now time.Time }

func (c *clock) Now() time.Time           { return c.now }
func (c *clock) advance(by time.Duration) { c.now = c.now.Add(by) }

func openStore(t *testing.T, path string, retention time.Duration, c *clock) *ledger.Store {
	t.Helper()
	store, err := ledger.Open(path, retention, c.Now)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	return store
}

func newStore(t *testing.T) (*ledger.Store, string, *clock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	c := &clock{now: t0}
	return openStore(t, path, 35*day, c), path, c
}

func appendSamples(t *testing.T, store *ledger.Store, samples ...ledger.Sample) (int, int) {
	t.Helper()
	accepted, skipped, err := store.Append(samples)
	if err != nil {
		t.Fatalf("Append() = %v", err)
	}
	return accepted, skipped
}

func assertCounts(t *testing.T, gotAccepted, gotSkipped, wantAccepted, wantSkipped int) {
	t.Helper()
	if gotAccepted != wantAccepted || gotSkipped != wantSkipped {
		t.Fatalf("Append() accepted=%d skipped=%d, want accepted=%d skipped=%d", gotAccepted, gotSkipped, wantAccepted, wantSkipped)
	}
}

func stampsOf(samples []ledger.Sample) []int64 {
	stamps := make([]int64, len(samples))
	for i, sample := range samples {
		stamps[i] = sample.At
	}
	return stamps
}

func assertStamps(t *testing.T, samples []ledger.Sample, want ...time.Time) {
	t.Helper()
	got := stampsOf(samples)
	if len(got) != len(want) {
		t.Fatalf("got %d samples %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i].UnixMilli() {
			t.Fatalf("sample %d at %d, want %d (all: %v)", i, got[i], want[i].UnixMilli(), got)
		}
	}
}

func fileLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func TestAppendSkipsUnchangedReadingWithinThirtyMinutes(t *testing.T) {
	store, _, _ := newStore(t)

	accepted, skipped := appendSamples(t, store,
		reading(t0, 50),
		reading(t0.Add(29*time.Minute), 50.04),
		reading(t0.Add(30*time.Minute), 50),
	)

	assertCounts(t, accepted, skipped, 2, 1)
	assertStamps(t, store.Since(0, 100), t0, t0.Add(30*time.Minute))
}

func TestAppendKeepsChangedReadings(t *testing.T) {
	store, _, _ := newStore(t)
	otherWindow := reading(t0.Add(2*time.Minute), 50)
	otherWindow.Window = "seven_day"
	newReset := reading(t0.Add(3*time.Minute), 49.94)
	newReset.ResetsAt = ms(t0.Add(5 * time.Hour).UnixMilli())

	accepted, skipped := appendSamples(t, store,
		reading(t0, 50),
		reading(t0.Add(time.Minute), 49.94),
		otherWindow,
		newReset,
	)

	assertCounts(t, accepted, skipped, 4, 0)
}

func TestAppendTreatsResetsWithinAMinuteAsTheSame(t *testing.T) {
	store, _, _ := newStore(t)
	first := reading(t0, 50)
	first.ResetsAt = ms(t0.Add(5 * time.Hour).UnixMilli())
	jittered := reading(t0.Add(time.Minute), 50)
	jittered.ResetsAt = ms(*first.ResetsAt + 59_999)
	moved := reading(t0.Add(2*time.Minute), 50)
	moved.ResetsAt = ms(*first.ResetsAt - 60_001)

	accepted, skipped := appendSamples(t, store, first, jittered, moved)

	assertCounts(t, accepted, skipped, 2, 1)
	assertStamps(t, store.Since(0, 100), t0, t0.Add(2*time.Minute))
}

func TestAppendIsIdempotentForRetriedBatches(t *testing.T) {
	store, _, _ := newStore(t)
	batch := []ledger.Sample{reading(t0, 50), reading(t0.Add(45*time.Minute), 40)}
	appendSamples(t, store, batch...)

	accepted, skipped := appendSamples(t, store, batch...)

	assertCounts(t, accepted, skipped, 0, 2)
}

func TestAppendSkipsSamplesOutsideRetention(t *testing.T) {
	store, _, _ := newStore(t)

	accepted, skipped := appendSamples(t, store, reading(t0.Add(-36*day), 50), reading(t0, 50))

	assertCounts(t, accepted, skipped, 1, 1)
}

func TestSinceReturnsSamplesFromCutoffInOrder(t *testing.T) {
	store, _, _ := newStore(t)
	appendSamples(t, store,
		reading(t0.Add(2*time.Hour), 30),
		reading(t0, 50),
		reading(t0.Add(time.Hour), 40),
	)

	assertStamps(t, store.Since(t0.Add(time.Hour).UnixMilli(), 100), t0.Add(time.Hour), t0.Add(2*time.Hour))
	assertStamps(t, store.Since(0, 2), t0.Add(time.Hour), t0.Add(2*time.Hour))
}

func TestReopenRestoresSamplesFromPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "samples.jsonl")
	c := &clock{now: t0}
	store := openStore(t, path, 35*day, c)
	appendSamples(t, store, reading(t0, 50), reading(t0.Add(time.Hour), 40))

	reopened := openStore(t, path, 35*day, c)

	assertStamps(t, reopened.Since(0, 100), t0, t0.Add(time.Hour))
	assertMode(t, path, 0o600)
	assertMode(t, filepath.Dir(path), os.ModeDir|0o700)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != want {
		t.Fatalf("mode of %s = %v, want %v", path, info.Mode(), want)
	}
}

func TestOpenRewritesFileWithoutExpiredOrCorruptLines(t *testing.T) {
	recent := `{"at":` + strconv.FormatInt(t0.Add(-day).UnixMilli(), 10) + `,"auth":"a","provider":"p","window":"w","remaining":10,"resets_at":null}`
	expired := `{"at":` + strconv.FormatInt(t0.Add(-40*day).UnixMilli(), 10) + `,"auth":"a","provider":"p","window":"w","remaining":20}`
	cases := map[string]string{
		"expired line":        expired + "\n" + recent + "\n",
		"corrupt line":        "not json\n" + recent + "\n",
		"torn last line":      recent + "\n" + `{"at":17`,
		"line missing fields": `{"at":` + strconv.FormatInt(t0.UnixMilli(), 10) + `,"auth":"a"}` + "\n" + recent + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "samples.jsonl")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			store := openStore(t, path, 35*day, &clock{now: t0})

			assertStamps(t, store.Since(0, 100), t0.Add(-day))
			lines := fileLines(t, path)
			if len(lines) != 1 || lines[0] != recent {
				t.Fatalf("rewritten file = %q, want only %q", lines, recent)
			}
		})
	}
}

func TestAppendPrunesExpiredSamplesAtMostHourly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	c := &clock{now: t0}
	store := openStore(t, path, day, c)
	nearlyExpired := reading(t0.Add(-day+10*time.Minute), 90)
	appendSamples(t, store, nearlyExpired)

	c.advance(20 * time.Minute)
	appendSamples(t, store, reading(c.now, 80))
	if got := len(store.Since(0, 100)); got != 2 {
		t.Fatalf("expired sample pruned before the hourly window: %d samples", got)
	}

	c.advance(41 * time.Minute)
	appendSamples(t, store, reading(c.now, 70))

	assertStamps(t, store.Since(0, 100), t0.Add(20*time.Minute), t0.Add(61*time.Minute))
	if got := len(fileLines(t, path)); got != 2 {
		t.Fatalf("file has %d lines after pruning, want 2", got)
	}
}
