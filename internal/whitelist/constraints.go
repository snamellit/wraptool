package whitelist

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/pti/wraptool/internal/config"
)

func checkConstraints(flags map[string]string, constraints map[string]config.FlagConstraint) error {
	// Check required flags are present.
	for flagName, constraint := range constraints {
		if constraint.Required {
			if _, ok := flags[flagName]; !ok {
				return fmt.Errorf("required flag %q is missing", flagName)
			}
		}
		if constraint.Deny {
			if _, ok := flags[flagName]; ok {
				return fmt.Errorf("flag %q is denied by constraint", flagName)
			}
		}
	}

	// Validate provided flag values against constraints.
	for flagName, value := range flags {
		constraint, ok := constraints[flagName]
		if !ok {
			continue
		}

		if err := validateValue(flagName, value, constraint); err != nil {
			return err
		}
	}

	return nil
}

func validateValue(flagName, value string, c config.FlagConstraint) error {
	// Semantics come from the single ConstraintType.Kind definition, so
	// enforcement can never diverge from how mcptools presents the flag.
	k, ok := c.Type.Kind()
	if !ok {
		// Unreachable for a loaded config (UnmarshalYAML rejects unknown types
		// at decode); kept for values built in-process.
		return fmt.Errorf("flag %q: unknown constraint type %q", flagName, c.Type)
	}
	if k.Valueless {
		// A boolean flag is a bare --flag: no value to type-check or match.
		return nil
	}
	if k.JSONSchema == "integer" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("flag %q: expected integer, got %q", flagName, value)
		}
		if c.Min != nil && n < *c.Min {
			return fmt.Errorf("flag %q: value %d is below minimum %d", flagName, n, *c.Min)
		}
		if c.Max != nil && n > *c.Max {
			return fmt.Errorf("flag %q: value %d exceeds maximum %d", flagName, n, *c.Max)
		}
	}
	// string/untyped (and integer, after range) fall through to the pattern.

	if c.Pattern != "" {
		matched, err := regexp.MatchString(c.Pattern, value)
		if err != nil {
			return fmt.Errorf("flag %q: invalid pattern %q: %w", flagName, c.Pattern, err)
		}
		if !matched {
			return fmt.Errorf("flag %q: value %q does not match pattern %q", flagName, value, c.Pattern)
		}
	}

	return nil
}
