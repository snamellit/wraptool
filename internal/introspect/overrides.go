package introspect

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// OverrideFile is the structure of a manual override YAML file.
type OverrideFile struct {
	Subcommands map[string]OverrideSubcommand `yaml:"subcommands"`
}

// OverrideSubcommand describes manual overrides for a subcommand.
type OverrideSubcommand struct {
	Description string                  `yaml:"description"`
	Flags       map[string]OverrideFlag `yaml:"flags"`
}

// OverrideFlag describes a manual override for a flag.
type OverrideFlag struct {
	Short       string `yaml:"short"`
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	TakesValue  *bool  `yaml:"takes_value"`
}

// mergeOverrides loads an override file and deep-merges it into the schema.
// Override values always win over introspected values.
func mergeOverrides(schema *Schema, overridesFile string) error {
	data, err := os.ReadFile(overridesFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading overrides file: %w", err)
	}

	var overrides OverrideFile
	if err := yaml.Unmarshal(data, &overrides); err != nil {
		return fmt.Errorf("parsing overrides file: %w", err)
	}

	for subcmdKey, override := range overrides.Subcommands {
		existing, ok := schema.Subcommands[subcmdKey]
		if !ok {
			existing = Subcommand{
				Flags: make(map[string]Flag),
			}
		}

		if override.Description != "" {
			existing.Description = override.Description
		}

		for flagName, of := range override.Flags {
			flag, exists := existing.Flags[flagName]
			if !exists {
				flag = Flag{}
			}

			if of.Short != "" {
				flag.Short = of.Short
			}
			if of.Type != "" {
				flag.Type = of.Type
			}
			if of.Description != "" {
				flag.Description = of.Description
			}
			if of.TakesValue != nil {
				flag.TakesValue = *of.TakesValue
			}

			existing.Flags[flagName] = flag
		}

		schema.Subcommands[subcmdKey] = existing
	}

	return nil
}
