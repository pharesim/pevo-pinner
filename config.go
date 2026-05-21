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

// defaultMaxPinBytes caps a single gateway fetch at 256 MiB — well above any
// realistic paper PDF or supplementary archive, well below disk-fill territory.
const defaultMaxPinBytes int64 = 256 << 20

type Config struct {
	HAFDatabaseURL  string
	AppTag          string
	IPFSMode        string
	DataDir         string
	PinataAPIKey    string
	PinataSecretKey string
	Port            string
	GatewayPort     string
	RefreshInterval time.Duration
	MaxPinBytes     int64
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
	refresh := flag.String("refresh", "", "Refresh interval (e.g. 1h, 30m)")
	maxPinBytes := flag.String("max-pin-bytes", "", "Maximum bytes copied from a single gateway fetch (default 256 MiB)")

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
  REFRESH_INTERVAL     Re-query interval (default: 1h)
  MAX_PIN_BYTES        Max bytes per gateway fetch (default: 268435456 = 256 MiB)
`)
		return nil, fmt.Errorf("HAF_DATABASE_URL is required")
	}

	cfg.AppTag = envOrFlag("APP_TAG", *appTag, "pevo")
	cfg.IPFSMode = envOrFlag("IPFS_MODE", *ipfsMode, "embedded")
	cfg.PinataAPIKey = envOrFlag("PINATA_API_KEY", "", "")
	cfg.PinataSecretKey = envOrFlag("PINATA_SECRET_KEY", "", "")
	cfg.Port = envOrFlag("PORT", *port, "8421")
	cfg.GatewayPort = envOrFlag("GATEWAY_PORT", *gatewayPort, "8080")

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

	// Max pin bytes
	maxStr := envOrFlag("MAX_PIN_BYTES", *maxPinBytes, "")
	if maxStr == "" {
		cfg.MaxPinBytes = defaultMaxPinBytes
	} else {
		n, err := strconv.ParseInt(maxStr, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid MAX_PIN_BYTES %q: must be a positive integer (bytes)", maxStr)
		}
		cfg.MaxPinBytes = n
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
	fmt.Printf("  REFRESH_INTERVAL:  %s\n", c.RefreshInterval)
	fmt.Printf("  MAX_PIN_BYTES:     %d (%.0f MiB)\n", c.MaxPinBytes, float64(c.MaxPinBytes)/float64(1<<20))
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
