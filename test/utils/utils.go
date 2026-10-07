// Package utils provides credential-safe subprocess support for test suites.
package utils

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/onsi/ginkgo/v2"
)

const connectionValuePattern = `(?:'[^']*'|"[^"]*"|[^\s;]+)`
const keywordConnectionPattern = `(?i)\b(?:host|hostaddr|port|dbname|user|password|sslmode|sslrootcert)` +
	`\s*=\s*` + connectionValuePattern + `(?:\s+[A-Za-z_]+\s*=\s*` + connectionValuePattern + `)+`

var credentialName = regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9-]*$`)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(keywordConnectionPattern),
	regexp.MustCompile(`(?i)bearer\s+[^\s"'<>]+`),
	regexp.MustCompile(`(?i)postgres(?:ql)?://[^\s"'<>]+`),
	regexp.MustCompile(`(?i)(?:password|passwd|pwd|token|secret|dsn|authorization)\s*[=:]\s*(?:'[^']*'|"[^"]*"|[^\s,;]+)`),
	regexp.MustCompile(`(?i)--(?:password|token|secret|dsn)\s+[^\s]+`),
	regexp.MustCompile(`(?i)\bpassword\s+'[^']*'`),
	regexp.MustCompile(`(?i)https?://[^\s/:]+:[^\s/@]+@[^\s]+`),
}

// Redact removes connection strings and common credential formats from diagnostics.
func Redact(value string) string {
	for _, pattern := range credentialPatterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}

// Run preserves the command's directory and environment and sanitizes diagnostics.
// An unset environment inherits the current process environment through os/exec.
func Run(cmd *exec.Cmd) (string, error) {
	_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "running: %s\n", filepath.Base(cmd.Path))
	output, err := cmd.CombinedOutput()
	sanitized := sanitize(string(output), cmd)
	if err != nil {
		return sanitized, fmt.Errorf("%s failed: %s: %w", filepath.Base(cmd.Path), sanitized, err)
	}
	return sanitized, nil
}

func sanitize(value string, cmd *exec.Cmd) string {
	environment := cmd.Env
	if environment == nil {
		environment = os.Environ()
	}
	for _, entry := range environment {
		key, secret, ok := strings.Cut(entry, "=")
		if ok && secret != "" && sensitiveKey(key) {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	for index, argument := range cmd.Args {
		key, secret, assigned := strings.Cut(argument, "=")
		if !assigned && !strings.HasPrefix(key, "-") {
			continue
		}
		key = strings.TrimLeft(key, "-")
		if !credentialName.MatchString(key) || !sensitiveKey(key) {
			continue
		}
		if !assigned && index+1 < len(cmd.Args) {
			secret = cmd.Args[index+1]
		}
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return Redact(value)
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "password") || strings.Contains(key, "token") ||
		strings.Contains(key, "secret") || strings.Contains(key, "dsn") || key == "passwd" || key == "pwd"
}

// GetProjectDir finds the module root without changing the process directory.
func GetProjectDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("module root not found")
		}
		dir = parent
	}
}
