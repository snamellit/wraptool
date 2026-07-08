package executor

import (
	"testing"
)

func TestSanitizeArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"clean args", []string{"status", "--short"}, false},
		{"semicolon injection", []string{"status; rm -rf /"}, true},
		{"pipe injection", []string{"log | cat /etc/passwd"}, true},
		{"backtick injection", []string{"`whoami`"}, true},
		{"dollar injection", []string{"$HOME"}, true},
		{"ampersand injection", []string{"status && rm -rf /"}, true},
		{"newline injection", []string{"status\nrm -rf /"}, true},
		{"clean flag with value", []string{"--message", "fix bug"}, false},
		{"redirect", []string{"> /etc/passwd"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SanitizeArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("SanitizeArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSanitizePositionalArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"clean paths", []string{"file.go", "dir/file.go"}, false},
		{"flag injection", []string{"--force"}, true},
		{"short flag injection", []string{"-rf"}, true},
		{"clean filename", []string{"README.md"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SanitizePositionalArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("SanitizePositionalArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSanitizeFlagValues(t *testing.T) {
	tests := []struct {
		name    string
		flags   map[string]string
		wantErr bool
	}{
		{"clean values", map[string]string{"--output": "json", "--message": "hello world"}, false},
		{"empty value (bare flag)", map[string]string{"--verbose": ""}, false},
		{"path value", map[string]string{"--file": "src/main.go"}, false},
		{"option injection via value", map[string]string{"--verbose": "--upload-pack=/tmp/x"}, true},
		{"short option injection", map[string]string{"--n": "-rf"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SanitizeFlagValues(tt.flags)
			if (err != nil) != tt.wantErr {
				t.Errorf("SanitizeFlagValues() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
