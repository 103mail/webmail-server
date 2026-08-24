// Package config reads the server configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the server's runtime configuration.
type Config struct {
	// Addr is the listen address, for example ":8080".
	Addr string
	// DSN is the MySQL connection string.
	DSN string
	// DataDir holds attachment and document bytes. It must be the same
	// directory the PHP backend uses while both run against one database.
	DataDir string
	// AllowedOrigins are the browser origins permitted by CORS.
	AllowedOrigins []string
	// MaxBodyBytes caps a request body, which bounds attachment uploads.
	MaxBodyBytes int64
	// SESRegion is the AWS region SES receives in and the bucket its receipt
	// rules write to lives in (ADR 15). Leaving it empty disables the inbound
	// webhook, which is what a development machine wants.
	SESRegion string
	// InboundTopicARNs are the SNS topics inbound mail is accepted from. An
	// empty list accepts any topic, which is only safe when the inbound
	// endpoint is unreachable from the internet.
	InboundTopicARNs []string
	// OutboundTopicARN is the SNS topic notification email is published to
	// (ADR 15). Leaving it empty keeps outbound mail in memory instead of
	// publishing it, so a development or staging deployment cannot mail real
	// people.
	OutboundTopicARN string
	// SNSRegion is the region that topic lives in. Empty means SESRegion.
	SNSRegion string
}

// Load reads the configuration, applying defaults suited to development.
func Load() (*Config, error) {
	c := &Config{
		Addr:    env("WEBMAIL_ADDR", ":8080"),
		DSN:     env("WEBMAIL_DSN", "webmail:webmail@tcp(127.0.0.1:3306)/webmail"),
		DataDir: env("WEBMAIL_DATA_DIR", "data"),
		AllowedOrigins: strings.Split(env("WEBMAIL_ALLOWED_ORIGINS",
			"http://localhost:4000,http://localhost:8000,http://localhost:5173,"+
				"https://103mail.com,https://www.103mail.com"), ","),
		MaxBodyBytes:     32 << 20,
		SESRegion:        os.Getenv("WEBMAIL_SES_REGION"),
		OutboundTopicARN: os.Getenv("WEBMAIL_SNS_TOPIC_ARN"),
		SNSRegion:        os.Getenv("WEBMAIL_SNS_REGION"),
	}
	if c.SNSRegion == "" {
		c.SNSRegion = c.SESRegion
	}
	c.InboundTopicARNs = splitList(os.Getenv("WEBMAIL_SES_INBOUND_TOPIC_ARNS"))
	if v := os.Getenv("WEBMAIL_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("WEBMAIL_MAX_BODY_BYTES must be a positive integer, got %q", v)
		}
		c.MaxBodyBytes = n
	}
	for i := range c.AllowedOrigins {
		c.AllowedOrigins[i] = strings.TrimSpace(c.AllowedOrigins[i])
	}
	return c, nil
}

// splitList reads a comma-separated setting, dropping empty entries.
func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
