package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AutoPinRule defines a filter rule for automatic pinning.
// All non-empty filters must match (AND). Any matching enabled rule triggers a pin (OR across rules).
type AutoPinRule struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Enabled       bool     `json:"enabled"`
	Disciplines   []string `json:"disciplines"`    // empty = any
	Authors       []string `json:"authors"`        // empty = any
	CIDTypes      []string `json:"cid_types"`      // empty = any; paper, supplementary, inline_image
	TitlePattern  string   `json:"title_pattern"`  // case-insensitive substring; empty = any
	CreatedAfter  string   `json:"created_after"`  // RFC 3339 date (YYYY-MM-DD); empty = no lower bound
	CreatedBefore string   `json:"created_before"` // RFC 3339 date (YYYY-MM-DD); empty = no upper bound
}

// Matches returns true if the discovered item satisfies all non-empty filters in this rule.
func (r *AutoPinRule) Matches(item DiscoveredItem) bool {
	if !r.Enabled {
		return false
	}

	if len(r.Disciplines) > 0 && !containsCI(r.Disciplines, item.Discipline) {
		return false
	}

	if len(r.Authors) > 0 && !containsCI(r.Authors, item.Author) {
		return false
	}

	if len(r.CIDTypes) > 0 && !containsCI(r.CIDTypes, item.CIDType) {
		return false
	}

	if r.TitlePattern != "" && !strings.Contains(strings.ToLower(item.Title), strings.ToLower(r.TitlePattern)) {
		return false
	}

	if r.CreatedAfter != "" {
		t, err := time.Parse("2006-01-02", r.CreatedAfter)
		if err == nil && item.Created.Before(t) {
			return false
		}
	}

	if r.CreatedBefore != "" {
		t, err := time.Parse("2006-01-02", r.CreatedBefore)
		if err == nil && !item.Created.Before(t.AddDate(0, 0, 1)) {
			return false
		}
	}

	return true
}

func containsCI(haystack []string, needle string) bool {
	n := strings.ToLower(needle)
	for _, s := range haystack {
		if strings.ToLower(s) == n {
			return true
		}
	}
	return false
}

// AutoPinManager manages auto-pin rules and evaluates them after discovery.
type AutoPinManager struct {
	mu       sync.RWMutex
	rules    []AutoPinRule
	filePath string
}

// NewAutoPinManager loads rules from the JSON file (or starts empty).
func NewAutoPinManager(dataDir string) (*AutoPinManager, error) {
	fp := filepath.Join(dataDir, "autopin.json")
	m := &AutoPinManager{filePath: fp}

	data, err := os.ReadFile(fp)
	if err != nil {
		if os.IsNotExist(err) {
			// Default rule: pin everything
			m.rules = []AutoPinRule{{
				ID:      newID(),
				Name:    "All PEvO files",
				Enabled: true,
			}}
			if err := m.saveLocked(); err != nil {
				return nil, fmt.Errorf("writing default autopin rules: %w", err)
			}
			log.Printf("[autopin] created default rule: pin all files")
			return m, nil
		}
		return nil, fmt.Errorf("reading autopin rules: %w", err)
	}

	if err := json.Unmarshal(data, &m.rules); err != nil {
		return nil, fmt.Errorf("parsing autopin rules: %w", err)
	}

	log.Printf("[autopin] loaded %d rules from %s", len(m.rules), fp)
	return m, nil
}

// Rules returns a copy of all rules.
func (m *AutoPinManager) Rules() []AutoPinRule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make([]AutoPinRule, len(m.rules))
	copy(cp, m.rules)
	return cp
}

// AddRule adds a new rule and persists to disk.
func (m *AutoPinManager) AddRule(r AutoPinRule) (AutoPinRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r.ID = newID()
	m.rules = append(m.rules, r)
	if err := m.saveLocked(); err != nil {
		// Roll back
		m.rules = m.rules[:len(m.rules)-1]
		return AutoPinRule{}, err
	}
	return r, nil
}

// UpdateRule replaces a rule by ID and persists.
func (m *AutoPinManager) UpdateRule(id string, r AutoPinRule) (AutoPinRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, existing := range m.rules {
		if existing.ID == id {
			r.ID = id
			m.rules[i] = r
			if err := m.saveLocked(); err != nil {
				m.rules[i] = existing // roll back
				return AutoPinRule{}, err
			}
			return r, nil
		}
	}
	return AutoPinRule{}, fmt.Errorf("rule %q not found", id)
}

// DeleteRule removes a rule by ID and persists.
func (m *AutoPinManager) DeleteRule(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, r := range m.rules {
		if r.ID == id {
			m.rules = append(m.rules[:i], m.rules[i+1:]...)
			return m.saveLocked()
		}
	}
	return fmt.Errorf("rule %q not found", id)
}

// SetAllEnabled sets the enabled flag on all rules and persists.
func (m *AutoPinManager) SetAllEnabled(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rules {
		m.rules[i].Enabled = enabled
	}
	return m.saveLocked()
}

// MatchingItems returns the items that match at least one enabled rule.
// The full DiscoveredItem (not just the CID) is returned so the autopin
// runner can apply per-author shedding before scheduling Pin calls.
func (m *AutoPinManager) MatchingItems(items []DiscoveredItem) []DiscoveredItem {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var matched []DiscoveredItem
	for _, item := range items {
		for _, rule := range m.rules {
			if rule.Matches(item) {
				matched = append(matched, item)
				break
			}
		}
	}
	return matched
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (m *AutoPinManager) saveLocked() error {
	data, err := json.MarshalIndent(m.rules, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling autopin rules: %w", err)
	}
	if err := os.WriteFile(m.filePath, data, 0o644); err != nil {
		return fmt.Errorf("writing autopin rules: %w", err)
	}
	return nil
}
