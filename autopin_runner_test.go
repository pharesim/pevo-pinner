package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackend records Pin calls and optionally blocks via the gate channel so
// tests can pin a deterministic mid-flight state. Implements IPFSBackend.
type fakeBackend struct {
	mu              sync.Mutex
	pinned          []string
	gate            chan struct{} // closed = release Pin; nil = no gating
	maxInFlight     int32
	curInFlight     int32
	failCIDs        map[string]bool
	pinnedRemote    map[string]bool
	isPinnedErrCIDs map[string]bool // CIDs for which IsPinned returns an error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{pinnedRemote: map[string]bool{}, isPinnedErrCIDs: map[string]bool{}}
}

func (b *fakeBackend) Pin(ctx context.Context, cid string) error {
	cur := atomic.AddInt32(&b.curInFlight, 1)
	defer atomic.AddInt32(&b.curInFlight, -1)
	for {
		max := atomic.LoadInt32(&b.maxInFlight)
		if cur <= max || atomic.CompareAndSwapInt32(&b.maxInFlight, max, cur) {
			break
		}
	}

	if b.gate != nil {
		select {
		case <-b.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failCIDs[cid] {
		return fmt.Errorf("fake pin failure for %s", cid)
	}
	b.pinned = append(b.pinned, cid)
	return nil
}

func (b *fakeBackend) Unpin(ctx context.Context, cid string) error { return nil }
func (b *fakeBackend) IsPinned(ctx context.Context, cid string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.isPinnedErrCIDs[cid] {
		return false, fmt.Errorf("fake IsPinned failure for %s", cid)
	}
	return b.pinnedRemote[cid], nil
}
func (b *fakeBackend) PinnedCIDs(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]string, len(b.pinned))
	copy(cp, b.pinned)
	return cp, nil
}
func (b *fakeBackend) Drain(ctx context.Context) error { return nil }
func (b *fakeBackend) Close() error                    { return nil }

func (b *fakeBackend) pinCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pinned)
}

// makeItems returns n DiscoveredItems with the given author and distinct CID
// strings. The runner never validates CID syntax (ValidateCID runs at
// discovery time), so the synthetic strings here only need to be unique.
func makeItems(t *testing.T, author string, n int) []DiscoveredItem {
	t.Helper()
	items := make([]DiscoveredItem, n)
	for i := 0; i < n; i++ {
		items[i] = DiscoveredItem{Author: author, CID: fmt.Sprintf("cid-%s-%d", author, i)}
	}
	return items
}

// captureLog redirects log output to a buffer for the duration of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})
	return buf
}

// TestAutoPinRunnerShedsOneAuthorPastCap proves a single hostile author with
// 100 CIDs and per-author cap = 20 produces exactly 20 Pin attempts and one
// summary log line citing the author and the cap.
func TestAutoPinRunnerShedsOneAuthorPastCap(t *testing.T) {
	buf := captureLog(t)
	backend := newFakeBackend()
	runner := NewAutoPinRunner(backend, 4, 20)

	items := makeItems(t, "hostile-author", 100)
	res := runner.Run(context.Background(), items)

	if got, want := backend.pinCount(), 20; got != want {
		t.Errorf("Pin call count = %d, want %d", got, want)
	}
	if res.Pinned != 20 {
		t.Errorf("res.Pinned = %d, want 20", res.Pinned)
	}
	if res.Shed != 80 {
		t.Errorf("res.Shed = %d, want 80", res.Shed)
	}
	if res.Matched != 100 {
		t.Errorf("res.Matched = %d, want 100", res.Matched)
	}

	logs := buf.String()
	want := "[autopin] shed 80 CIDs from hostile-author (per-author cap=20)"
	if !strings.Contains(logs, want) {
		t.Errorf("logs missing %q\nfull log:\n%s", want, logs)
	}
	// Exactly one shed line: count occurrences.
	if got := strings.Count(logs, "shed "); got != 1 {
		t.Errorf("shed-log lines = %d, want 1\nfull log:\n%s", got, logs)
	}
}

// TestAutoPinRunnerDoesNotShedAcrossAuthors proves the per-author cap does
// NOT shed across multiple authors. Ten authors with ten CIDs each, cap = 20:
// no author hits the cap, so all 100 CIDs are pinned and no shed log fires.
func TestAutoPinRunnerDoesNotShedAcrossAuthors(t *testing.T) {
	buf := captureLog(t)
	backend := newFakeBackend()
	runner := NewAutoPinRunner(backend, 4, 20)

	var all []DiscoveredItem
	for i := 0; i < 10; i++ {
		all = append(all, makeItems(t, fmt.Sprintf("author-%d", i), 10)...)
	}
	res := runner.Run(context.Background(), all)

	if got, want := backend.pinCount(), 100; got != want {
		t.Errorf("Pin call count = %d, want %d", got, want)
	}
	if res.Pinned != 100 {
		t.Errorf("res.Pinned = %d, want 100", res.Pinned)
	}
	if res.Shed != 0 {
		t.Errorf("res.Shed = %d, want 0", res.Shed)
	}

	if strings.Contains(buf.String(), "shed ") {
		t.Errorf("unexpected shed log line:\n%s", buf.String())
	}
}

// TestAutoPinRunnerRespectsConcurrencyBound proves the bounded pool never
// runs more than N Pin calls simultaneously. With concurrency = 4 and a Pin
// that holds the gate until released, the max-in-flight observation must
// equal 4.
func TestAutoPinRunnerRespectsConcurrencyBound(t *testing.T) {
	captureLog(t)
	backend := newFakeBackend()
	backend.gate = make(chan struct{})

	runner := NewAutoPinRunner(backend, 4, 50)
	items := makeItems(t, "author", 20)

	done := make(chan AutoPinResult, 1)
	go func() {
		done <- runner.Run(context.Background(), items)
	}()

	// Give the runner time to fill the semaphore. The gate keeps every Pin
	// in-flight, so maxInFlight plateaus at the concurrency bound. 50ms is
	// well above the scheduling-noise floor.
	time.Sleep(50 * time.Millisecond)
	close(backend.gate)
	<-done

	if got := atomic.LoadInt32(&backend.maxInFlight); got != 4 {
		t.Errorf("max in-flight Pin calls = %d, want exactly 4 (pool should saturate)", got)
	}
	if got := backend.pinCount(); got != 20 {
		t.Errorf("Pin call count = %d, want 20", got)
	}
}

// TestAutoPinRunnerEmpty proves an empty batch returns a zero result with no
// log output.
func TestAutoPinRunnerEmpty(t *testing.T) {
	buf := captureLog(t)
	backend := newFakeBackend()
	runner := NewAutoPinRunner(backend, 4, 20)

	res := runner.Run(context.Background(), nil)
	if res.Matched != 0 || res.Pinned != 0 || res.Shed != 0 || res.Failed != 0 {
		t.Errorf("res = %+v, want zero", res)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log output: %q", buf.String())
	}
}

// TestAutoPinRunnerSkipsAlreadyPinned proves CIDs the backend reports as
// already pinned are not re-attempted.
func TestAutoPinRunnerSkipsAlreadyPinned(t *testing.T) {
	captureLog(t)
	backend := newFakeBackend()
	runner := NewAutoPinRunner(backend, 4, 50)

	items := makeItems(t, "author", 10)
	// Mark half as already pinned.
	for i := 0; i < 5; i++ {
		backend.pinnedRemote[items[i].CID] = true
	}

	res := runner.Run(context.Background(), items)
	if res.Pinned != 5 {
		t.Errorf("res.Pinned = %d, want 5 (other 5 already pinned)", res.Pinned)
	}
	if backend.pinCount() != 5 {
		t.Errorf("Pin call count = %d, want 5", backend.pinCount())
	}
}

// TestAutoPinRunnerNonPositiveArgsClampToOne proves NewAutoPinRunner does not
// silently disable autopin on a misconfiguration. Zero / negative values clamp
// to 1, the minimum useful setting.
func TestAutoPinRunnerNonPositiveArgsClampToOne(t *testing.T) {
	captureLog(t)
	backend := newFakeBackend()
	runner := NewAutoPinRunner(backend, 0, -5)

	if runner.concurrency != 1 {
		t.Errorf("concurrency = %d, want 1", runner.concurrency)
	}
	if runner.authorCap != 1 {
		t.Errorf("authorCap = %d, want 1", runner.authorCap)
	}

	// With cap = 1, 5 CIDs from one author should shed 4.
	items := makeItems(t, "author", 5)
	res := runner.Run(context.Background(), items)
	if res.Pinned != 1 || res.Shed != 4 {
		t.Errorf("res = %+v, want Pinned=1, Shed=4", res)
	}
}

// TestAutoPinRunnerSkipsPinOnIsPinnedError proves that when IsPinned returns
// an error, the runner does NOT fall through to call Pin. A swallowed error
// against a live-HTTP IsPinned (e.g. Pinata 429/5xx) would otherwise produce
// repeat Pin attempts every discovery cycle — a pin storm against a backend
// already under stress.
func TestAutoPinRunnerSkipsPinOnIsPinnedError(t *testing.T) {
	buf := captureLog(t)
	backend := newFakeBackend()
	runner := NewAutoPinRunner(backend, 4, 50)

	items := makeItems(t, "author", 5)
	// Two CIDs trip the IsPinned error path; three are healthy.
	backend.isPinnedErrCIDs[items[0].CID] = true
	backend.isPinnedErrCIDs[items[2].CID] = true

	res := runner.Run(context.Background(), items)

	if got, want := backend.pinCount(), 3; got != want {
		t.Errorf("Pin call count = %d, want %d (no Pin call should follow a failed IsPinned)", got, want)
	}
	if res.Pinned != 3 {
		t.Errorf("res.Pinned = %d, want 3", res.Pinned)
	}
	if res.IsPinnedErrors != 2 {
		t.Errorf("res.IsPinnedErrors = %d, want 2", res.IsPinnedErrors)
	}
	if res.Failed != 0 {
		t.Errorf("res.Failed = %d, want 0 (skipped CIDs are not Failed; no Pin was attempted)", res.Failed)
	}

	logs := buf.String()
	if !strings.Contains(logs, "IsPinned failed for "+items[0].CID) {
		t.Errorf("logs missing IsPinned-failed line for %s\nfull log:\n%s", items[0].CID, logs)
	}
}

// TestAutoPinRunnerConcurrentRunsShareBound proves the runner's concurrency
// bound is global across callers. Two goroutines call Run on the same runner
// instance with a gate held; the observed max-in-flight Pin count must equal
// the configured bound, NOT 2×bound. This guards against a regression where
// the semaphore lives as a stack local inside Run, which would let two
// concurrent invocations effectively double the in-flight ceiling.
func TestAutoPinRunnerConcurrentRunsShareBound(t *testing.T) {
	captureLog(t)
	backend := newFakeBackend()
	backend.gate = make(chan struct{})

	const concurrency = 4
	runner := NewAutoPinRunner(backend, concurrency, 100)

	// Two batches with distinct authors so neither batch trips the per-author
	// cap; we want to observe the shared semaphore, not the per-author cap.
	batchA := makeItems(t, "author-a", 20)
	batchB := makeItems(t, "author-b", 20)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		runner.Run(context.Background(), batchA)
	}()
	go func() {
		defer wg.Done()
		runner.Run(context.Background(), batchB)
	}()

	// Give both runners time to saturate the shared semaphore. With the gate
	// held, every started Pin call stays in-flight until release.
	time.Sleep(50 * time.Millisecond)
	close(backend.gate)
	wg.Wait()

	if got := atomic.LoadInt32(&backend.maxInFlight); got != concurrency {
		t.Errorf("max in-flight Pin calls = %d, want exactly %d (shared semaphore should cap two concurrent Run calls to the configured bound)", got, concurrency)
	}
	if got := backend.pinCount(); got != 40 {
		t.Errorf("Pin call count = %d, want 40 (all CIDs from both batches)", got)
	}
}
