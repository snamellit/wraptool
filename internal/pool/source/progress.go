package source

import (
	"fmt"
	"io"
	"time"
)

// progressInterval is how often a running download reports its byte count.
// Downloads that finish sooner print only their start line, so small npm
// tarballs stay one line each.
const progressInterval = 5 * time.Second

// ProgressReporter is implemented by Sources that can report artifact
// downloads (NPM, Archive). cmd attaches the command's stderr through it.
type ProgressReporter interface {
	SetProgress(w io.Writer)
}

var (
	_ ProgressReporter = (*NPM)(nil)
	_ ProgressReporter = (*Archive)(nil)
)

// downloadProgress reports one artifact download to out: a start line
// naming the artifact and its size, then a byte-count line every
// progressInterval. It is an io.Writer so it can sit in the download's
// io.MultiWriter and count bytes as they stream past.
type downloadProgress struct {
	out   io.Writer
	label string
	total int64 // from Content-Length; <= 0 when the server did not send one
	done  int64
	start time.Time
	last  time.Time
	now   func() time.Time // test seam; time.Now in production
}

// newDownloadProgress prints the start line and returns the byte counter
// for one download; a nil out disables reporting (io.Discard).
func newDownloadProgress(out io.Writer, label string, total int64) io.Writer {
	if out == nil {
		return io.Discard
	}
	return startDownloadProgress(out, label, total, time.Now)
}

func startDownloadProgress(out io.Writer, label string, total int64, now func() time.Time) *downloadProgress {
	t := now()
	if total > 0 {
		fmt.Fprintf(out, "fetching %s (%s)\n", label, formatBytes(total))
	} else {
		fmt.Fprintf(out, "fetching %s\n", label)
	}
	return &downloadProgress{out: out, label: label, total: total, start: t, last: t, now: now}
}

func (p *downloadProgress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if t := p.now(); t.Sub(p.last) >= progressInterval {
		p.last = t
		rate := float64(p.done) / t.Sub(p.start).Seconds()
		if p.total > 0 {
			fmt.Fprintf(p.out, "  %s: %s / %s (%d%%, %s/s)\n", p.label,
				formatBytes(p.done), formatBytes(p.total), p.done*100/p.total, formatBytes(int64(rate)))
		} else {
			fmt.Fprintf(p.out, "  %s: %s (%s/s)\n", p.label, formatBytes(p.done), formatBytes(int64(rate)))
		}
	}
	return len(b), nil
}

// formatBytes renders n with a binary-prefixed unit and one decimal.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
