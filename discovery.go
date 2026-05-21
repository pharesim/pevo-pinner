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

		// 1. Paper PDF CID
		if ipfsCID.Valid && ipfsCID.String != "" && !seen[ipfsCID.String] {
			if err := ValidateCID(ipfsCID.String); err != nil {
				dropped++
				log.Printf("[discovery] dropped invalid paper CID by %s/%s: %v", author, permlink, err)
			} else {
				seen[ipfsCID.String] = true
				items = append(items, DiscoveredItem{
					Author:     author,
					Permlink:   permlink,
					Title:      title,
					CID:        ipfsCID.String,
					Filename:   filename.String,
					CIDType:    "paper",
					Discipline: discipline.String,
					Created:    created,
				})
			}
		}

		// 2. Supplementary files
		if suppJSON.Valid && suppJSON.String != "" {
			var suppFiles []SupplementaryFile
			if err := json.Unmarshal([]byte(suppJSON.String), &suppFiles); err == nil {
				for _, sf := range suppFiles {
					if sf.CID == "" || seen[sf.CID] {
						continue
					}
					if err := ValidateCID(sf.CID); err != nil {
						dropped++
						log.Printf("[discovery] dropped invalid supplementary CID by %s/%s: %v", author, permlink, err)
						continue
					}
					seen[sf.CID] = true
					items = append(items, DiscoveredItem{
						Author:     author,
						Permlink:   permlink,
						Title:      title,
						CID:        sf.CID,
						Filename:   sf.Filename,
						CIDType:    "supplementary",
						Discipline: discipline.String,
						Created:    created,
					})
				}
			}
		}

		// 3. Inline images
		if body.Valid {
			matches := ipfsCIDRegex.FindAllStringSubmatch(body.String, -1)
			for _, m := range matches {
				cid := m[1]
				if seen[cid] {
					continue
				}
				if err := ValidateCID(cid); err != nil {
					dropped++
					log.Printf("[discovery] dropped invalid inline-image CID by %s/%s: %v", author, permlink, err)
					continue
				}
				seen[cid] = true
				items = append(items, DiscoveredItem{
					Author:     author,
					Permlink:   permlink,
					Title:      title,
					CID:        cid,
					CIDType:    "inline_image",
					Discipline: discipline.String,
					Created:    created,
				})
			}
		}
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
