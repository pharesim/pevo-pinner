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
// autopin capacity. Construct once and reuse across batches; Run is
// safe for serial reuse, not for concurrent batches.
type AutoPinRunner struct {
	backend     IPFSBackend
	concurrency int
	authorCap   int
}

// AutoPinResult reports per-batch counts. Matched is the total enabled-rule
// match count before shedding; Pinned counts successful new pins; Failed
// counts Pin call errors; Shed counts CIDs dropped by the per-author cap.
type AutoPinResult struct {
	Matched int
	Pinned  int
	Failed  int
	Shed    int
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
	}
}

// Run pins the items through the bounded pool, applying the per-author cap
// first. Excess CIDs from any author past the cap are shed before any Pin call
// is made for that author's overflow, so a wedged backend on one author's
// in-flight pins cannot push out other authors' work. Run blocks until every
// dispatched goroutine completes (the pool drains before return) so successive
// discovery batches don't overlap each other's in-flight pins. Returns even
// if ctx is cancelled: scheduling stops, in-flight Pin calls see the
// cancellation through their own ctx argument.
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

	res := AutoPinResult{Matched: len(items), Shed: sumValues(shedByAuthor)}
	if len(queued) == 0 {
		return res
	}

	sem := make(chan struct{}, r.concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	pinned, failed := 0, 0

schedule:
	for _, item := range queued {
		item := item
		select {
		case <-ctx.Done():
			break schedule
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			already, _ := r.backend.IsPinned(ctx, item.CID)
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
	return res
}

func sumValues(m map[string]int) int {
	s := 0
	for _, v := range m {
		s += v
	}
	return s
}
