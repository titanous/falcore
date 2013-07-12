package falcore

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Server struct {
	Addr               string
	Pipeline           *Pipeline
	CompletionCallback RequestCompletionCallback
	listener           net.Listener
	listenerFile       *os.File
	stopAccepting      chan int
	handlerWaitGroup   *sync.WaitGroup
	logPrefix          string
	AcceptReady        chan int
	sendfile           bool
	sockOpt            int
	bufferPool         *BufferPool
}

type RequestCompletionCallback func(req *Request, res *http.Response)

func NewServer(port int, pipeline *Pipeline) *Server {
	s := new(Server)
	s.Addr = fmt.Sprintf(":%v", port)
	s.Pipeline = pipeline
	s.stopAccepting = make(chan int)
	s.AcceptReady = make(chan int, 1)
	s.handlerWaitGroup = new(sync.WaitGroup)
	s.logPrefix = fmt.Sprintf("%d", syscall.Getpid())

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

	// buffer pool for reusing connection bufio.Readers
	s.bufferPool = NewBufferPool(100, 8192)

	return s
}

func (srv *Server) FdListen(fd int) error {
	var err error
	srv.listenerFile = os.NewFile(uintptr(fd), "")
	if srv.listener, err = net.FileListener(srv.listenerFile); err != nil {
		return err
	}
	if _, ok := srv.listener.(*net.TCPListener); !ok {
		return errors.New("Broken listener isn't TCP")
	}
	return nil
}

func (srv *Server) socketListen() error {
	var la *net.TCPAddr
	var err error
	if la, err = net.ResolveTCPAddr("tcp", srv.Addr); err != nil {
		return err
	}

	var l *net.TCPListener
	if l, err = net.ListenTCP("tcp", la); err != nil {
		return err
	}
	srv.listener = l
	// setup listener to be non-blocking if we're not on windows.
	// this is required for hot restart to work.
	return srv.setupNonBlockingListener(err, l)
}

func (srv *Server) ListenAndServe() error {
	if srv.Addr == "" {
		srv.Addr = ":http"
	}
	if srv.listener == nil {
		if err := srv.socketListen(); err != nil {
			return err
		}
	}
	return srv.serve()
}

func (srv *Server) SocketFd() int {
	return int(srv.listenerFile.Fd())
}

func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {
	if srv.Addr == "" {
		srv.Addr = ":https"
	}
	config := &tls.Config{
		Rand:       rand.Reader,
		Time:       time.Now,
		NextProtos: []string{"http/1.1"},
	}

	var err error
	config.Certificates = make([]tls.Certificate, 1)
	config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	if srv.listener == nil {
		if err := srv.socketListen(); err != nil {
			return err
		}
	}

	srv.listener = tls.NewListener(srv.listener, config)

	return srv.serve()
}

func (srv *Server) StopAccepting() {
	close(srv.stopAccepting)
}

func (srv *Server) Port() int {
	if l := srv.listener; l != nil {
		a := l.Addr()
		if _, p, e := net.SplitHostPort(a.String()); e == nil && p != "" {
			server_port, _ := strconv.Atoi(p)
			return server_port
		}
	}
	return 0
}

func (srv *Server) serve() (e error) {
	var accept = true
	srv.AcceptReady <- 1
	for accept {
		var c net.Conn
		if l, ok := srv.listener.(*net.TCPListener); ok {
			l.SetDeadline(time.Now().Add(3 * time.Second))
		}
		c, e = srv.listener.Accept()
		if e != nil {
			if ope, ok := e.(*net.OpError); ok {
				if !(ope.Timeout() && ope.Temporary()) {
					Error("%s SERVER Accept Error: %v", srv.serverLogPrefix(), ope)
				}
			} else {
				Error("%s SERVER Accept Error: %v", srv.serverLogPrefix(), e)
			}
		} else {
			//Trace("Handling!")
			srv.handlerWaitGroup.Add(1)
			go srv.handler(c)
		}
		select {
		case <-srv.stopAccepting:
			accept = false
		default:
		}
	}
	Trace("Stopped accepting, waiting for handlers")
	// wait for handlers
	srv.handlerWaitGroup.Wait()
	return nil
}

func (srv *Server) sentinel(c net.Conn, connClosed chan int) {
	select {
	case <-srv.stopAccepting:
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
	case <-connClosed:
	}
}

// For compatibility with net/http.Server or Google App Engine
// If you are using falcore.Server as a net/http.Handler, you should
// not call any of the Listen methods
func (srv *Server) ServeHTTP(wr http.ResponseWriter, req *http.Request) {
	// We can't get the connection in this case.
	// Need to be really careful about how we use this property elsewhere.
	request := NewRequest(req, nil, time.Now())
	res := srv.handlerExecutePipeline(request, false)

	// Copy headers
	theHeader := wr.Header()
	for key, header := range res.Header {
		theHeader[key] = header
	}

	// Write headers
	wr.WriteHeader(res.StatusCode)

	// Write Body
	request.startPipelineStage("server.ResponseWrite")
	if res.Body != nil {
		defer res.Body.Close()
		io.Copy(wr, res.Body)
	}
	request.finishPipelineStage()
	request.finishRequest()

	srv.requestFinished(request, res)
}

func (srv *Server) handler(c net.Conn) {
	var startTime time.Time
	bpe := srv.bufferPool.Take(c)
	defer srv.bufferPool.Give(bpe)
	var closeSentinelChan = make(chan int)
	go srv.sentinel(c, closeSentinelChan)
	defer srv.connectionFinished(c, closeSentinelChan)
	var err error
	var req *http.Request
	// no keepalive (for now)
	reqCount := 0
	keepAlive := true
	for err == nil && keepAlive {
		if _, err := bpe.Br.Peek(1); err == nil {
			startTime = time.Now()
		}
		if req, err = http.ReadRequest(bpe.Br); err == nil {
			if req.ProtoAtLeast(1, 1) {
				if req.Header.Get("Connection") == "close" {
					keepAlive = false
				}
			} else if strings.ToLower(req.Header.Get("Connection")) != "keep-alive" {
				keepAlive = false
			}
			request := NewRequest(req, c, startTime)
			reqCount++

			pssInit := new(PipelineStageStat)
			pssInit.Name = "server.Init"
			pssInit.StartTime = startTime
			pssInit.EndTime = time.Now()
			pssInit.Type = PipelineStageTypeOverhead
			request.appendPipelineStage(pssInit)

			// execute the pipeline
			var res = srv.handlerExecutePipeline(request, keepAlive)

			// shutting down?
			select {
			case <-srv.stopAccepting:
				keepAlive = false
				res.Close = true
			default:
			}

			// write response
			srv.handlerWriteResponse(request, res, c)

			if res.Close {
				keepAlive = false
			}
		} else {
			// EOF is socket closed
			if nerr, ok := err.(net.Error); err != io.EOF && !(ok && nerr.Timeout()) {
				Error("%s %v ERROR reading request: <%T %v>", srv.serverLogPrefix(), c.RemoteAddr(), err, err)
			}
		}
	}
	//Debug("%s Processed %v requests on connection %v", srv.serverLogPrefix(), reqCount, c.RemoteAddr())
}

func (srv *Server) handlerExecutePipeline(request *Request, keepAlive bool) *http.Response {
	var res *http.Response
	// execute the pipeline
	if res = srv.Pipeline.execute(request); res == nil {
		res = StringResponse(request.HttpRequest, 404, nil, "Not Found")
	}

	// The res.Write omits Content-length on 0 length bodies, and by spec,
	// it SHOULD. While this is not MUST, it's kinda broken.  See sec 4.4
	// of rfc2616 and a 200 with a zero length does not satisfy any of the
	// 5 conditions if Connection: keep-alive is set :(
	// I'm forcing chunked which seems to work because I couldn't get the
	// content length to write if it was 0.
	// Specifically, the android http client waits forever if there's no
	// content-length instead of assuming zero at the end of headers. der.
	if res.Body == nil {
		if request.HttpRequest.Method != "HEAD" {
			res.ContentLength = 0
		}
		res.TransferEncoding = []string{"identity"}
		res.Body = ioutil.NopCloser(bytes.NewBuffer([]byte{}))
	} else if res.ContentLength == 0 && len(res.TransferEncoding) == 0 && !((res.StatusCode-100 < 100) || res.StatusCode == 204 || res.StatusCode == 304) {
		// the following is copied from net/http/transfer.go
		// in the std lib, this is only applied to a request.  we need it on a response

		// Test to see if it's actually zero or just unset.
		var buf [1]byte
		n, _ := io.ReadFull(res.Body, buf[:])
		if n == 1 {
			// Oh, guess there is data in this Body Reader after all.
			// The ContentLength field just wasn't set.
			// Stich the Body back together again, re-attaching our
			// consumed byte.
			res.ContentLength = -1
			res.Body = &lengthFixReadCloser{io.MultiReader(bytes.NewBuffer(buf[:]), res.Body), res.Body}
		} else {
			res.TransferEncoding = []string{"identity"}
		}
	}
	if res.ContentLength < 0 && request.HttpRequest.Method != "HEAD" {
		res.TransferEncoding = []string{"chunked"}
	}

	// For HTTP/1.0 and Keep-Alive, sending the Connection: Keep-Alive response header is required
	// because close is default (opposite of 1.1)
	if keepAlive && !request.HttpRequest.ProtoAtLeast(1, 1) {
		res.Header.Add("Connection", "Keep-Alive")
	}

	// cleanup
	request.HttpRequest.Body.Close()
	return res
}

func (srv *Server) handlerWriteResponse(request *Request, res *http.Response, c io.WriteCloser) {
	request.startPipelineStage("server.ResponseWrite")
	request.CurrentStage.Type = PipelineStageTypeOverhead
	if srv.sendfile {
		res.Write(c)
		if conn, ok := c.(net.Conn); ok {
			srv.cycleNonBlock(conn)
		}
	} else {
		wbuf := bufio.NewWriter(c)
		res.Write(wbuf)
		wbuf.Flush()
	}
	if res.Body != nil {
		res.Body.Close()
	}
	request.finishPipelineStage()
	request.finishRequest()
	srv.requestFinished(request, res)
}

func (srv *Server) serverLogPrefix() string {
	return srv.logPrefix
}

func (srv *Server) requestFinished(request *Request, res *http.Response) {
	if srv.CompletionCallback != nil {
		// Don't block the connecion for this
		go srv.CompletionCallback(request, res)
	}
}

func (srv *Server) connectionFinished(c net.Conn, closeChan chan int) {
	c.Close()
	close(closeChan)
	srv.handlerWaitGroup.Done()
}

type lengthFixReadCloser struct {
	io.Reader
	io.Closer
}
