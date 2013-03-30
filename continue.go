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
		if err := resp.Write(r.conn); err != nil {
			return 0, err
		}
		r.req = nil
		r.opened = true
	}
	return r.r.Read(p)
}

func (r *continueReader) Close() error {
	r.req = nil
	return r.r.Close()
}
