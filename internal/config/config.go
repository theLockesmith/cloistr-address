package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all configuration for cloistr-me
type Config struct {
	Server        ServerConfig
	Database      DatabaseConfig
	Domain        string
	Relays        []string
	LND           LNDConfig
	BTCPay        BTCPayConfig
	InternalAPI   InternalAPIConfig
	EmailInternal EmailInternalConfig
	NWC           NWCConfig
}

// EmailInternalConfig points at cloistr-email's internal domain-admin API.
//
// This is cloistr-email's OWN inbound secret, not a reuse of our InternalAPI.Secret:
// each service has its own. Both values are required for the /admin/v1/domains
// proxy to register; with either missing the routes stay off.
type EmailInternalConfig struct {
	URL    string // e.g. http://cloistr-email.cloistr.svc.cluster.local:8080
	Secret string // Bearer token for cloistr-email's /internal/v1/domains/*
}

// NWCConfig holds Nostr Wallet Connect configuration
type NWCConfig struct {
	EncryptionKey string // 32-byte hex key for encrypting NWC secrets at rest
}

// InternalAPIConfig holds internal API configuration
type InternalAPIConfig struct {
	Secret string // Shared secret for internal service-to-service calls
}

// BTCPayConfig holds BTCPay Server configuration
type BTCPayConfig struct {
	URL           string
	APIKey        string
	StoreID       string
	WebhookSecret string // For verifying webhook signatures
}

// ServerConfig holds HTTP server configuration
type ServerConfig struct {
	Address string
}

// DatabaseConfig holds PostgreSQL connection configuration
type DatabaseConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string
}

// LNDConfig holds LND REST API configuration (for payment processing)
type LNDConfig struct {
	Host         string
	MacaroonPath string
	TLSCertPath  string
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Address: getEnv("SERVER_ADDRESS", ":8080"),
		},
		Database: DatabaseConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnvInt("DB_PORT", 5432),
			User:     getEnv("DB_USER", "cloistr"),
			Password: getEnv("DB_PASSWORD", ""),
			Database: getEnv("DB_NAME", "cloistr"),
			SSLMode:  getEnv("DB_SSLMODE", "require"),
		},
		Domain: getEnv("DOMAIN", "cloistr.xyz"),
		Relays: getEnvSlice("DEFAULT_RELAYS", []string{"wss://relay.cloistr.xyz"}),
		LND: LNDConfig{
			Host:         getEnv("LND_REST_HOST", ""),
			MacaroonPath: getEnv("LND_MACAROON_PATH", ""),
			TLSCertPath:  getEnv("LND_TLS_CERT_PATH", ""),
		},
		BTCPay: BTCPayConfig{
			URL:           getEnv("BTCPAY_URL", ""),
			APIKey:        getEnv("BTCPAY_API_KEY", ""),
			StoreID:       getEnv("BTCPAY_STORE_ID", ""),
			WebhookSecret: getEnv("BTCPAY_WEBHOOK_SECRET", ""),
		},
		InternalAPI: InternalAPIConfig{
			Secret: getEnv("INTERNAL_API_SECRET", ""),
		},
		EmailInternal: EmailInternalConfig{
			URL:    strings.TrimSuffix(getEnv("EMAIL_INTERNAL_URL", ""), "/"),
			Secret: getEnv("EMAIL_INTERNAL_SECRET", ""),
		},
		NWC: NWCConfig{
			EncryptionKey: getEnv("NWC_ENCRYPTION_KEY", ""),
		},
	}

	// Validate required fields
	if cfg.Database.Password == "" {
		return nil, fmt.Errorf("DB_PASSWORD is required")
	}

	return cfg, nil
}

// ConnectionString returns the PostgreSQL connection string
func (c *DatabaseConfig) ConnectionString() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, c.Port, c.User, c.Password, c.Database, c.SSLMode,
	)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvSlice(key string, defaultValue []string) []string {
	if value := os.Getenv(key); value != "" {
		return strings.Split(value, ",")
	}
	return defaultValue
}
