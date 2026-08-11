package doctor

import (
	"fmt"
	"io"
)

// RenderHuman writes r as the operator-facing plain-text report. cmd passes
// cmd.OutOrStdout(); tests pass a bytes.Buffer and diff a golden file.
func RenderHuman(w io.Writer, r Report) error {
	if err := renderHeader(w, r); err != nil {
		return err
	}

	if r.Incomplete {
		if _, err := fmt.Fprintf(w, "\ndoctor could not complete: %s. Checks after this point did not run.\n",
			r.IncompleteReason); err != nil {
			return err
		}
	}

	if err := renderBody(w, r); err != nil {
		return err
	}

	return renderFooter(w, r)
}

func renderHeader(w io.Writer, r Report) error {
	// exhaustive: names all three ConfigState members.
	switch r.ConfigState {
	case ConfigLoaded:
		_, err := fmt.Fprintf(w, "wraptool doctor — %s\n", r.ConfigPath)
		return err
	case ConfigAbsent:
		if _, err := fmt.Fprintf(w, "wraptool doctor — no config file yet (%s)\n", r.ConfigPath); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, "  info  no config yet; one is created on your first `wraptool serve` or `wraptool up`.")
		return err
	case ConfigUnreadable:
		_, err := fmt.Fprintf(w, "wraptool doctor — %s (unreadable)\n", r.ConfigPath)
		return err
	default:
		_, err := fmt.Fprintf(w, "wraptool doctor — %s\n", r.ConfigPath)
		return err
	}
}

// renderBody partitions r.Findings (already in canonical order) by group and
// prints sections in the fixed groupOrder, so section order does not shuffle
// as findings come and go across runs (§2, "reports diff cleanly").
func renderBody(w io.Writer, r Report) error {
	byGroup := make(map[Group][]Finding, len(groupOrder))
	for _, f := range r.Findings {
		g := f.ID.Group()
		byGroup[g] = append(byGroup[g], f)
	}

	for _, entry := range groupOrder {
		findings := byGroup[entry.Name]
		if len(findings) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "\n%s:\n", entry.Title); err != nil {
			return err
		}
		for _, f := range findings {
			if err := renderFinding(w, f); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderFinding(w io.Writer, f Finding) error {
	label, err := severityLabel(f.Severity)
	if err != nil {
		return err
	}

	line := "  " + label + "  " + string(f.ID) + ": "
	if f.Location != "" {
		line += f.Location + " — "
	}
	line += f.Detail
	if _, err := fmt.Fprintln(w, line); err != nil {
		return err
	}

	if f.Remedy != "" {
		if _, err := fmt.Fprintf(w, "        → %s\n", f.Remedy); err != nil {
			return err
		}
	}
	if f.DocRef != "" {
		if _, err := fmt.Fprintf(w, "        %s\n", f.DocRef); err != nil {
			return err
		}
	}
	return nil
}

// severityLabel renders the fixed-width severity marker. Uppercase only for
// fail, per §8's sample report.
func severityLabel(s Severity) (string, error) {
	// exhaustive: names all three Severity members.
	switch s {
	case SeverityFail:
		return "FAIL", nil
	case SeverityWarn:
		return "warn", nil
	case SeverityInfo:
		return "info", nil
	}
	return "", fmt.Errorf("doctor: invalid severity %q", s)
}

func renderFooter(w io.Writer, r Report) error {
	suppressed := ""
	if r.Summary.Suppressed > 0 {
		suppressed = fmt.Sprintf(" (%d suppressed)", r.Summary.Suppressed)
	}
	_, err := fmt.Fprintf(w, "\n%d failed, %d warnings%s, %d info, %d passed, %d skipped.\n",
		r.Summary.Fail, r.Summary.Warn, suppressed, r.Summary.Info, r.Summary.Pass, r.Summary.Skipped)
	return err
}
