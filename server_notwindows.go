// +build !windows
// +build !go1.1

package falcore

import (
	"net"
	"runtime"
	"syscall"
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
	if srv.sendfile {
		if e := syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, srv.sockOpt, 1); e != nil {
			return e
		}
	}
	return nil
}

// Backwards support for go1.0
// Go1.1 does not require special code for this
func (srv *Server) cycleNonBlock(c net.Conn) {
	if srv.sendfile {
		if tcpC, ok := c.(*net.TCPConn); ok {
			if f, err := tcpC.File(); err == nil {
				// f is a copy.  must be closed
				defer f.Close()
				fd := int(f.Fd())
				// Disable TCP_CORK/TCP_NOPUSH
				syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, srv.sockOpt, 0)
				// For TCP_NOPUSH, we need to force flush
				c.Write([]byte{})
				// Re-enable TCP_CORK/TCP_NOPUSH
				syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, srv.sockOpt, 1)
			}
		}
	}
}

func (s *Server) setupNonBlockOptions() {
	// openbsd/netbsd don't have TCP_NOPUSH so it's likely sendfile will be slower
	// without these socket options, just enable for linux, mac and freebsd.
	// TODO (Graham) windows has TransmitFile zero-copy mechanism, try to use it
	switch runtime.GOOS {
	case "linux":
		s.sendfile = true
		s.sockOpt = 0x3 // syscall.TCP_CORK
	case "freebsd", "darwin":
		s.sendfile = true
		s.sockOpt = 0x4 // syscall.TCP_NOPUSH
	default:
		s.sendfile = false
	}
}
