// Credential-leak regression test: no static cloud credentials may be
// committed to code, IaC, CI, or scripts (run: go test ./...).
// Mirrors the CI cred-scan guard (.github/workflows/terraform.yml) so the
// invariant holds locally before push. OIDC/federated identity is the only
// accepted AWS auth; long-lived keys are forbidden as ordinary credentials.
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var leakPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"AKIA access key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"secret access key assignment", regexp.MustCompile(`(?i)aws_secret_access_key\s*[:=]`)},
	{"session token assignment", regexp.MustCompile(`(?i)aws_session_token\s*[:=]`)},
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"github token", regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`)},
	{"slack token", regexp.MustCompile(`xox[bap]-[A-Za-z0-9-]+`)},
}

var leakTrees = []string{
	"../../apps", "../../infrastructure", "../../scripts",
	"../../workloads", "../../packages", "../../.github",
}

var leakExts = map[string]bool{
	".go": true, ".tf": true, ".hcl": true, ".tfvars": true,
	".yml": true, ".yaml": true,
	".sh": true, ".json": true, ".py": true, ".sql": true, ".env": true,
}

// TestNoStaticCredentials fails on any committed static credential. The CI
// guard's own detection pattern (a line invoking grep) and this file's
// pattern literals are excluded; everything else is a finding.
func TestNoStaticCredentials(t *testing.T) {
	var findings []string
	for _, tree := range leakTrees {
		root := filepath.Clean(tree)
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if filepath.Base(path) == "security_leak_test.go" {
				return nil
			}
			if !leakExts[strings.ToLower(filepath.Ext(path))] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, "grep ") {
					continue // the CI guard's own detection pattern
				}
				for _, p := range leakPatterns {
					if p.re.MatchString(line) {
						findings = append(findings, p.name+" @ "+path+":"+strconv.Itoa(i+1))
					}
				}
			}
			return nil
		})
	}
	if len(findings) > 0 {
		t.Fatalf("static credentials committed:\n%s", strings.Join(findings, "\n"))
	}
}
