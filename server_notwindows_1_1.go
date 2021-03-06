// +build !windows
// +build go1.1

package falcore

import (
	"net"
	"runtime"
)

// Used NoDelay (Nagle's algorithm) where available
func (srv *Server) setNoDelay(c net.Conn, noDelay bool) bool {
	switch runtime.GOOS {
	case "linux", "freebsd", "darwin":
		if tcpC, ok := c.(*net.TCPConn); ok {
			if noDelay {
				// Disable TCP_CORK/TCP_NOPUSH
				tcpC.SetNoDelay(true)
				// For TCP_NOPUSH, we need to force flush
				c.Write([]byte{})
			} else {
				// Re-enable TCP_CORK/TCP_NOPUSH
				tcpC.SetNoDelay(false)
			}
		}
		return true
	default:
		return false
	}
}
