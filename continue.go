package falcore

import (
	"io"
	"net"
	"net/http"
)

type continueReader struct {
	req    *http.Request
	r      io.ReadCloser
	conn   net.Conn
	opened bool
}

var _ io.ReadCloser = new(continueReader)

func (r *continueReader) Read(p []byte) (int, error) {
	// sent 100 continue the first time we try to read the body
	if !r.opened {
		resp := SimpleResponse(r.req, 100, nil, 0, nil)
		resp.Write(r.conn)
		r.req = nil
	}
	return r.r.Read(p)
}

func (r *continueReader) Close() error {
	r.req = nil
	return r.r.Close()
}
