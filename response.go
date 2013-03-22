package falcore

import (
	"bytes"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	// "encoding/json"
)

func SimpleResponse(req *http.Request, status int, headers http.Header, contentLength int64, body io.Reader) *http.Response {
	res := new(http.Response)
	res.StatusCode = status
	res.ProtoMajor = 1
	res.ProtoMinor = 1
	res.ContentLength = contentLength
	res.Request = req
	res.Header = make(map[string][]string)
	if body_rdr, ok := body.(io.ReadCloser); ok {
		res.Body = body_rdr
	} else {
		res.Body = ioutil.NopCloser(body)
	}
	if headers != nil {
		res.Header = headers
	}
	return res
}

func ByteResponse(req *http.Request, status int, headers http.Header, body []byte) *http.Response {
	return SimpleResponse(req, status, headers, int64(len(body)), bytes.NewBuffer(body))
}

func StringResponse(req *http.Request, status int, headers http.Header, body string) *http.Response {
	return SimpleResponse(req, status, headers, int64(len(body)), strings.NewReader(body))
}

func RedirectResponse(req *http.Request, url string) *http.Response {
	h := make(http.Header)
	h.Set("Location", url)
	return SimpleResponse(req, 302, h, 0, nil)
}

// func JSONResponse(req *http.Request, status int, headers http.Header, body interface{}) *http.Response {
// 	// TODO
// }
