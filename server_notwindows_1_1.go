// +build !windows
// +build go1.1

package falcore

import (
	"net"
	"syscall"
	"runtime"
)

// only valid on non-windows
func (srv *Server) setupNonBlockingListener(err error, l *net.TCPListener) error {
	// FIXME: File() returns a copied pointer.  we're leaking it.  probably doesn't matter
	if srv.listenerFile, err = l.File(); err != nil {
		return err
	}
	fd := int(srv.listenerFile.Fd())
	if e := syscall.SetNonblock(fd, true); e != nil {
		return e
	}
	return nil
}


func (srv *Server) cycleNonBlock(c net.Conn) {
	if srv.sendfile {
		if tcpC, ok := c.(*net.TCPConn); ok {
			// Disable TCP_CORK/TCP_NOPUSH
			tcpC.SetNoDelay(true)
			// For TCP_NOPUSH, we need to force flush
			c.Write([]byte{})
			// Re-enable TCP_CORK/TCP_NOPUSH
			tcpC.SetNoDelay(false)
		}
	}
}

func (s *Server) setupNonBlockOptions() {
	switch runtime.GOOS {
	case "linux", "freebsd", "darwin":
		s.sendfile = true
	default:
		s.sendfile = false
	}
}