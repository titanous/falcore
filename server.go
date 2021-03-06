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
	"reflect"
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
	stopAccepting      chan struct{}
	handlerWaitGroup   *sync.WaitGroup
	logPrefix          string
	AcceptReady        chan struct{}
	bufferPool         *BufferPool
	writeBufferPool    *WriteBufferPool
	PanicHandler       func(conn net.Conn, err interface{})
}

type RequestCompletionCallback func(req *Request, res *http.Response)

func NewServer(port int, pipeline *Pipeline) *Server {
	s := new(Server)
	s.Addr = fmt.Sprintf(":%v", port)
	s.Pipeline = pipeline
	s.stopAccepting = make(chan struct{})
	s.AcceptReady = make(chan struct{})
	s.handlerWaitGroup = new(sync.WaitGroup)
	s.logPrefix = fmt.Sprintf("%d", syscall.Getpid())

	// buffer pool for reusing connection bufio.Readers
	s.bufferPool = NewBufferPool(100, 8192)
	s.writeBufferPool = NewWriteBufferPool(100, 4096)

	return s
}

func (srv *Server) FdListen(fd int, name string) error {
	var err error
	if srv.listener, err = net.FileListener(os.NewFile(uintptr(fd), name)); err != nil {
		return err
	}
	if _, ok := srv.listener.(*net.TCPListener); !ok {
		return errors.New("Broken listener isn't TCP")
	}
	return syscall.Close(fd)
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
	return nil
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

func fcntl(fd int, cmd int, arg int) {
	syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(cmd), uintptr(arg))
}

func noCloseOnExec(fd int) {
	fcntl(fd, syscall.F_SETFD, ^syscall.FD_CLOEXEC)
}

func (srv *Server) SocketFd() (int, string) {
	fd := int(reflect.ValueOf(srv.listener).Elem().FieldByName("fd").Elem().FieldByName("sysfd").Int())
	noCloseOnExec(fd)
	return fd, fmt.Sprintf("tcp:%s->", srv.listener.Addr())
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
	close(srv.AcceptReady)
accept:
	for {
		var c net.Conn
		if l, ok := srv.listener.(*net.TCPListener); ok {
			l.SetDeadline(time.Now().Add(3 * time.Second))
		}
		c, e = srv.listener.Accept()
		if e != nil {
			if ope, ok := e.(*net.OpError); ok {
				if !(ope.Timeout() && ope.Temporary()) {
					Error("%s SERVER Accept Error: %v", srv.serverLogPrefix(), ope)
					os.Exit(1)
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
			break accept
		default:
		}
	}
	Trace("Stopped accepting, waiting for handlers")
	// wait for handlers
	srv.handlerWaitGroup.Wait()
	return nil
}

func (srv *Server) sentinel(c net.Conn, connClosed chan struct{}) {
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
	wbpe := srv.writeBufferPool.Take(c)
	defer srv.writeBufferPool.Give(wbpe)
	closeSentinelChan := make(chan struct{})
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
			srv.handlerWriteResponse(request, res, c, wbpe.Br)

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
		res.Header.Set("Connection", "Keep-Alive")
	}

	// cleanup
	request.HttpRequest.Body.Close()
	return res
}

func (srv *Server) handlerWriteResponse(request *Request, res *http.Response, c net.Conn, bw *bufio.Writer) {
	request.startPipelineStage("server.ResponseWrite")
	request.CurrentStage.Type = PipelineStageTypeOverhead

	var nodelay = srv.setNoDelay(c, false)
	if nodelay {
		res.Write(bw)
		bw.Flush()
		srv.setNoDelay(c, true)
	} else {
		res.Write(bw)
		bw.Flush()
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

func (srv *Server) connectionFinished(c net.Conn, closeChan chan struct{}) {
	if srv.PanicHandler != nil {
		if err := recover(); err != nil {
			srv.PanicHandler(c, err)
		}
	}
	c.Close()
	close(closeChan)
	srv.handlerWaitGroup.Done()
}

type lengthFixReadCloser struct {
	io.Reader
	io.Closer
}
