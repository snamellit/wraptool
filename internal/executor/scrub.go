package executor

import (
	"regexp"
	"strings"
)

// credentialPatterns matches common credential patterns that should be
// scrubbed from command output before returning to the AI context.
var credentialPatterns = []*regexp.Regexp{
	// Bearer/Basic auth tokens.
	regexp.MustCompile(`(?i)(Bearer|Basic)\s+[A-Za-z0-9_\-./+=]{8,}`),
	// Generic key=value with sensitive key names.
	regexp.MustCompile(`(?i)(api[_-]?key|api[_-]?secret|auth[_-]?token|access[_-]?token|secret[_-]?key|private[_-]?key|password|passwd|credential|client[_-]?secret)\s*[=:]\s*\S+`),
	// AWS access keys (AKIA...).
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// AWS secret keys (40 char base64).
	regexp.MustCompile(`(?i)aws[_-]?secret[_-]?access[_-]?key\s*[=:]\s*\S+`),
	// GCP service account key fragments.
	regexp.MustCompile(`"private_key"\s*:\s*"-----BEGIN[^"]+"`),
	// Generic long base64 strings that look like tokens (40+ chars).
	regexp.MustCompile(`(?i)(token|key|secret|password)\s*[=:"]\s*[A-Za-z0-9_\-./+=]{40,}`),
	// SSH private key markers.
	regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----`),
	// Environment variable assignments with sensitive names.
	regexp.MustCompile(`(?i)(API_KEY|SECRET|TOKEN|PASSWORD|CREDENTIAL|AUTH)[A-Z_]*=[^\s]+`),
}

const redacted = "[REDACTED]"

// ScrubCredentials removes potential credentials from command output.
func ScrubCredentials(output string) string {
	for _, pattern := range credentialPatterns {
		output = pattern.ReplaceAllString(output, redacted)
	}
	return output
}

// ScrubEnvVars removes specific environment variable values from output.
// This is used to scrub the tool's configured env vars from output.
func ScrubEnvVars(output string, envVars map[string]string) string {
	for _, value := range envVars {
		if len(value) >= 8 { // only scrub non-trivial values
			output = strings.ReplaceAll(output, value, redacted)
		}
	}
	return output
}
