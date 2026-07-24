package config

import "net"

// ListenIsLoopback reports whether an sse listen address binds only the
// loopback interface. An empty host or a wildcard bind (0.0.0.0 / ::) is NOT
// loopback — it is reachable off-host and therefore must be authenticated. Used
// both by Validate (to require a token for non-loopback binds) and by `wraptool
// up` (to decide when a bridged container needs a wildcard bind).
func ListenIsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen // no port present; treat the whole value as the host
	}
	switch host {
	case "":
		return false // wildcard bind
	case "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
