package main

import (
	"context"
	"log"
	"sort"
	"sync"
)

// AutoPinRunner schedules backend.Pin calls for a discovery batch through a
// bounded goroutine pool, applying a per-author CID cap before scheduling so a
// single hostile accredited author broadcasting many CIDs cannot monopolize
// autopin capacity. The semaphore is a struct field so the concurrency bound
// is global across callers: a discovery refresh and an operator-triggered
// evaluate-rules invocation cannot together exceed `concurrency` in-flight
// pins. Construct once and reuse across batches and concurrent callers.
type AutoPinRunner struct {
	backend     IPFSBackend
	concurrency int
	authorCap   int
	sem         chan struct{}
}

// AutoPinResult reports per-batch counts. Matched is the total enabled-rule
// match count before shedding; Pinned counts successful new pins; Failed
// counts Pin call errors; Shed counts CIDs dropped by the per-author cap;
// IsPinnedErrors counts CIDs skipped because the backend's IsPinned probe
// returned an error (these are not counted as Failed because no Pin call was
// attempted).
type AutoPinResult struct {
	Matched        int
	Pinned         int
	Failed         int
	Shed           int
	IsPinnedErrors int
}

// NewAutoPinRunner returns a runner with the given concurrency and per-author
// cap. Both arguments must be positive; non-positive values are clamped to 1
// so a misconfiguration does not silently disable autopin.
func NewAutoPinRunner(backend IPFSBackend, concurrency, authorCap int) *AutoPinRunner {
	if concurrency < 1 {
		concurrency = 1
	}
	if authorCap < 1 {
		authorCap = 1
	}
	return &AutoPinRunner{
		backend:     backend,
		concurrency: concurrency,
		authorCap:   authorCap,
		sem:         make(chan struct{}, concurrency),
	}
}

// Run pins the items through the bounded pool, applying the per-author cap
// first. Excess CIDs from any author past the cap are shed before any Pin call
// is made for that author's overflow, so a wedged backend on one author's
// in-flight pins cannot push out other authors' work. Run blocks until every
// goroutine it dispatched completes (its own pool drains before return) so a
// caller's batch does not overlap its own next batch. The semaphore is shared
// across the runner instance, so concurrent callers (e.g. discovery callback
// and HTTP evaluate-rules handler) collectively hold at most `concurrency`
// in-flight pins. Returns even if ctx is cancelled: scheduling stops,
// in-flight Pin calls see the cancellation through their own ctx argument.
func (r *AutoPinRunner) Run(ctx context.Context, items []DiscoveredItem) AutoPinResult {
	authorCount := make(map[string]int, 16)
	shedByAuthor := make(map[string]int, 4)
	queued := make([]DiscoveredItem, 0, len(items))
	for _, item := range items {
		if authorCount[item.Author] >= r.authorCap {
			shedByAuthor[item.Author]++
			continue
		}
		authorCount[item.Author]++
		queued = append(queued, item)
	}

	// One summary line per capped author per batch. Sort authors so the log
	// order is stable across runs and tests can pin a deterministic line.
	if len(shedByAuthor) > 0 {
		authors := make([]string, 0, len(shedByAuthor))
		for a := range shedByAuthor {
			authors = append(authors, a)
		}
		sort.Strings(authors)
		for _, a := range authors {
			log.Printf("[autopin] shed %d CIDs from %s (per-author cap=%d)", shedByAuthor[a], a, r.authorCap)
		}
	}

	shedTotal := 0
	for _, n := range shedByAuthor {
		shedTotal += n
	}
	res := AutoPinResult{Matched: len(items), Shed: shedTotal}
	if len(queued) == 0 {
		return res
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	pinned, failed, isPinnedErrors := 0, 0, 0

schedule:
	for _, item := range queued {
		item := item
		select {
		case <-ctx.Done():
			break schedule
		case r.sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-r.sem }()

			already, err := r.backend.IsPinned(ctx, item.CID)
			if err != nil {
				// A swallowed IsPinned error against PinataBackend (live HTTP
				// GET) coerces the answer to "not pinned" and falls through to
				// an unconditional Pin attempt — a pin storm against a backend
				// that may already be under stress. Surface the error, skip
				// the Pin call, and count it separately.
				log.Printf("[autopin] IsPinned failed for %s: %v", item.CID, err)
				mu.Lock()
				isPinnedErrors++
				mu.Unlock()
				return
			}
			if already {
				return
			}
			if err := r.backend.Pin(ctx, item.CID); err != nil {
				log.Printf("[autopin] failed to pin %s: %v", item.CID, err)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			mu.Lock()
			pinned++
			mu.Unlock()
		}()
	}
	wg.Wait()

	res.Pinned = pinned
	res.Failed = failed
	res.IsPinnedErrors = isPinnedErrors
	return res
}
