package cdc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Security: CDC must never commit, log, or expose secrets. Local/dev
// credentials live in Compose env only; Go code must not print them and the
// connector file must contain only the known local dev password.
func TestNoSecretsInApplierSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, needle := range []string{"AKIA", "AWS_SECRET", "client_secret", "cloudshop-db:5433:cloudshop:cloudshop"} {
			if strings.Contains(s, needle) {
				t.Fatalf("%s leaks %q", e.Name(), needle)
			}
		}
		// TARGET_DATABASE_URL must be read, never logged: fail only on direct
		// logging of the variable value.
		for _, pat := range []string{`log.Print(targetURL`, `log.Printf("%s", targetURL`, `log.Fatal(targetURL`, `_ = targetURL`} {
			_ = pat
		}
		if strings.Contains(s, "log.Print(targetURL") || strings.Contains(s, "log.Printf(\"%v\", targetURL") ||
			strings.Contains(s, "log.Fatal(targetURL") || strings.Contains(s, "log.Println(targetURL") {
			t.Fatalf("%s logs the target URL value", e.Name())
		}
	}
}

func TestConnectorUsesLocalDevCredentialsOnly(t *testing.T) {
	b, err := os.ReadFile("../../debezium/postgres-connector.json")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"database.password": "cloudshop"`) {
		t.Fatal("connector must use the documented local/dev password only")
	}
	for _, needle := range []string{"AKIA", "arn:aws", "DefaultEndpointsProtocol", "AccountKey="} {
		if strings.Contains(s, needle) {
			t.Fatalf("connector leaks cloud material %q", needle)
		}
	}
	if strings.Contains(s, "idempotency_keys") {
		t.Fatal("connector must not capture idempotency_keys")
	}
}
