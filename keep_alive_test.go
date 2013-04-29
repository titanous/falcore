package falcore

import (
	"testing"
	"net"
	"fmt"
	"net/http"
	"bufio"
)

func TestKeepAlive(t *testing.T){
	// Startup a basic server and get the port
	pipeline := NewPipeline()
	srv := NewServer(0, pipeline)
	go func(){
		srv.ListenAndServe()
	}()
	<- srv.AcceptReady
	serverPort := srv.Port()
	
	// Connect and make some requests
	// Not using http.Client because transport does too many magics
	conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%v", serverPort))
	bconn := bufio.NewReader(conn)
	if err != nil {
		t.Fatal("Couldn't connect")
	}
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", "/", nil)
		req.Header.Set("Connection", "Keep-Alive")
		if err = req.Write(conn); err != nil {
			t.Fatal(fmt.Sprintf("[%v] Couldn't write request: %v", i, err))
		}
		res, err := http.ReadResponse(bconn, req)
		if err != nil {
			t.Fatal(fmt.Sprintf("[%v] Couldn't read response: %v", i, err))
		}
		res.Body.Close()
	}
	
	
	// Clean up
	srv.StopAccepting()
}

