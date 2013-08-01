// +build windows

package falcore

import (
	"net"
)

func (srv *Server) setNoDelay(c net.Conn, noDelay bool) {
	return false
}
