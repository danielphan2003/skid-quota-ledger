package ledger

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

const (
	unchangedTolerance = 0.05
	unchangedSpan      = 30 * time.Minute
	pruneInterval      = time.Hour
	// ResetTolerance is how far apart two resets_at may be and still name the
	// same window reset. Sources disagree by fractions of a second (the panel's
	// quota API against response headers), and a reset derived from a
	// relative "resets after N seconds" drifts by a second or so per request.
	ResetTolerance = time.Minute
)

type Store struct {
	mu         sync.Mutex
	path       string
	retention  time.Duration
	now        func() time.Time
	samples    []Sample
	latest     map[series]Sample
	lastPruned time.Time
}

func Open(path string, retention time.Duration, now func() time.Time) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	samples, clean, err := readSamples(path)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(samples, byAt)
	store := &Store{path: path, retention: retention, now: now, samples: samples}
	pruned := store.dropExpired(now())
	if !clean || pruned {
		if err := store.rewrite(); err != nil {
			return nil, err
		}
	}
	store.rebuildLatest()
	return store, nil
}

func (s *Store) Append(batch []Sample) (accepted, skipped int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if err := s.pruneIfDue(now); err != nil {
		return 0, 0, err
	}
	fresh, latest := s.selectFresh(batch, s.cutoff(now))
	if err := appendLines(s.path, fresh); err != nil {
		return 0, 0, err
	}
	s.insert(fresh)
	for key, sample := range latest {
		s.latest[key] = sample
	}
	return len(fresh), len(batch) - len(fresh), nil
}

func (s *Store) Since(since int64, limit int) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()

	matching := s.samples[s.firstAtOrAfter(since):]
	if len(matching) > limit {
		matching = matching[len(matching)-limit:]
	}
	return append([]Sample{}, matching...)
}

func (s *Store) selectFresh(batch []Sample, cutoff int64) ([]Sample, map[series]Sample) {
	ordered := slices.Clone(batch)
	slices.SortStableFunc(ordered, byAt)

	fresh := make([]Sample, 0, len(ordered))
	latest := make(map[series]Sample)
	for _, sample := range ordered {
		key := sample.series()
		previous, known := latest[key]
		if !known {
			previous, known = s.latest[key]
		}
		if sample.At < cutoff || s.holdsInstant(sample) || (known && repeats(previous, sample)) {
			continue
		}
		fresh = append(fresh, sample)
		if !known || sample.At >= previous.At {
			latest[key] = sample
		}
	}
	return fresh, latest
}

func repeats(previous, next Sample) bool {
	age := time.Duration(next.At-previous.At) * time.Millisecond
	return age >= 0 && age < unchangedSpan &&
		math.Abs(next.Remaining-previous.Remaining) <= unchangedTolerance &&
		sameReset(previous.ResetsAt, next.ResetsAt)
}

func sameReset(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	delta := *a - *b
	if delta < 0 {
		delta = -delta
	}
	return delta <= ResetTolerance.Milliseconds()
}

func (s *Store) holdsInstant(sample Sample) bool {
	key := sample.series()
	for _, stored := range s.samples[s.firstAtOrAfter(sample.At):] {
		if stored.At != sample.At {
			return false
		}
		if stored.series() == key {
			return true
		}
	}
	return false
}

func (s *Store) insert(fresh []Sample) {
	if len(fresh) == 0 {
		return
	}
	inOrder := len(s.samples) == 0 || s.samples[len(s.samples)-1].At <= fresh[0].At
	s.samples = append(s.samples, fresh...)
	if !inOrder {
		slices.SortStableFunc(s.samples, byAt)
	}
}

func (s *Store) pruneIfDue(now time.Time) error {
	if now.Sub(s.lastPruned) < pruneInterval {
		return nil
	}
	if !s.dropExpired(now) {
		return nil
	}
	s.rebuildLatest()
	return s.rewrite()
}

func (s *Store) dropExpired(now time.Time) bool {
	s.lastPruned = now
	expired := s.firstAtOrAfter(s.cutoff(now))
	if expired == 0 {
		return false
	}
	s.samples = slices.Clone(s.samples[expired:])
	return true
}

func (s *Store) rebuildLatest() {
	s.latest = make(map[series]Sample)
	for _, sample := range s.samples {
		s.latest[sample.series()] = sample
	}
}

func (s *Store) cutoff(now time.Time) int64 {
	return now.Add(-s.retention).UnixMilli()
}

func (s *Store) firstAtOrAfter(at int64) int {
	return sort.Search(len(s.samples), func(i int) bool { return s.samples[i].At >= at })
}

func (s *Store) rewrite() error {
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".quota-ledger-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())

	if _, err := temp.Write(encodeLines(s.samples)); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), s.path)
}

func readSamples(path string) (samples []Sample, clean bool, err error) {
	file, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()

	clean = true
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, false, readErr
		}
		if len(line) > 0 {
			sample, ok := decodeLine(line)
			if ok {
				samples = append(samples, sample)
			} else {
				clean = false
			}
		}
		if readErr != nil {
			return samples, clean, nil
		}
	}
}

func decodeLine(line []byte) (Sample, bool) {
	if !bytes.HasSuffix(line, []byte("\n")) {
		return Sample{}, false
	}
	var sample Sample
	if err := json.Unmarshal(line, &sample); err != nil {
		return Sample{}, false
	}
	return sample, true
}

func appendLines(path string, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(encodeLines(samples)); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func encodeLines(samples []Sample) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	for _, sample := range samples {
		_ = encoder.Encode(sample)
	}
	return buffer.Bytes()
}

func byAt(a, b Sample) int {
	return cmp.Compare(a.At, b.At)
}
