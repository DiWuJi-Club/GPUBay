package config_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/platform/config"
)

var validKey = base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "postgres://x", "SPARK_SECRET_KEY": validKey}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "dev" || c.HTTPAddr != ":8080" || c.Domain != "spark.local" || c.SMSDriver != "mock" ||
		c.KYCDriver != "mock" || c.SweepInterval != time.Minute || len(c.SecretKey) != 32 || c.TrustProxy {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadTrustProxy(t *testing.T) {
	base := func(v string) map[string]string {
		return map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_TRUST_PROXY": v}
	}
	for v, want := range map[string]bool{"true": true, "1": true, "false": false, "0": false, "": false} {
		c, err := config.Load(env(base(v)))
		if err != nil || c.TrustProxy != want {
			t.Fatalf("SPARK_TRUST_PROXY=%q: got %v, err %v, want %v", v, c.TrustProxy, err, want)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing db", map[string]string{"SPARK_SECRET_KEY": validKey}, "SPARK_DATABASE_URL"},
		{"bad key", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": "short"}, "SPARK_SECRET_KEY"},
		{"bad env", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_ENV": "test"}, "SPARK_ENV"},
		{"prod mock", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_ENV": "prod"}, "mock"},
		{"bad interval", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_SWEEP_INTERVAL": "soon"}, "SPARK_SWEEP_INTERVAL"},
		{"bad trust proxy", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_TRUST_PROXY": "yes"}, "SPARK_TRUST_PROXY"},
	}
	for _, c := range cases {
		_, err := config.Load(env(c.env))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %s", c.name, err, c.want)
		}
	}
}
