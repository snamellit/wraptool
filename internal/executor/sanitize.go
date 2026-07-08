package executor

import (
	"fmt"
	"strings"
)

// shellMetachars are characters that could enable shell injection
// when passed as arguments. Since we use exec.CommandContext (no shell),
// these are rejected as an extra safety layer.
const shellMetachars = ";|&$`(){}\\<>\n\r"

// SanitizeArgs validates that arguments don't contain shell metacharacters
// or other dangerous patterns.
func SanitizeArgs(args []string) error {
	for i, arg := range args {
		if err := sanitizeArg(arg); err != nil {
			return fmt.Errorf("argument %d: %w", i, err)
		}
	}
	return nil
}

func sanitizeArg(arg string) error {
	for _, c := range arg {
		if strings.ContainsRune(shellMetachars, c) {
			return fmt.Errorf("contains forbidden character %q", string(c))
		}
	}
	return nil
}

// SanitizePositionalArgs validates positional arguments.
// Positional args must not start with '-' to prevent flag injection.
func SanitizePositionalArgs(args []string) error {
	for i, arg := range args {
		if err := sanitizeArg(arg); err != nil {
			return fmt.Errorf("positional argument %d: %w", i, err)
		}
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("positional argument %d: must not start with '-' (possible flag injection)", i)
		}
	}
	return nil
}

// SanitizeFlagValues rejects flag values that could be reinterpreted as options
// by the wrapped tool. buildArgs emits each flag as two argv tokens (flag, value),
// so a value beginning with '-' — e.g. a valueless flag handed
// "--upload-pack=/tmp/x" — lands as its own option token and bypasses the flag
// allowlist entirely. Mirrors the positional-arg guard above. See the F2 finding
// in design/security-audit-policy-enforcement.md.
func SanitizeFlagValues(flags map[string]string) error {
	for flag, value := range flags {
		if strings.HasPrefix(value, "-") {
			return fmt.Errorf("flag %q value %q must not start with '-' (possible option injection)", flag, value)
		}
	}
	return nil
}
