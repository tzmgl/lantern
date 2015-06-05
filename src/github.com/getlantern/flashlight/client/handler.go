package client

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/getlantern/detour"
)

const (
	httpConnectMethod  = "CONNECT" // HTTP CONNECT method
	httpXFlashlightQOS = "X-Flashlight-QOS"
)

type connMeta struct {
	hostAddr      string
	peerAddr      string
	establishedAt time.Time
}

var muConns sync.RWMutex
var conns = make(map[net.Conn]connMeta)
var clientConns = make(map[net.Conn]connMeta)

func init() {
	go func() {
		ch := time.Tick(10 * time.Second)
		for now := range ch {
			muConns.RLock()
			for _, meta := range conns {
				d := now.Sub(meta.establishedAt)
				msg := fmt.Sprintf("**********Connection to %s via %s lasted for %v", meta.hostAddr, meta.peerAddr, d)
				if d > 10*time.Minute {
					log.Debug(msg)
				} else {
					log.Trace(msg)
				}
			}
			log.Debugf("**********%d connections in total", len(conns))
			for _, meta := range clientConns {
				d := now.Sub(meta.establishedAt)
				msg := fmt.Sprintf("**********Client connection to %s from %s lasted for %v", meta.hostAddr, meta.peerAddr, d)
				if d > 10*time.Minute {
					log.Debug(msg)
				} else {
					log.Trace(msg)
				}
			}
			log.Debugf("**********%d client connections in total", len(clientConns))
			muConns.RUnlock()
		}
	}()
}

// ServeHTTP implements the method from interface http.Handler using the latest
// handler available from getHandler() and latest ReverseProxy available from
// getReverseProxy().
func (client *Client) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	if req.Method == httpConnectMethod {
		// CONNECT requests are often used for HTTPS requests.
		log.Tracef("Intercepting CONNECT %s", req.URL)
		client.intercept(resp, req)
	} else {
		// Direct proxying can only be used for plain HTTP connections.
		log.Tracef("Reverse proxying %s %v", req.Method, req.URL)
		client.getReverseProxy().ServeHTTP(resp, req)
	}
}

// intercept intercepts an HTTP CONNECT request, hijacks the underlying client
// connetion and starts piping the data over a new net.Conn obtained from the
// given dial function.
func (client *Client) intercept(resp http.ResponseWriter, req *http.Request) {

	if req.Method != httpConnectMethod {
		panic("Intercept used for non-CONNECT request!")
	}

	var err error

	// Hijack underlying connection.
	var clientConn net.Conn
	if clientConn, _, err = resp.(http.Hijacker).Hijack(); err != nil {
		respondBadGateway(resp, fmt.Sprintf("Unable to hijack connection: %s", err))
		return
	}
	muConns.Lock()
	clientConns[clientConn] = connMeta{req.Host, clientConn.RemoteAddr().String(), time.Now()}
	muConns.Unlock()
	defer func() {
		clientConn.Close()
		muConns.Lock()
		delete(clientConns, clientConn)
		muConns.Unlock()
	}()

	addr := hostIncludingPort(req, 443)
	// Establish outbound connection.
	d := func(network, addr string) (net.Conn, error) {
		return client.getBalancer().DialQOS("tcp", addr, client.targetQOS(req))
	}

	var connOut net.Conn
	if runtime.GOOS == "android" || client.ProxyAll {
		connOut, err = d("tcp", addr)
	} else {
		connOut, err = detour.Dialer(d)("tcp", addr)
	}

	if err != nil {
		respondBadGateway(clientConn, fmt.Sprintf("Unable to handle CONNECT request: %s", err))
		return
	}

	serverAddr := func() (ret string) {
		// to avoid panic of RemoteAddr()
		defer func() {
			if v := recover(); v != nil {
				// do nothing
			}
		}()
		return connOut.RemoteAddr().String()
	}()
	muConns.Lock()
	conns[connOut] = connMeta{addr, serverAddr, time.Now()}
	muConns.Unlock()
	defer func() {
		connOut.Close()
		muConns.Lock()
		delete(conns, connOut)
		muConns.Unlock()
	}()

	// Pipe data between the client and the proxy.
	pipeData(clientConn, connOut, req)
}

// targetQOS determines the target quality of service given the X-Flashlight-QOS
// header if available, else returns MinQOS.
func (client *Client) targetQOS(req *http.Request) int {
	requestedQOS := req.Header.Get(httpXFlashlightQOS)

	if requestedQOS != "" {
		rqos, err := strconv.Atoi(requestedQOS)
		if err == nil {
			return rqos
		}
	}

	return client.MinQOS
}

// pipeData pipes data between the client and proxy connections.  It's also
// responsible for responding to the initial CONNECT request with a 200 OK.
func pipeData(clientConn net.Conn, connOut net.Conn, req *http.Request) {

	var wg sync.WaitGroup
	wg.Add(1)
	// Start piping from client to proxy
	go func() {
		io.Copy(connOut, clientConn)
		// Force closing if EOF at the request half or error encountered.
		// A bit arbitrary, but it's rather rare now to use half closing
		// as a way to notify server. Most application closes both connections
		// after completed send / receive so that won't cause problem.
		wg.Wait()
		clientConn.Close()
	}()

	// Respond OK
	err := respondOK(clientConn, req)
	wg.Done()
	if err != nil {
		log.Errorf("Unable to respond OK: %s", err)
		return
	}

	// Then start coyping from proxy to client
	io.Copy(clientConn, connOut)
}

func respondOK(writer io.Writer, req *http.Request) error {
	defer req.Body.Close()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		ProtoMajor: 1,
		ProtoMinor: 1,
	}

	return resp.Write(writer)
}

func respondBadGateway(w io.Writer, msg string) error {
	log.Debugf("Responding BadGateway: %v", msg)
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	err := resp.Write(w)
	if err == nil {
		_, err = w.Write([]byte(msg))
	}
	return err
}

// hostIncludingPort extracts the host:port from a request.  It fills in a
// a default port if none was found in the request.
func hostIncludingPort(req *http.Request, defaultPort int) string {
	_, port, err := net.SplitHostPort(req.Host)
	if port == "" || err != nil {
		return req.Host + ":" + strconv.Itoa(defaultPort)
	} else {
		return req.Host
	}
}
