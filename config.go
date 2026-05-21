package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Autopin defaults: bounded worker pool + per-author cap defend the autopin
// queue against a single hostile accredited author broadcasting many CIDs.
const (
	defaultAutoPinConcurrency = 4
	defaultAutoPinAuthorCap   = 20
)

type Config struct {
	HAFDatabaseURL     string
	AppTag             string
	IPFSMode           string
	DataDir            string
	PinataAPIKey       string
	PinataSecretKey    string
	Port               string
	GatewayPort        string
	GatewayBind        string
	RefreshInterval    time.Duration
	AutoPinConcurrency int
	AutoPinAuthorCap   int

	// Embedded-node libp2p / fetch knobs. Empty values fall back to library
	// defaults (libp2p picks a random listen port, no anchor peer beyond
	// the DHT's hardcoded seeds, default bitswap timeout).
	Libp2pListen       []string
	PEvOMainLibp2pAddr string
	PEvOMainGatewayURL string
	FallbackGateways   []string
	BitswapTimeout     time.Duration
	MaxPinBytes        int64

	// Community pinner mesh knobs. Cadence and TTL govern heartbeat and
	// known-peers eviction; MeshAdvertiseDisabled flips the pinner to
	// subscribe-only (still consumes the mesh, no publish). MeshEnabled
	// defaults true and gates the entire mesh layer; MeshAllowPrivate
	// relaxes the SSRF guard on heartbeat-advertised gateway URLs for
	// trusted-network meshes (loopback / RFC1918 hosts).
	MeshEnabled           bool
	MeshAllowPrivate      bool
	MeshPublicGatewayURL  string
	MeshHeartbeatInterval time.Duration
	MeshCacheTTL          time.Duration
	MeshAdvertiseDisabled bool
}

func ParseConfig() (*Config, error) {
	// Load .env file (does not override existing env vars)
	loadDotenv(".env")

	cfg := &Config{}

	// Define CLI flags
	hafURL := flag.String("haf-url", "", "PostgreSQL connection string for HAF")
	appTag := flag.String("app-tag", "", "Hive app tag for content discovery")
	ipfsMode := flag.String("ipfs-mode", "", "IPFS backend: embedded or pinata")
	dataDir := flag.String("data-dir", "", "Persistent storage directory")
	port := flag.String("port", "", "Management UI port")
	gatewayPort := flag.String("gateway-port", "", "IPFS gateway port (embedded mode)")
	gatewayBind := flag.String("gateway-bind", "", "IPFS gateway bind IP (default 127.0.0.1; set to 0.0.0.0 for public)")
	refresh := flag.String("refresh", "", "Refresh interval (e.g. 1h, 30m)")
	autoPinConcurrency := flag.String("autopin-concurrency", "", "Max in-flight autopin operations per discovery batch (default 4)")
	autoPinAuthorCap := flag.String("autopin-author-cap", "", "Max CIDs pinned from any single Hive author per discovery batch (default 20)")
	libp2pListen := flag.String("libp2p-listen", "", "Comma-separated libp2p listen multiaddrs (default: libp2p picks)")
	pevoMainLibp2p := flag.String("pevo-main-libp2p-addr", "", "PEvO main libp2p multiaddr (bootstrap peer); empty = DHT-only bootstrap")
	pevoMainGateway := flag.String("pevo-main-gateway-url", "", "PEvO main HTTP gateway URL (first CAR-fetch fallback entry); empty = no PEvO-main entry")
	fallbackGw := flag.String("fallback-gateways", "", "Comma-separated extra CAR-fetch gateway URLs (inserted before the public defaults)")
	bitswapTimeout := flag.String("bitswap-timeout", "", "Per-Pin bitswap fetch timeout (default 60s)")
	maxPinBytes := flag.String("max-pin-bytes", "", "Cap on bytes accepted from any single CAR-fetch response (default 1073741824 = 1 GiB)")
	meshEnabled := flag.String("mesh-enabled", "", "Enable community pinner mesh (default true; set to false to disable)")
	meshAllowPrivate := flag.Bool("mesh-allow-private", false, "Allow mesh-advertised gateway URLs with private/loopback hosts (default false; trusted-network only)")
	meshGateway := flag.String("mesh-public-gateway-url", "", "Public HTTP gateway URL advertised on the mesh (empty = don't share)")
	meshInterval := flag.String("mesh-heartbeat-interval", "", "Mesh heartbeat cadence (default 30s)")
	meshTTL := flag.String("mesh-cache-ttl", "", "Mesh known-peers TTL (default 5m)")
	meshAdvOff := flag.Bool("mesh-advertise-disabled", false, "Suppress mesh heartbeat publish (subscribe-only)")

	flag.Parse()

	// HAF_DATABASE_URL (required)
	cfg.HAFDatabaseURL = envOrFlag("HAF_DATABASE_URL", *hafURL, "")
	if cfg.HAFDatabaseURL == "" {
		fmt.Fprintf(os.Stderr, `pevo-pinner: HAF_DATABASE_URL is required

Usage:
  pevo-pinner [flags]

  Set HAF_DATABASE_URL as an environment variable or pass --haf-url:
    export HAF_DATABASE_URL="postgres://user:pass@host:5432/haf_block_log"
    pevo-pinner

  Or:
    pevo-pinner --haf-url "postgres://user:pass@host:5432/haf_block_log"

Environment variables (CLI flags override):
  HAF_DATABASE_URL     PostgreSQL connection string (required)
  APP_TAG              Hive app tag (default: pevo)
  IPFS_MODE            embedded or pinata (default: embedded)
  DATA_DIR             Storage directory (default: ~/.pevo-pinner)
  PINATA_API_KEY       Pinata API key (required if mode=pinata)
  PINATA_SECRET_KEY    Pinata secret key (required if mode=pinata)
  PORT                 Management UI port (default: 8421)
  GATEWAY_PORT         IPFS gateway port (default: 8080)
  GATEWAY_BIND         IPFS gateway bind IP (default: 127.0.0.1; set 0.0.0.0 for public)
  REFRESH_INTERVAL     Re-query interval (default: 1h)
  AUTOPIN_CONCURRENCY  Max in-flight autopin operations per batch (default: 4)
  AUTOPIN_AUTHOR_CAP   Max CIDs pinned per Hive author per discovery batch (default: 20)
  LIBP2P_LISTEN        Comma-separated libp2p listen multiaddrs (default: libp2p picks)
  PEVO_MAIN_LIBP2P_ADDR  PEvO main libp2p multiaddr (default: empty)
  PEVO_MAIN_GATEWAY_URL  PEvO main HTTP gateway URL for CAR fallback (default: empty)
  FALLBACK_GATEWAYS    Comma-separated extra CAR-fetch gateways
  BITSWAP_TIMEOUT      Per-Pin bitswap timeout duration (default: 60s)
  MAX_PIN_BYTES        Cap on CAR-fetch response bytes (default: 1073741824 = 1 GiB)
  MESH_ENABLED         Enable community pinner mesh (default: true)
  MESH_ALLOW_PRIVATE   Allow mesh-advertised gateway URLs with private/loopback hosts (default: false)
  MESH_PUBLIC_GATEWAY_URL  Gateway URL advertised on the mesh (default: empty)
  MESH_HEARTBEAT_INTERVAL  Mesh heartbeat cadence (default: 30s)
  MESH_CACHE_TTL       Mesh known-peers TTL (default: 5m)
  MESH_ADVERTISE_DISABLED  Subscribe-only mode (default: false)
`)
		return nil, fmt.Errorf("HAF_DATABASE_URL is required")
	}

	cfg.AppTag = envOrFlag("APP_TAG", *appTag, "pevo")
	cfg.IPFSMode = envOrFlag("IPFS_MODE", *ipfsMode, "embedded")
	cfg.PinataAPIKey = envOrFlag("PINATA_API_KEY", "", "")
	cfg.PinataSecretKey = envOrFlag("PINATA_SECRET_KEY", "", "")
	cfg.Port = envOrFlag("PORT", *port, "8421")
	cfg.GatewayPort = envOrFlag("GATEWAY_PORT", *gatewayPort, "8080")
	cfg.GatewayBind = envOrFlag("GATEWAY_BIND", *gatewayBind, "127.0.0.1")

	// DataDir with home expansion
	defaultDataDir := "~/.pevo-pinner"
	rawDataDir := envOrFlag("DATA_DIR", *dataDir, defaultDataDir)
	if strings.HasPrefix(rawDataDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot resolve home directory: %w", err)
		}
		rawDataDir = filepath.Join(home, rawDataDir[2:])
	}
	cfg.DataDir = rawDataDir

	// Refresh interval
	refreshStr := envOrFlag("REFRESH_INTERVAL", *refresh, "1h")
	dur, err := time.ParseDuration(refreshStr)
	if err != nil {
		return nil, fmt.Errorf("invalid refresh interval %q: %w", refreshStr, err)
	}
	cfg.RefreshInterval = dur

	// Autopin concurrency
	concStr := envOrFlag("AUTOPIN_CONCURRENCY", *autoPinConcurrency, "")
	if concStr == "" {
		cfg.AutoPinConcurrency = defaultAutoPinConcurrency
	} else {
		n, err := strconv.Atoi(concStr)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid AUTOPIN_CONCURRENCY %q: must be a positive integer", concStr)
		}
		cfg.AutoPinConcurrency = n
	}

	// Autopin per-author cap
	capStr := envOrFlag("AUTOPIN_AUTHOR_CAP", *autoPinAuthorCap, "")
	if capStr == "" {
		cfg.AutoPinAuthorCap = defaultAutoPinAuthorCap
	} else {
		n, err := strconv.Atoi(capStr)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid AUTOPIN_AUTHOR_CAP %q: must be a positive integer", capStr)
		}
		cfg.AutoPinAuthorCap = n
	}

	// libp2p listen multiaddrs (comma-separated)
	listenStr := envOrFlag("LIBP2P_LISTEN", *libp2pListen, "")
	if listenStr != "" {
		for _, p := range strings.Split(listenStr, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.Libp2pListen = append(cfg.Libp2pListen, p)
			}
		}
	}

	cfg.PEvOMainLibp2pAddr = envOrFlag("PEVO_MAIN_LIBP2P_ADDR", *pevoMainLibp2p, "")
	cfg.PEvOMainGatewayURL = envOrFlag("PEVO_MAIN_GATEWAY_URL", *pevoMainGateway, "")

	if extra := envOrFlag("FALLBACK_GATEWAYS", *fallbackGw, ""); extra != "" {
		for _, p := range strings.Split(extra, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.FallbackGateways = append(cfg.FallbackGateways, p)
			}
		}
	}

	// Bitswap per-Pin timeout
	bswStr := envOrFlag("BITSWAP_TIMEOUT", *bitswapTimeout, "")
	if bswStr != "" {
		d, err := parsePositiveDuration(bswStr)
		if err != nil {
			return nil, fmt.Errorf("invalid BITSWAP_TIMEOUT %q: %w", bswStr, err)
		}
		cfg.BitswapTimeout = d
	}

	// MAX_PIN_BYTES: cap on CAR-fetch response body
	if s := envOrFlag("MAX_PIN_BYTES", *maxPinBytes, ""); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid MAX_PIN_BYTES %q: must be a positive integer", s)
		}
		cfg.MaxPinBytes = n
	}

	// Mesh knobs. MESH_ENABLED defaults true; set to "false"/"0"/"no" to disable.
	cfg.MeshEnabled = true
	if s := envOrFlag("MESH_ENABLED", *meshEnabled, ""); s != "" {
		switch strings.ToLower(s) {
		case "0", "false", "no", "off":
			cfg.MeshEnabled = false
		case "1", "true", "yes", "on":
			cfg.MeshEnabled = true
		default:
			return nil, fmt.Errorf("invalid MESH_ENABLED %q: expected true/false", s)
		}
	}
	cfg.MeshPublicGatewayURL = envOrFlag("MESH_PUBLIC_GATEWAY_URL", *meshGateway, "")
	if s := envOrFlag("MESH_HEARTBEAT_INTERVAL", *meshInterval, ""); s != "" {
		d, err := parsePositiveDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid MESH_HEARTBEAT_INTERVAL %q: %w", s, err)
		}
		cfg.MeshHeartbeatInterval = d
	}
	if s := envOrFlag("MESH_CACHE_TTL", *meshTTL, ""); s != "" {
		d, err := parsePositiveDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid MESH_CACHE_TTL %q: %w", s, err)
		}
		cfg.MeshCacheTTL = d
	}
	// MESH_ADVERTISE_DISABLED bool: any truthy env value (1/true/yes) flips
	// the pinner to subscribe-only. The flag default is false; the env var
	// overrides only when explicitly set, matching the rest of the config
	// surface.
	if v := os.Getenv("MESH_ADVERTISE_DISABLED"); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			cfg.MeshAdvertiseDisabled = true
		}
	} else if *meshAdvOff {
		cfg.MeshAdvertiseDisabled = true
	}
	// MESH_ALLOW_PRIVATE: opt-in for trusted-network meshes.
	if v := os.Getenv("MESH_ALLOW_PRIVATE"); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			cfg.MeshAllowPrivate = true
		}
	} else if *meshAllowPrivate {
		cfg.MeshAllowPrivate = true
	}

	// Validate mode
	if cfg.IPFSMode != "embedded" && cfg.IPFSMode != "pinata" {
		return nil, fmt.Errorf("IPFS_MODE must be 'embedded' or 'pinata', got %q", cfg.IPFSMode)
	}

	// Validate pinata keys
	if cfg.IPFSMode == "pinata" {
		if cfg.PinataAPIKey == "" || cfg.PinataSecretKey == "" {
			return nil, fmt.Errorf("PINATA_API_KEY and PINATA_SECRET_KEY are required when IPFS_MODE=pinata")
		}
	}

	return cfg, nil
}

// parsePositiveDuration wraps time.ParseDuration with a positivity check.
// Returns a clear error rather than wrapping a nil from ParseDuration's
// success path (which would render as `%!w(<nil>)` to operators).
func parsePositiveDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", d)
	}
	return d, nil
}

// envOrFlag returns the CLI flag value if non-empty, else the env var, else the default.
func envOrFlag(envKey, flagVal, defaultVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return defaultVal
}

// LogConfig prints config at startup, redacting passwords.
func (c *Config) LogConfig() {
	redacted := redactURL(c.HAFDatabaseURL)
	fmt.Printf("pevo-pinner starting with config:\n")
	fmt.Printf("  HAF_DATABASE_URL:  %s\n", redacted)
	fmt.Printf("  APP_TAG:           %s\n", c.AppTag)
	fmt.Printf("  IPFS_MODE:         %s\n", c.IPFSMode)
	fmt.Printf("  DATA_DIR:          %s\n", c.DataDir)
	fmt.Printf("  PORT:              %s\n", c.Port)
	fmt.Printf("  GATEWAY_PORT:      %s\n", c.GatewayPort)
	fmt.Printf("  GATEWAY_BIND:      %s\n", c.GatewayBind)
	fmt.Printf("  REFRESH_INTERVAL:  %s\n", c.RefreshInterval)
	fmt.Printf("  AUTOPIN_CONCURRENCY: %d\n", c.AutoPinConcurrency)
	fmt.Printf("  AUTOPIN_AUTHOR_CAP:  %d\n", c.AutoPinAuthorCap)
	if c.IPFSMode == "embedded" {
		if len(c.Libp2pListen) > 0 {
			fmt.Printf("  LIBP2P_LISTEN:     %s\n", strings.Join(c.Libp2pListen, ","))
		} else {
			fmt.Printf("  LIBP2P_LISTEN:     (libp2p default)\n")
		}
		if c.PEvOMainLibp2pAddr != "" {
			fmt.Printf("  PEVO_MAIN_LIBP2P_ADDR: %s\n", c.PEvOMainLibp2pAddr)
		}
		if c.PEvOMainGatewayURL != "" {
			fmt.Printf("  PEVO_MAIN_GATEWAY_URL: %s\n", c.PEvOMainGatewayURL)
		}
		if len(c.FallbackGateways) > 0 {
			fmt.Printf("  FALLBACK_GATEWAYS: %s\n", strings.Join(c.FallbackGateways, ","))
		}
		if c.BitswapTimeout > 0 {
			fmt.Printf("  BITSWAP_TIMEOUT:   %s\n", c.BitswapTimeout)
		} else {
			fmt.Printf("  BITSWAP_TIMEOUT:   (default)\n")
		}
		if c.MaxPinBytes > 0 {
			fmt.Printf("  MAX_PIN_BYTES:     %d\n", c.MaxPinBytes)
		} else {
			fmt.Printf("  MAX_PIN_BYTES:     (default)\n")
		}
		fmt.Printf("  MESH_ENABLED:      %t\n", c.MeshEnabled)
		if c.MeshAllowPrivate {
			fmt.Printf("  MESH_ALLOW_PRIVATE: true\n")
		}
	}
	if c.IPFSMode == "pinata" {
		if len(c.PinataAPIKey) >= 4 {
			fmt.Printf("  PINATA_API_KEY:    %s***\n", c.PinataAPIKey[:4])
		} else {
			fmt.Printf("  PINATA_API_KEY:    ***\n")
		}
	}
}

// loadDotenv reads a .env file and sets env vars that are not already set.
func loadDotenv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env file is fine
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Remove surrounding quotes
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		// Don't override existing env vars
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func redactURL(u string) string {
	// Redact password in postgres://user:pass@host/db
	at := strings.Index(u, "@")
	if at == -1 {
		return u
	}
	prefix := u[:strings.Index(u, "://")+3]
	rest := u[len(prefix):]
	atIdx := strings.Index(rest, "@")
	if atIdx == -1 {
		return u
	}
	userPass := rest[:atIdx]
	after := rest[atIdx:]
	colonIdx := strings.Index(userPass, ":")
	if colonIdx == -1 {
		return u
	}
	return prefix + userPass[:colonIdx] + ":****" + after
}
