package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all configuration for cloistr-me
type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	Domain   string
	Relays   []string
	LND      LNDConfig
	BTCPay   BTCPayConfig
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
