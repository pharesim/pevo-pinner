package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/lib/pq"
)

// DiscoveredItem represents a CID found on-chain via HAF.
type DiscoveredItem struct {
	Author     string    `json:"author"`
	Permlink   string    `json:"permlink"`
	Title      string    `json:"title"`
	CID        string    `json:"cid"`
	Filename   string    `json:"filename"`
	CIDType    string    `json:"cid_type"` // paper, supplementary, inline_image
	Discipline string    `json:"discipline"`
	Created    time.Time `json:"created"`
}

var ipfsCIDRegex = regexp.MustCompile(`/ipfs/(Qm[1-9A-HJ-NP-Za-km-z]{44}|b[A-Za-z2-7]{58})/`)

// buildDiscoveryQuery returns a query with the app tag interpolated directly,
// avoiding prepared statements which PgBouncer (used by HAF nodes) does not support.
func buildDiscoveryQuery(appTag string) string {
	t := pq.QuoteLiteral(appTag)
	p := pq.QuoteLiteral(appTag + "%")
	return `
SELECT c.author, c.permlink, c.title,
       c.json_metadata -> ` + t + ` ->> 'ipfs_cid' AS ipfs_cid,
       c.json_metadata -> ` + t + ` ->> 'ipfs_filename' AS filename,
       c.json_metadata -> ` + t + ` ->> 'discipline' AS discipline,
       c.json_metadata -> ` + t + ` -> 'supplementary_files' AS supplementary_files,
       c.body,
       'paper' AS cid_type,
       c.created
FROM hafsql.comments c
WHERE c.parent_author = ''
  AND c.parent_permlink = ` + t + `
  AND (c.json_metadata -> ` + t + ` ->> 'type') IN ('paper', 'bridge_paper')
  AND c.json_metadata ->> 'app' LIKE ` + p + `
  AND c.json_metadata -> ` + t + ` ->> 'ipfs_cid' IS NOT NULL
ORDER BY c.created DESC
`
}

// SupplementaryFile represents one entry in the supplementary_files JSON array.
type SupplementaryFile struct {
	CID      string `json:"cid"`
	Filename string `json:"filename"`
}

// OnRefreshFunc is called after each successful discovery refresh with the new items.
type OnRefreshFunc func(items []DiscoveredItem)

// Discovery manages periodic HAF queries and an in-memory CID cache.
type Discovery struct {
	db       *sql.DB
	appTag   string
	interval time.Duration

	mu    sync.RWMutex
	items []DiscoveredItem

	onRefresh OnRefreshFunc
	cancel    context.CancelFunc
}

// NewDiscovery creates a Discovery instance and opens the database connection.
func NewDiscovery(databaseURL, appTag string, interval time.Duration) (*Discovery, error) {
	// Add sslmode=disable if not specified — HAF nodes typically don't support SSL
	if u, err := url.Parse(databaseURL); err == nil && u.Query().Get("sslmode") == "" {
		q := u.Query()
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
		databaseURL = u.String()
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(10 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	return &Discovery{
		db:       db,
		appTag:   appTag,
		interval: interval,
	}, nil
}

// SetOnRefresh registers a callback invoked after each successful discovery refresh.
func (d *Discovery) SetOnRefresh(fn OnRefreshFunc) {
	d.onRefresh = fn
}

// Start runs the initial query and begins periodic refresh.
func (d *Discovery) Start(ctx context.Context) {
	ctx, d.cancel = context.WithCancel(ctx)

	// Initial fetch
	if err := d.refresh(ctx); err != nil {
		log.Printf("[discovery] initial query failed: %v", err)
	}

	go func() {
		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := d.refresh(ctx); err != nil {
					log.Printf("[discovery] refresh failed: %v", err)
				}
			}
		}
	}()
}

// Stop cancels the periodic refresh.
func (d *Discovery) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
	d.db.Close()
}

// Items returns the current snapshot of discovered items.
func (d *Discovery) Items() []DiscoveredItem {
	d.mu.RLock()
	defer d.mu.RUnlock()
	cp := make([]DiscoveredItem, len(d.items))
	copy(cp, d.items)
	return cp
}

// discoveryRow is the in-memory shape of a single HAF result row. Extracted from
// `refresh()` so the per-row CID-extraction logic (paper / supplementary /
// inline-image) can be exercised in tests without stubbing `sql.Rows`.
type discoveryRow struct {
	Author     string
	Permlink   string
	Title      string
	IpfsCID    string
	Filename   string
	Discipline string
	SuppJSON   string
	Body       string
	Created    time.Time
}

// extractItemsFromRow yields the DiscoveredItems contained in one HAF row,
// dropping any CID that fails `ValidateCID`. The `seen` map is updated in place
// so callers can deduplicate across rows. Returns the items kept and the count
// of CIDs dropped by the validator.
//
// The three branches correspond to the three CID entry points the pinner
// supports: paper PDF CID from `json_metadata.ipfs_cid`, supplementary files
// from the `supplementary_files` JSON array, and inline-image CIDs scraped from
// the post body via `ipfsCIDRegex`.
func extractItemsFromRow(row discoveryRow, seen map[string]bool) (items []DiscoveredItem, dropped int) {
	// 1. Paper PDF CID
	if row.IpfsCID != "" && !seen[row.IpfsCID] {
		if err := ValidateCID(row.IpfsCID); err != nil {
			dropped++
			log.Printf("[discovery] dropped invalid paper CID by %s/%s: %v", row.Author, row.Permlink, err)
		} else {
			seen[row.IpfsCID] = true
			items = append(items, DiscoveredItem{
				Author:     row.Author,
				Permlink:   row.Permlink,
				Title:      row.Title,
				CID:        row.IpfsCID,
				Filename:   row.Filename,
				CIDType:    "paper",
				Discipline: row.Discipline,
				Created:    row.Created,
			})
		}
	}

	// 2. Supplementary files
	if row.SuppJSON != "" {
		var suppFiles []SupplementaryFile
		if err := json.Unmarshal([]byte(row.SuppJSON), &suppFiles); err == nil {
			for _, sf := range suppFiles {
				if sf.CID == "" || seen[sf.CID] {
					continue
				}
				if err := ValidateCID(sf.CID); err != nil {
					dropped++
					log.Printf("[discovery] dropped invalid supplementary CID by %s/%s: %v", row.Author, row.Permlink, err)
					continue
				}
				seen[sf.CID] = true
				items = append(items, DiscoveredItem{
					Author:     row.Author,
					Permlink:   row.Permlink,
					Title:      row.Title,
					CID:        sf.CID,
					Filename:   sf.Filename,
					CIDType:    "supplementary",
					Discipline: row.Discipline,
					Created:    row.Created,
				})
			}
		}
	}

	// 3. Inline images
	if row.Body != "" {
		matches := ipfsCIDRegex.FindAllStringSubmatch(row.Body, -1)
		for _, m := range matches {
			cid := m[1]
			if seen[cid] {
				continue
			}
			if err := ValidateCID(cid); err != nil {
				dropped++
				log.Printf("[discovery] dropped invalid inline-image CID by %s/%s: %v", row.Author, row.Permlink, err)
				continue
			}
			seen[cid] = true
			items = append(items, DiscoveredItem{
				Author:     row.Author,
				Permlink:   row.Permlink,
				Title:      row.Title,
				CID:        cid,
				CIDType:    "inline_image",
				Discipline: row.Discipline,
				Created:    row.Created,
			})
		}
	}

	return items, dropped
}

func (d *Discovery) refresh(ctx context.Context) error {
	query := buildDiscoveryQuery(d.appTag)
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	var items []DiscoveredItem
	var dropped int

	for rows.Next() {
		var (
			author, permlink, title string
			ipfsCID, filename       sql.NullString
			discipline              sql.NullString
			suppJSON                sql.NullString
			body                    sql.NullString
			cidType                 string
			created                 time.Time
		)
		if err := rows.Scan(&author, &permlink, &title, &ipfsCID, &filename, &discipline, &suppJSON, &body, &cidType, &created); err != nil {
			log.Printf("[discovery] scan error: %v", err)
			continue
		}

		rowItems, rowDropped := extractItemsFromRow(discoveryRow{
			Author:     author,
			Permlink:   permlink,
			Title:      title,
			IpfsCID:    ipfsCID.String,
			Filename:   filename.String,
			Discipline: discipline.String,
			SuppJSON:   suppJSON.String,
			Body:       body.String,
			Created:    created,
		}, seen)
		items = append(items, rowItems...)
		dropped += rowDropped
		_ = cidType // column is selected for query shape parity; row.CIDType is set per-branch above
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows iteration: %w", err)
	}

	d.mu.Lock()
	d.items = items
	d.mu.Unlock()

	if dropped > 0 {
		log.Printf("[discovery] found %d CIDs (dropped %d invalid)", len(items), dropped)
	} else {
		log.Printf("[discovery] found %d CIDs", len(items))
	}

	if d.onRefresh != nil {
		d.onRefresh(items)
	}

	return nil
}
