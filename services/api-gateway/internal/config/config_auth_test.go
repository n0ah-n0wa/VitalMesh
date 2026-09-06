package config

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestJWTSecretIsMandatory(t *testing.T) {
	_, err := Load(func(key string) (string, bool) {
		if key == "DATABASE_URL" {
			return testDatabaseURL, true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "JWT_SECRET: is required") {
		t.Fatalf("Load without JWT_SECRET: err = %v", err)
	}
}

func TestAuthDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	jwt := cfg.Auth.JWT
	if string(jwt.Secret) != testJWTSecret || jwt.KeyID != "1" || jwt.Issuer != "vitalmesh" {
		t.Errorf("JWT identity = %+v", jwt)
	}
	if jwt.TTL != 15*time.Minute || jwt.ClockSkew != 30*time.Second || len(jwt.PreviousSecrets) != 0 {
		t.Errorf("JWT timing = ttl %s skew %s previous %d", jwt.TTL, jwt.ClockSkew, len(jwt.PreviousSecrets))
	}
	if ph := cfg.Auth.Password; ph.MemoryKiB != 64*1024 || ph.Time != 3 || ph.Parallelism != 1 {
		t.Errorf("password hash defaults = %+v", ph)
	}
}

func TestAuthSettings(t *testing.T) {
	previous := strings.Repeat("p", 32)
	cfg, err := Load(lookupFrom(map[string]string{
		"JWT_KEY_ID":                "2026-09",
		"JWT_PREVIOUS_SECRETS":      "2026-03=" + previous + ",old_1=" + strings.Repeat("q", 40),
		"JWT_ISSUER":                "vitalmesh-staging",
		"JWT_TTL":                   "5m",
		"JWT_CLOCK_SKEW":            "10s",
		"PASSWORD_HASH_MEMORY_KIB":  "19456",
		"PASSWORD_HASH_TIME":        "2",
		"PASSWORD_HASH_PARALLELISM": "4",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	jwt := cfg.Auth.JWT
	if jwt.KeyID != "2026-09" || jwt.Issuer != "vitalmesh-staging" || jwt.TTL != 5*time.Minute || jwt.ClockSkew != 10*time.Second {
		t.Errorf("JWT = %+v", jwt)
	}
	if string(jwt.PreviousSecrets["2026-03"]) != previous || len(jwt.PreviousSecrets["old_1"]) != 40 || len(jwt.PreviousSecrets) != 2 {
		t.Errorf("PreviousSecrets = %v", jwt.PreviousSecrets)
	}
	if ph := cfg.Auth.Password; ph.MemoryKiB != 19456 || ph.Time != 2 || ph.Parallelism != 4 {
		t.Errorf("password hash = %+v", ph)
	}
}

func TestLoadPasswordHashAlone(t *testing.T) {
	ph, err := LoadPasswordHash(func(key string) (string, bool) {
		if key == "PASSWORD_HASH_TIME" {
			return "4", true
		}
		return "", false
	})
	if err != nil || ph.Time != 4 || ph.MemoryKiB != 64*1024 {
		t.Fatalf("LoadPasswordHash = %+v, %v", ph, err)
	}
	if _, err := LoadPasswordHash(lookupFrom(map[string]string{"PASSWORD_HASH_MEMORY_KIB": "1"})); err == nil {
		t.Error("LoadPasswordHash accepted a memory cost below the minimum")
	}
}

func TestAuthConfigurationFailures(t *testing.T) {
	cases := map[string]struct {
		values map[string]string
		want   string
	}{
		"short secret":            {map[string]string{"JWT_SECRET": "too-short"}, "JWT_SECRET: must be at least 32 bytes"},
		"key id with spaces":      {map[string]string{"JWT_KEY_ID": "key one"}, "JWT_KEY_ID: must be 1 to 64 characters"},
		"key id too long":         {map[string]string{"JWT_KEY_ID": strings.Repeat("k", 65)}, "JWT_KEY_ID: must be"},
		"issuer with whitespace":  {map[string]string{"JWT_ISSUER": "vital mesh"}, "JWT_ISSUER: must not be empty or contain whitespace"},
		"ttl not a duration":      {map[string]string{"JWT_TTL": "15"}, "JWT_TTL:"},
		"ttl zero":                {map[string]string{"JWT_TTL": "0s"}, "JWT_TTL: must be positive"},
		"ttl above a day":         {map[string]string{"JWT_TTL": "25h"}, "JWT_TTL: must be at most 24h0m0s"},
		"skew above bound":        {map[string]string{"JWT_CLOCK_SKEW": "10m"}, "JWT_CLOCK_SKEW: must be at most 5m0s"},
		"previous without equals": {map[string]string{"JWT_PREVIOUS_SECRETS": "abc"}, "JWT_PREVIOUS_SECRETS: entries must be of the form kid=secret"},
		"previous bad key id":     {map[string]string{"JWT_PREVIOUS_SECRETS": "bad id=" + strings.Repeat("x", 32)}, "JWT_PREVIOUS_SECRETS: key id must be"},
		"previous short secret":   {map[string]string{"JWT_PREVIOUS_SECRETS": "old=short"}, `JWT_PREVIOUS_SECRETS: secret for key id "old" must be at least 32 bytes`},
		"previous equals active":  {map[string]string{"JWT_PREVIOUS_SECRETS": "1=" + strings.Repeat("x", 32)}, `JWT_PREVIOUS_SECRETS: key id "1" is the active JWT_KEY_ID`},
		"previous duplicate":      {map[string]string{"JWT_PREVIOUS_SECRETS": "a=" + strings.Repeat("x", 32) + ",a=" + strings.Repeat("y", 32)}, `JWT_PREVIOUS_SECRETS: key id "a" is listed twice`},
		"memory below minimum":    {map[string]string{"PASSWORD_HASH_MEMORY_KIB": "1024"}, "PASSWORD_HASH_MEMORY_KIB: must be between 8192 and 1048576"},
		"memory above maximum":    {map[string]string{"PASSWORD_HASH_MEMORY_KIB": "2097152"}, "PASSWORD_HASH_MEMORY_KIB: must be between"},
		"memory not a number":     {map[string]string{"PASSWORD_HASH_MEMORY_KIB": "lots"}, "PASSWORD_HASH_MEMORY_KIB: must be a positive integer"},
		"time zero":               {map[string]string{"PASSWORD_HASH_TIME": "0"}, "PASSWORD_HASH_TIME: must be positive"},
		"time above maximum":      {map[string]string{"PASSWORD_HASH_TIME": "101"}, "PASSWORD_HASH_TIME: must be at most 100"},
		"parallelism above max":   {map[string]string{"PASSWORD_HASH_PARALLELISM": "65"}, "PASSWORD_HASH_PARALLELISM: must be at most 64"},
		"parallelism negative":    {map[string]string{"PASSWORD_HASH_PARALLELISM": "-1"}, "PASSWORD_HASH_PARALLELISM: must be a positive integer"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(tc.values))
			if err == nil {
				t.Fatalf("Load accepted %v", tc.values)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestAuthFailuresAreReportedTogether(t *testing.T) {
	_, err := Load(lookupFrom(map[string]string{
		"JWT_SECRET":               "short",
		"JWT_TTL":                  "48h",
		"PASSWORD_HASH_MEMORY_KIB": "1",
	}))
	if err == nil {
		t.Fatal("Load accepted invalid auth configuration")
	}
	for _, want := range []string{"JWT_SECRET", "JWT_TTL", "PASSWORD_HASH_MEMORY_KIB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestSecretsNeverAppearInErrors(t *testing.T) {
	_, err := Load(lookupFrom(map[string]string{
		"JWT_PREVIOUS_SECRETS": "old=" + strings.Repeat("S3cr3t", 6) + ",old=" + strings.Repeat("S3cr3t", 6),
	}))
	if err == nil {
		t.Fatal("Load accepted duplicate key ids")
	}
	if strings.Contains(err.Error(), "S3cr3t") {
		t.Errorf("error message contains a secret: %v", err)
	}
}

func TestPasswordHashMaxConcurrent(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil || cfg.Auth.Password.MaxConcurrent != 4 {
		t.Fatalf("default MaxConcurrent = %d, %v; want 4", cfg.Auth.Password.MaxConcurrent, err)
	}
	cfg, err = Load(lookupFrom(map[string]string{"PASSWORD_HASH_MAX_CONCURRENT": "16"}))
	if err != nil || cfg.Auth.Password.MaxConcurrent != 16 {
		t.Errorf("MaxConcurrent = %d, %v; want 16", cfg.Auth.Password.MaxConcurrent, err)
	}
	for value, want := range map[string]string{
		"0":    "PASSWORD_HASH_MAX_CONCURRENT: must be positive",
		"2000": "PASSWORD_HASH_MAX_CONCURRENT: must be at most 1024",
		"many": "PASSWORD_HASH_MAX_CONCURRENT: must be a positive integer",
	} {
		_, err := Load(lookupFrom(map[string]string{"PASSWORD_HASH_MAX_CONCURRENT": value}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", value, err, want)
		}
	}
}

func TestDeployedEnvironmentsRequireDatabaseTLS(t *testing.T) {
	plaintext := []string{
		"postgres://u:p@db:5432/vm?sslmode=disable",
		"postgres://u:p@db:5432/vm?sslmode=prefer",
		"postgres://u:p@db:5432/vm?sslmode=allow",
		"postgres://u:p@db:5432/vm",
		"host=db user=u password=p dbname=vm sslmode=disable",
		"host=db user=u password=p dbname=vm",
	}
	encrypted := []string{
		"postgres://u:p@db:5432/vm?sslmode=require",
		"postgresql://u:p@db:5432/vm?sslmode=verify-ca",
		"postgres://u:p@db:5432/vm?sslmode=verify-full&application_name=x",
		"host=db user=u password=p dbname=vm sslmode=verify-full",
		"host=db sslmode='require' user=u",
	}
	for _, env := range []string{"staging", "production"} {
		for _, dsn := range plaintext {
			_, err := Load(lookupFrom(map[string]string{"ENVIRONMENT": env, "DATABASE_URL": dsn}))
			if err == nil || !strings.Contains(err.Error(), "DATABASE_URL: sslmode must be require, verify-ca or verify-full in "+env) {
				t.Errorf("%s %q: err = %v, want a TLS requirement", env, dsn, err)
			}
			if err != nil && strings.Contains(err.Error(), "u:p@") {
				t.Errorf("%s: error message contains database credentials: %v", env, err)
			}
		}
		for _, dsn := range encrypted {
			if _, err := Load(lookupFrom(map[string]string{"ENVIRONMENT": env, "DATABASE_URL": dsn})); err != nil {
				t.Errorf("%s %q: unexpected error %v", env, dsn, err)
			}
		}
	}
	for _, env := range []string{"local", "test"} {
		if _, err := Load(lookupFrom(map[string]string{"ENVIRONMENT": env, "DATABASE_URL": plaintext[0]})); err != nil {
			t.Errorf("%s permits plaintext for development: %v", env, err)
		}
	}
}

func TestSecretsAreRedactedByEveryFormatter(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{"JWT_PREVIOUS_SECRETS": "old=" + strings.Repeat("Prev1ous", 4)}))
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("cfg", "jwt", cfg.Auth.JWT, "secret", cfg.Auth.JWT.Secret)
	encoded, _ := json.Marshal(cfg)

	dumps := map[string]string{
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg.Auth),
		"%#v":  fmt.Sprintf("%#v", cfg.Auth.JWT),
		"%s":   fmt.Sprintf("%s", cfg.Auth.JWT.Secret),
		"%q":   fmt.Sprintf("%q", cfg.Auth.JWT.PreviousSecrets),
		"%x":   fmt.Sprintf("%x", cfg.Auth.JWT.Secret),
		"slog": logs.String(),
		"json": string(encoded),
	}
	for name, dump := range dumps {
		if strings.Contains(dump, testJWTSecret) || strings.Contains(dump, "Prev1ous") || strings.Contains(dump, hex.EncodeToString([]byte(testJWTSecret))) {
			t.Errorf("%s dump contains a secret: %s", name, dump)
		}
		// %x hex-encodes the placeholder, which is still not the secret.
		if name != "%x" && !strings.Contains(dump, "[redacted]") {
			t.Errorf("%s dump does not show the placeholder: %s", name, dump)
		}
	}
	if string(cfg.Auth.JWT.Secret) != testJWTSecret {
		t.Error("the raw bytes are no longer available for signing")
	}
}
