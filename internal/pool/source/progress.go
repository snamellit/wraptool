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

// downloadProgress reports one artifact download to out: a start line naming
// the artifact and its size, then a byte-count line every progressInterval.
// A nil out disables reporting. It is an io.Writer so it can sit in the
// download's io.MultiWriter and count bytes as they stream past.
type downloadProgress struct {
	out   io.Writer
	label string
	total int64 // from Content-Length; <= 0 when the server did not send one
	done  int64
	start time.Time
	last  time.Time
	now   func() time.Time // test seam; time.Now in production
}

func newDownloadProgress(out io.Writer, label string, total int64) *downloadProgress {
	p := &downloadProgress{out: out, label: label, total: total, now: time.Now}
	p.start = p.now()
	p.last = p.start
	if out != nil {
		if total > 0 {
			fmt.Fprintf(out, "fetching %s (%s)\n", label, formatBytes(total))
		} else {
			fmt.Fprintf(out, "fetching %s\n", label)
		}
	}
	return p
}

func (p *downloadProgress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.out == nil {
		return len(b), nil
	}
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
