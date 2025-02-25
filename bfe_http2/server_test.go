// Copyright (c) 2019 Baidu, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bfe_http2

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

import (
	"github.com/baidu/go-lib/gotrack"
)

import (
	http "github.com/baidu/bfe/bfe_http"
	"github.com/baidu/bfe/bfe_http2/hpack"
	tls "github.com/baidu/bfe/bfe_tls"
	util "github.com/baidu/bfe/bfe_util"
)

var stderrVerbose = flag.Bool("stderr_verbose", false, "Mirror verbosity to stderr, unbuffered")

func stderrv() io.Writer {
	if *stderrVerbose {
		return os.Stderr
	}
	return ioutil.Discard
}

type serverTester struct {
	cc             net.Conn // client conn
	t              testing.TB
	ts             *util.MockServer
	fr             *Framer
	logBuf         *bytes.Buffer
	logFilter      []string   // substrings to filter out
	scMu           sync.Mutex // guards sc
	sc             *serverConn
	hpackDec       *hpack.Decoder
	decodedHeaders [][2]string

	// writing headers:
	headerBuf bytes.Buffer
	hpackEnc  *hpack.Encoder

	// reading frames:
	frc       chan Frame
	frErrc    chan error
	readTimer *time.Timer
}

func init() {
	testHookOnPanicMu = new(sync.Mutex)
}

func resetHooks() {
	testHookOnPanicMu.Lock()
	testHookOnPanic = nil
	testHookOnPanicMu.Unlock()
}

func newServerTester(t testing.TB, handler http.HandlerFunc, opts ...interface{}) *serverTester {
	resetHooks()

	logBuf := new(bytes.Buffer)
	ts := util.NewUnstartedServer(handler)

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{NextProtoTLS, "h2"},
	}

	for _, opt := range opts {
		switch v := opt.(type) {
		case func(*tls.Config):
			v(tlsConfig)
		case func(*util.MockServer):
			v(ts)
		default:
			t.Fatalf("unknown newServerTester option type %T", v)
		}
	}
	ConfigureServer(ts.Config, &Server{})

	st := &serverTester{
		t:      t,
		ts:     ts,
		logBuf: logBuf,
		frc:    make(chan Frame, 1),
		frErrc: make(chan error, 1),
	}
	st.hpackEnc = hpack.NewEncoder(&st.headerBuf)
	st.hpackDec = hpack.NewDecoder(initialHeaderTableSize, st.onHeaderField)

	// for server
	ts.TLS = ts.Config.TLSConfig
	ts.Config.ErrorLog = log.New(io.MultiWriter(stderrv(), twriter{t: t, st: st}, logBuf), "", log.LstdFlags)
	ts.StartTLS()

	testHookGetServerConn = func(v *serverConn) {
		st.scMu.Lock()
		defer st.scMu.Unlock()
		st.sc = v
		st.sc.testHookCh = make(chan func(int))
	}
	log.SetOutput(io.MultiWriter(stderrv(), twriter{t: t, st: st}))

	// for client
	cc, err := tls.Dial("tcp", ts.Listener.Addr().String(), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	st.cc = cc
	st.fr = NewFramer(cc, cc)

	return st
}

func (st *serverTester) closeConn() {
	st.scMu.Lock()
	defer st.scMu.Unlock()
	st.sc.conn.Close()
}

func (st *serverTester) addLogFilter(phrase string) {
	st.logFilter = append(st.logFilter, phrase)
}

func (st *serverTester) stream(id uint32) *stream {
	ch := make(chan *stream, 1)
	st.sc.testHookCh <- func(int) {
		ch <- st.sc.streams[id]
	}
	return <-ch
}

func (st *serverTester) streamState(id uint32) streamState {
	ch := make(chan streamState, 1)
	st.sc.testHookCh <- func(int) {
		state, _ := st.sc.state(id)
		ch <- state
	}
	return <-ch
}

// loopNum reports how many times this conn's select loop has gone around.
func (st *serverTester) loopNum() int {
	lastc := make(chan int, 1)
	st.sc.testHookCh <- func(loopNum int) {
		lastc <- loopNum
	}
	return <-lastc
}

// awaitIdle heuristically awaits for the server conn's select loop to be idle.
// The heuristic is that the server connection's serve loop must schedule
// 50 times in a row without any channel sends or receives occurring.
func (st *serverTester) awaitIdle() {
	remain := 50
	last := st.loopNum()
	for remain > 0 {
		n := st.loopNum()
		if n == last+1 {
			remain--
		} else {
			remain = 50
		}
		last = n
	}
}

func (st *serverTester) Close() {
	if st.t.Failed() {
		// If we failed already (and are likely in a Fatal,
		// unwindowing), force close the connection, so the
		// mockServer doesn't wait forever for the conn
		// to close.
		if st.cc != nil {
			st.cc.Close()
		}
	}
	st.ts.Close()
	if st.cc != nil {
		st.cc.Close()
	}
	log.SetOutput(os.Stderr)
}

// greet initiates the client's HTTP/2 connection into a state where
// frames may be sent.
func (st *serverTester) greet() {
	st.writePreface()
	st.writeInitialSettings()
	st.wantSettings()
	st.writeSettingsAck()
	st.wantSettingsAck()
}

func (st *serverTester) writePreface() {
	n, err := st.cc.Write(clientPreface)
	if err != nil {
		st.t.Fatalf("Error writing client preface: %v", err)
	}
	if n != len(clientPreface) {
		st.t.Fatalf("Writing client preface, wrote %d bytes; want %d", n, len(clientPreface))
	}
}

func (st *serverTester) writeInitialSettings() {
	if err := st.fr.WriteSettings(); err != nil {
		st.t.Fatalf("Error writing initial SETTINGS frame from client to server: %v", err)
	}
}

func (st *serverTester) writeSettingsAck() {
	if err := st.fr.WriteSettingsAck(); err != nil {
		st.t.Fatalf("Error writing ACK of server's SETTINGS: %v", err)
	}
}

func (st *serverTester) writeHeaders(p HeadersFrameParam) {
	if err := st.fr.WriteHeaders(p); err != nil {
		st.t.Fatalf("Error writing HEADERS: %v", err)
	}
}

func (st *serverTester) encodeHeaderField(k, v string) {
	err := st.hpackEnc.WriteField(hpack.HeaderField{Name: k, Value: v})
	if err != nil {
		st.t.Fatalf("HPACK encoding error for %q/%q: %v", k, v, err)
	}
}

// encodeHeaderRaw is the magic-free version of encodeHeader.
// It takes 0 or more (k, v) pairs and encodes them.
func (st *serverTester) encodeHeaderRaw(headers ...string) []byte {
	if len(headers)%2 == 1 {
		panic("odd number of kv args")
	}
	st.headerBuf.Reset()
	for len(headers) > 0 {
		k, v := headers[0], headers[1]
		st.encodeHeaderField(k, v)
		headers = headers[2:]
	}
	return st.headerBuf.Bytes()
}

// encodeHeader encodes headers and returns their HPACK bytes. headers
// must contain an even number of key/value pairs.  There may be
// multiple pairs for keys (e.g. "cookie").  The :method, :path, and
// :scheme headers default to GET, / and https.
func (st *serverTester) encodeHeader(headers ...string) []byte {
	if len(headers)%2 == 1 {
		panic("odd number of kv args")
	}

	st.headerBuf.Reset()

	if len(headers) == 0 {
		// Fast path, mostly for benchmarks, so test code doesn't pollute
		// profiles when we're looking to improve server allocations.
		st.encodeHeaderField(":method", "GET")
		st.encodeHeaderField(":path", "/")
		st.encodeHeaderField(":scheme", "https")
		return st.headerBuf.Bytes()
	}

	if len(headers) == 2 && headers[0] == ":method" {
		// Another fast path for benchmarks.
		st.encodeHeaderField(":method", headers[1])
		st.encodeHeaderField(":path", "/")
		st.encodeHeaderField(":scheme", "https")
		return st.headerBuf.Bytes()
	}

	pseudoCount := map[string]int{}
	keys := []string{":method", ":path", ":scheme"}
	vals := map[string][]string{
		":method": {"GET"},
		":path":   {"/"},
		":scheme": {"https"},
	}
	for len(headers) > 0 {
		k, v := headers[0], headers[1]
		headers = headers[2:]
		if _, ok := vals[k]; !ok {
			keys = append(keys, k)
		}
		if strings.HasPrefix(k, ":") {
			pseudoCount[k]++
			if pseudoCount[k] == 1 {
				vals[k] = []string{v}
			} else {
				// Allows testing of invalid headers w/ dup pseudo fields.
				vals[k] = append(vals[k], v)
			}
		} else {
			vals[k] = append(vals[k], v)
		}
	}
	for _, k := range keys {
		for _, v := range vals[k] {
			st.encodeHeaderField(k, v)
		}
	}
	return st.headerBuf.Bytes()
}

// bodylessReq1 writes a HEADERS frames with StreamID 1 and EndStream and EndHeaders set.
func (st *serverTester) bodylessReq1(headers ...string) {
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: st.encodeHeader(headers...),
		EndStream:     true,
		EndHeaders:    true,
	})
}

func (st *serverTester) writeData(streamID uint32, endStream bool, data []byte) {
	if err := st.fr.WriteData(streamID, endStream, data); err != nil {
		st.t.Fatalf("Error writing DATA: %v", err)
	}
}

func (st *serverTester) writeDataPadded(streamID uint32, endStream bool, data, pad []byte) {
	if err := st.fr.WriteDataPadded(streamID, endStream, data, pad); err != nil {
		st.t.Fatalf("Error writing DATA: %v", err)
	}
}

func (st *serverTester) readFrame() (Frame, error) {
	go func() {
		fr, err := st.fr.ReadFrame()
		if err != nil {
			st.frErrc <- err
		} else {
			st.frc <- fr
		}
	}()
	t := st.readTimer
	if t == nil {
		t = time.NewTimer(2 * time.Second)
		st.readTimer = t
	}
	t.Reset(2 * time.Second)
	defer t.Stop()
	select {
	case f := <-st.frc:
		return f, nil
	case err := <-st.frErrc:
		return nil, err
	case <-t.C:
		return nil, errors.New("timeout waiting for frame")
	}
}

func (st *serverTester) wantHeaders() *HeadersFrame {
	f, err := st.readFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting a HEADERS frame: %v", err)
	}
	hf, ok := f.(*HeadersFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *HeadersFrame", f)
	}
	return hf
}

func (st *serverTester) wantContinuation() *ContinuationFrame {
	f, err := st.readFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting a CONTINUATION frame: %v", err)
	}
	cf, ok := f.(*ContinuationFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *ContinuationFrame", f)
	}
	return cf
}

func (st *serverTester) wantData() *DataFrame {
	f, err := st.readFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting a DATA frame: %v", err)
	}
	df, ok := f.(*DataFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *DataFrame", f)
	}
	return df
}

func (st *serverTester) wantSettings() *SettingsFrame {
	f, err := st.readFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting a SETTINGS frame: %v", err)
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *SettingsFrame", f)
	}
	return sf
}

func (st *serverTester) wantPing() *PingFrame {
	f, err := st.readFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting a PING frame: %v", err)
	}
	pf, ok := f.(*PingFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *PingFrame", f)
	}
	return pf
}

func (st *serverTester) wantGoAway() *GoAwayFrame {
	f, err := st.readFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting a GOAWAY frame: %v", err)
	}
	gf, ok := f.(*GoAwayFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *GoAwayFrame", f)
	}
	return gf
}

func (st *serverTester) wantRSTStream(streamID uint32, errCode ErrCode) {
	f, err := st.readFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting an RSTStream frame: %v", err)
	}
	rs, ok := f.(*RSTStreamFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *RSTStreamFrame", f)
	}
	if rs.FrameHeader.StreamID != streamID {
		st.t.Fatalf("RSTStream StreamID = %d; want %d", rs.FrameHeader.StreamID, streamID)
	}
	if rs.ErrCode != errCode {
		st.t.Fatalf("RSTStream ErrCode = %d (%s); want %d (%s)", rs.ErrCode, rs.ErrCode, errCode, errCode)
	}
}

func (st *serverTester) wantWindowUpdate(streamID, incr uint32) {
	f, err := st.readFrame()
	if err != nil {
		st.t.Fatalf("Error while expecting a WINDOW_UPDATE frame: %v", err)
	}
	wu, ok := f.(*WindowUpdateFrame)
	if !ok {
		st.t.Fatalf("got a %T; want *WindowUpdateFrame", f)
	}
	if wu.FrameHeader.StreamID != streamID {
		st.t.Fatalf("WindowUpdate StreamID = %d; want %d", wu.FrameHeader.StreamID, streamID)
	}
	if wu.Increment != incr {
		st.t.Fatalf("WindowUpdate increment = %d; want %d", wu.Increment, incr)
	}
}

func (st *serverTester) wantSettingsAck() {
	f, err := st.readFrame()
	if err != nil {
		st.t.Fatal(err)
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		st.t.Fatalf("Wanting a settings ACK, received a %T", f)
	}
	if !sf.Header().Flags.Has(FlagSettingsAck) {
		st.t.Fatal("Settings Frame didn't have ACK set")
	}

}

func TestServer(t *testing.T) {
	gotReq := make(chan bool, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Foo", "Bar")
		gotReq <- true
	})
	defer st.Close()

	covers("3.5", `
		The server connection preface consists of a potentially empty
		SETTINGS frame ([SETTINGS]) that MUST be the first frame the
		server sends in the HTTP/2 connection.
	`)

	st.writePreface()
	st.writeInitialSettings()
	st.wantSettings()
	st.writeSettingsAck()
	st.wantSettingsAck()

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: st.encodeHeader(),
		EndStream:     true, // no DATA frames
		EndHeaders:    true,
	})

	select {
	case <-gotReq:
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for request")
	}
}

func TestServer_Request_Get(t *testing.T) {
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader("foo-bar", "some-value"),
			EndStream:     true, // no DATA frames
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("Method = %q; want GET", r.Method)
		}
		if r.URL.Path != "/" {
			t.Errorf("URL.Path = %q; want /", r.URL.Path)
		}
		if r.ContentLength != 0 {
			t.Errorf("ContentLength = %v; want 0", r.ContentLength)
		}
		if r.Close {
			t.Error("Close = true; want false")
		}
		if !strings.Contains(r.RemoteAddr, ":") {
			t.Errorf("RemoteAddr = %q; want something with a colon", r.RemoteAddr)
		}
		if r.Proto != "HTTP/2.0" || r.ProtoMajor != 2 || r.ProtoMinor != 0 {
			t.Errorf("Proto = %q Major=%v,Minor=%v; want HTTP/2.0", r.Proto, r.ProtoMajor, r.ProtoMinor)
		}
		wantHeader := http.Header{
			"Foo-Bar": []string{"some-value"},
		}
		if !reflect.DeepEqual(r.Header, wantHeader) {
			t.Errorf("Header = %#v; want %#v", r.Header, wantHeader)
		}
		if n, err := r.Body.Read([]byte(" ")); err != io.EOF || n != 0 {
			t.Errorf("Read = %d, %v; want 0, EOF", n, err)
		}
	})
}

func TestServer_Request_Post_NoContentLength_EndStream(t *testing.T) {
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST"),
			EndStream:     true,
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Method = %q; want POST", r.Method)
		}
		if r.ContentLength != 0 {
			t.Errorf("ContentLength = %v; want 0", r.ContentLength)
		}
		if n, err := r.Body.Read([]byte(" ")); err != io.EOF || n != 0 {
			t.Errorf("Read = %d, %v; want 0, EOF", n, err)
		}
	})
}

func TestServer_Request_Post_Body_ImmediateEOF(t *testing.T) {
	testBodyContents(t, -1, "", func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST"),
			EndStream:     false, // to say DATA frames are coming
			EndHeaders:    true,
		})
		st.writeData(1, true, nil) // just kidding. empty body.
	})
}

func TestServer_Request_Post_Body_OneData(t *testing.T) {
	const content = "Some content"
	testBodyContents(t, -1, content, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST"),
			EndStream:     false, // to say DATA frames are coming
			EndHeaders:    true,
		})
		st.writeData(1, true, []byte(content))
	})
}

func TestServer_Request_Post_Body_TwoData(t *testing.T) {
	const content = "Some content"
	testBodyContents(t, -1, content, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST"),
			EndStream:     false, // to say DATA frames are coming
			EndHeaders:    true,
		})
		st.writeData(1, false, []byte(content[:5]))
		st.writeData(1, true, []byte(content[5:]))
	})
}

func TestServer_Request_Post_Body_ContentLength_Correct(t *testing.T) {
	const content = "Some content"
	testBodyContents(t, int64(len(content)), content, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1, // clients send odd numbers
			BlockFragment: st.encodeHeader(
				":method", "POST",
				"content-length", strconv.Itoa(len(content)),
			),
			EndStream:  false, // to say DATA frames are coming
			EndHeaders: true,
		})
		st.writeData(1, true, []byte(content))
	})
}

func TestServer_Request_Post_Body_ContentLength_TooLarge(t *testing.T) {
	testBodyContentsFail(t, 3, "request declared a Content-Length of 3 but only wrote 2 bytes",
		func(st *serverTester) {
			st.writeHeaders(HeadersFrameParam{
				StreamID: 1, // clients send odd numbers
				BlockFragment: st.encodeHeader(
					":method", "POST",
					"content-length", "3",
				),
				EndStream:  false, // to say DATA frames are coming
				EndHeaders: true,
			})
			st.writeData(1, true, []byte("12"))
		})
}

func TestServer_Request_Post_Body_ContentLength_TooSmall(t *testing.T) {
	testBodyContentsFail(t, 4, "sender tried to send more than declared Content-Length of 4 bytes",
		func(st *serverTester) {
			st.writeHeaders(HeadersFrameParam{
				StreamID: 1, // clients send odd numbers
				BlockFragment: st.encodeHeader(
					":method", "POST",
					"content-length", "4",
				),
				EndStream:  false, // to say DATA frames are coming
				EndHeaders: true,
			})
			st.writeData(1, true, []byte("12345"))
		})
}

func testBodyContents(t *testing.T, wantContentLength int64, wantBody string, write func(st *serverTester)) {
	testServerRequest(t, write, func(r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Method = %q; want POST", r.Method)
		}
		if r.ContentLength != wantContentLength {
			t.Errorf("ContentLength = %v; want %d", r.ContentLength, wantContentLength)
		}
		all, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(all) != wantBody {
			t.Errorf("Read = %q; want %q", all, wantBody)
		}
		if err := r.Body.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

func testBodyContentsFail(t *testing.T, wantContentLength int64, wantReadError string, write func(st *serverTester)) {
	testServerRequest(t, write, func(r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Method = %q; want POST", r.Method)
		}
		if r.ContentLength != wantContentLength {
			t.Errorf("ContentLength = %v; want %d", r.ContentLength, wantContentLength)
		}
		all, err := ioutil.ReadAll(r.Body)
		if err == nil {
			t.Fatalf("expected an error (%q) reading from the body. Successfully read %q instead.",
				wantReadError, all)
		}
		if !strings.Contains(err.Error(), wantReadError) {
			t.Fatalf("Body.Read = %v; want substring %q", err, wantReadError)
		}
		if err := r.Body.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// Using a Host header, instead of :authority
func TestServer_Request_Get_Host(t *testing.T) {
	const host = "example.com"
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader("host", host),
			EndStream:     true,
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.Host != host {
			t.Errorf("Host = %q; want %q", r.Host, host)
		}
	})
}

// Using an :authority pseudo-header, instead of Host
func TestServer_Request_Get_Authority(t *testing.T) {
	const host = "example.com"
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":authority", host),
			EndStream:     true,
			EndHeaders:    true,
		})
	}, func(r *http.Request) {
		if r.Host != host {
			t.Errorf("Host = %q; want %q", r.Host, host)
		}
	})
}

func TestServer_Request_WithContinuation(t *testing.T) {
	wantHeader := http.Header{
		"Foo-One":   []string{"value-one"},
		"Foo-Two":   []string{"value-two"},
		"Foo-Three": []string{"value-three"},
	}
	testServerRequest(t, func(st *serverTester) {
		fullHeaders := st.encodeHeader(
			"foo-one", "value-one",
			"foo-two", "value-two",
			"foo-three", "value-three",
		)
		remain := fullHeaders
		chunks := 0
		for len(remain) > 0 {
			const maxChunkSize = 5
			chunk := remain
			if len(chunk) > maxChunkSize {
				chunk = chunk[:maxChunkSize]
			}
			remain = remain[len(chunk):]

			if chunks == 0 {
				st.writeHeaders(HeadersFrameParam{
					StreamID:      1, // clients send odd numbers
					BlockFragment: chunk,
					EndStream:     true,  // no DATA frames
					EndHeaders:    false, // we'll have continuation frames
				})
			} else {
				err := st.fr.WriteContinuation(1, len(remain) == 0, chunk)
				if err != nil {
					t.Fatal(err)
				}
			}
			chunks++
		}
		if chunks < 2 {
			t.Fatal("too few chunks")
		}
	}, func(r *http.Request) {
		if !reflect.DeepEqual(r.Header, wantHeader) {
			t.Errorf("Header = %#v; want %#v", r.Header, wantHeader)
		}
	})
}

// Concatenated cookie headers. ("8.1.2.5 Compressing the Cookie Header Field")
func TestServer_Request_CookieConcat(t *testing.T) {
	const host = "example.com"
	testServerRequest(t, func(st *serverTester) {
		st.bodylessReq1(
			":authority", host,
			"cookie", "a=b",
			"cookie", "c=d",
			"cookie", "e=f",
		)
	}, func(r *http.Request) {
		const want = "a=b; c=d; e=f"
		if got := r.Header.Get("Cookie"); got != want {
			t.Errorf("Cookie = %q; want %q", got, want)
		}
	})
}

func TestServer_Request_Reject_CapitalHeader(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("UPPER", "v") })
}

func TestServer_Request_Reject_HeaderFieldNameColon(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("has:colon", "v") })
}

func TestServer_Request_Reject_HeaderFieldNameNULL(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("has\x00null", "v") })
}

func TestServer_Request_Reject_HeaderFieldNameEmpty(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("", "v") })
}

func TestServer_Request_Reject_HeaderFieldValueNewline(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("foo", "has\nnewline") })
}

func TestServer_Request_Reject_HeaderFieldValueCR(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("foo", "has\rcarriage") })
}

func TestServer_Request_Reject_HeaderFieldValueDEL(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1("foo", "has\x7fdel") })
}

func TestServer_Request_Reject_Pseudo_Missing_method(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":method", "") })
}

func TestServer_Request_Reject_Pseudo_ExactlyOne(t *testing.T) {
	// 8.1.2.3 Request Pseudo-Header Fields
	// "All HTTP/2 requests MUST include exactly one valid value" ...
	testRejectRequest(t, func(st *serverTester) {
		st.addLogFilter("duplicate pseudo-header")
		st.bodylessReq1(":method", "GET", ":method", "POST")
	})
}

func TestServer_Request_Reject_Pseudo_AfterRegular(t *testing.T) {
	// 8.1.2.3 Request Pseudo-Header Fields
	// "All pseudo-header fields MUST appear in the header block
	// before regular header fields. Any request or response that
	// contains a pseudo-header field that appears in a header
	// block after a regular header field MUST be treated as
	// malformed (Section 8.1.2.6)."
	testRejectRequest(t, func(st *serverTester) {
		st.addLogFilter("pseudo-header after regular header")
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		enc.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"})
		enc.WriteField(hpack.HeaderField{Name: "regular", Value: "foobar"})
		enc.WriteField(hpack.HeaderField{Name: ":path", Value: "/"})
		enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: buf.Bytes(),
			EndStream:     true,
			EndHeaders:    true,
		})
	})
}

func TestServer_Request_Reject_Pseudo_Missing_path(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":path", "") })
}

func TestServer_Request_Reject_Pseudo_Missing_scheme(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":scheme", "") })
}

func TestServer_Request_Reject_Pseudo_scheme_invalid(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) { st.bodylessReq1(":scheme", "bogus") })
}

func TestServer_Request_Reject_Pseudo_Unknown(t *testing.T) {
	testRejectRequest(t, func(st *serverTester) {
		st.addLogFilter(`invalid pseudo-header ":unknown_thing"`)
		st.bodylessReq1(":unknown_thing", "")
	})
}

func testRejectRequest(t *testing.T, send func(*serverTester)) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server request made it to handler; should've been rejected")
	})
	defer st.Close()

	st.greet()
	send(st)
	st.wantRSTStream(1, ErrCodeProtocol)
}

func TestServer_Request_Connect(t *testing.T) {
	testServerRequest(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1,
			BlockFragment: st.encodeHeaderRaw(
				":method", "CONNECT",
				":authority", "example.com:123",
			),
			EndStream:  true,
			EndHeaders: true,
		})
	}, func(r *http.Request) {
		if g, w := r.Method, "CONNECT"; g != w {
			t.Errorf("Method = %q; want %q", g, w)
		}
		if g, w := r.RequestURI, "example.com:123"; g != w {
			t.Errorf("RequestURI = %q; want %q", g, w)
		}
		if g, w := r.URL.Host, "example.com:123"; g != w {
			t.Errorf("URL.Host = %q; want %q", g, w)
		}
	})
}

func TestServer_Request_Connect_InvalidPath(t *testing.T) {
	testServerRejectsStream(t, ErrCodeProtocol, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1,
			BlockFragment: st.encodeHeaderRaw(
				":method", "CONNECT",
				":authority", "example.com:123",
				":path", "/bogus",
			),
			EndStream:  true,
			EndHeaders: true,
		})
	})
}

func TestServer_Request_Connect_InvalidScheme(t *testing.T) {
	testServerRejectsStream(t, ErrCodeProtocol, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1,
			BlockFragment: st.encodeHeaderRaw(
				":method", "CONNECT",
				":authority", "example.com:123",
				":scheme", "https",
			),
			EndStream:  true,
			EndHeaders: true,
		})
	})
}

func TestServer_Ping(t *testing.T) {
	st := newServerTester(t, nil)
	defer st.Close()
	st.greet()

	// Server should ignore this one, since it has ACK set.
	ackPingData := [8]byte{1, 2, 4, 8, 16, 32, 64, 128}
	if err := st.fr.WritePing(true, ackPingData); err != nil {
		t.Fatal(err)
	}

	// But the server should reply to this one, since ACK is false.
	pingData := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if err := st.fr.WritePing(false, pingData); err != nil {
		t.Fatal(err)
	}

	pf := st.wantPing()
	if !pf.Flags.Has(FlagPingAck) {
		t.Error("response ping doesn't have ACK set")
	}
	if pf.Data != pingData {
		t.Errorf("response ping has data %q; want %q", pf.Data, pingData)
	}
}

func TestServer_RejectsLargeFrames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("see golang.org/issue/13434")
	}

	st := newServerTester(t, nil)
	defer st.Close()
	st.greet()

	// Write too large of a frame (too large by one byte)
	// We ignore the return value because it's expected that the server
	// will only read the first 9 bytes (the headre) and then disconnect.
	st.fr.WriteRawFrame(0xff, 0, 0, make([]byte, defaultMaxReadFrameSize+1))

	gf := st.wantGoAway()
	if gf.ErrCode != ErrCodeFrameSize {
		t.Errorf("GOAWAY err = %v; want %v", gf.ErrCode, ErrCodeFrameSize)
	}
	if st.logBuf.Len() != 0 {
		// Previously we spun here for a bit until the GOAWAY disconnect
		// timer fired, logging while we fired.
		t.Errorf("unexpected server output: %.500s\n", st.logBuf.Bytes())
	}
}

func TestServer_Handler_Sends_WindowUpdate(t *testing.T) {
	puppet := newHandlerPuppet()
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		puppet.act(w, r)
	})
	defer st.Close()
	defer puppet.done()

	st.greet()

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false, // data coming
		EndHeaders:    true,
	})
	st.writeData(1, false, []byte("abcdef"))
	puppet.do(readBodyHandler(t, "abc"))
	st.wantWindowUpdate(0, 3)
	st.wantWindowUpdate(1, 3)

	puppet.do(readBodyHandler(t, "def"))
	st.wantWindowUpdate(0, 3)
	st.wantWindowUpdate(1, 3)

	st.writeData(1, true, []byte("ghijkl")) // END_STREAM here
	puppet.do(readBodyHandler(t, "ghi"))
	puppet.do(readBodyHandler(t, "jkl"))
	st.wantWindowUpdate(0, 3)
	st.wantWindowUpdate(0, 3) // no more stream-level, since END_STREAM
}

// the version of the TestServer_Handler_Sends_WindowUpdate with padding.
// See golang.org/issue/16556
func TestServer_Handler_Sends_WindowUpdate_Padding(t *testing.T) {
	puppet := newHandlerPuppet()
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		puppet.act(w, r)
	})
	defer st.Close()
	defer puppet.done()

	st.greet()

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false,
		EndHeaders:    true,
	})
	st.writeDataPadded(1, false, []byte("abcdef"), []byte("1234"))

	// Expect to immediately get our 5 bytes of padding back for
	// both the connection and stream (4 bytes of padding + 1 byte of length)
	st.wantWindowUpdate(0, 5)
	st.wantWindowUpdate(1, 5)

	puppet.do(readBodyHandler(t, "abc"))
	st.wantWindowUpdate(0, 3)
	st.wantWindowUpdate(1, 3)

	puppet.do(readBodyHandler(t, "def"))
	st.wantWindowUpdate(0, 3)
	st.wantWindowUpdate(1, 3)
}

func TestServer_Send_GoAway_After_Bogus_WindowUpdate(t *testing.T) {
	st := newServerTester(t, nil)
	defer st.Close()
	st.greet()
	if err := st.fr.WriteWindowUpdate(0, 1<<31-1); err != nil {
		t.Fatal(err)
	}
	gf := st.wantGoAway()
	if gf.ErrCode != ErrCodeFlowControl {
		t.Errorf("GOAWAY err = %v; want %v", gf.ErrCode, ErrCodeFlowControl)
	}
	if gf.LastStreamID != 0 {
		t.Errorf("GOAWAY last stream ID = %v; want %v", gf.LastStreamID, 0)
	}
}

func TestServer_Send_RstStream_After_Bogus_WindowUpdate(t *testing.T) {
	inHandler := make(chan bool)
	blockHandler := make(chan bool)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		inHandler <- true
		<-blockHandler
	})
	defer st.Close()
	defer close(blockHandler)
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})
	<-inHandler
	// Send a bogus window update:
	if err := st.fr.WriteWindowUpdate(1, 1<<31-1); err != nil {
		t.Fatal(err)
	}
	st.wantRSTStream(1, ErrCodeFlowControl)
}

// testServerPostUnblock sends a hanging POST with unsent data to handler,
// then runs fn once in the handler, and verifies that the error returned from
// handler is acceptable. It fails if takes over 5 seconds for handler to exit.
func testServerPostUnblock(t *testing.T,
	handler func(http.ResponseWriter, *http.Request) error,
	fn func(*serverTester),
	checkErr func(error),
	otherHeaders ...string) {
	inHandler := make(chan bool)
	errc := make(chan error, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		inHandler <- true
		errc <- handler(w, r)
	})
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(append([]string{":method", "POST"}, otherHeaders...)...),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})
	<-inHandler
	fn(st)
	select {
	case err := <-errc:
		if checkErr != nil {
			checkErr(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Handler to return")
	}
	st.Close()
}

func TestServer_RSTStream_Unblocks_Read(t *testing.T) {
	testServerPostUnblock(t,
		func(w http.ResponseWriter, r *http.Request) (err error) {
			_, err = r.Body.Read(make([]byte, 1))
			return
		},
		func(st *serverTester) {
			if err := st.fr.WriteRSTStream(1, ErrCodeCancel); err != nil {
				t.Fatal(err)
			}
		},
		func(err error) {
			want := StreamError{StreamID: 0x1, Code: 0x8}
			if !checkEqual(err, want) {
				t.Errorf("Read error = %v; want %v", err, want)
			}
		},
	)
}

func TestServer_RSTStream_Unblocks_Header_Write(t *testing.T) {
	// Run this test a bunch, because it doesn't always
	// deadlock. But with a bunch, it did.
	n := 50
	if testing.Short() {
		n = 5
	}
	for i := 0; i < n; i++ {
		testServer_RSTStream_Unblocks_Header_Write(t)
	}
}

func testServer_RSTStream_Unblocks_Header_Write(t *testing.T) {
	inHandler := make(chan bool, 1)
	unblockHandler := make(chan bool, 1)
	headerWritten := make(chan bool, 1)
	wroteRST := make(chan bool, 1)

	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		inHandler <- true
		<-wroteRST
		w.Header().Set("foo", "bar")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		headerWritten <- true
		<-unblockHandler
	})
	defer st.Close()

	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})
	<-inHandler
	if err := st.fr.WriteRSTStream(1, ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	wroteRST <- true
	st.awaitIdle()
	select {
	case <-headerWritten:
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for header write")
	}
	unblockHandler <- true
}

func TestServer_DeadConn_Unblocks_Read(t *testing.T) {
	testServerPostUnblock(t,
		func(w http.ResponseWriter, r *http.Request) (err error) {
			_, err = r.Body.Read(make([]byte, 1))
			return
		},
		func(st *serverTester) { st.cc.Close() },
		func(err error) {
			if err == nil {
				t.Error("unexpected nil error from Request.Body.Read")
			}
		},
	)
}

var blockUntilClosed = func(w http.ResponseWriter, r *http.Request) error {
	<-w.(http.CloseNotifier).CloseNotify()
	return nil
}

func TestServer_CloseNotify_After_RSTStream(t *testing.T) {
	testServerPostUnblock(t, blockUntilClosed, func(st *serverTester) {
		if err := st.fr.WriteRSTStream(1, ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	}, nil)
}

func TestServer_CloseNotify_After_ConnClose(t *testing.T) {
	testServerPostUnblock(t, blockUntilClosed, func(st *serverTester) { st.cc.Close() }, nil)
}

// that CloseNotify unblocks after a stream error due to the client's
// problem that's unrelated to them explicitly canceling it (which is
// TestServer_CloseNotify_After_RSTStream above)
func TestServer_CloseNotify_After_StreamError(t *testing.T) {
	testServerPostUnblock(t, blockUntilClosed, func(st *serverTester) {
		// data longer than declared Content-Length => stream error
		st.writeData(1, true, []byte("1234"))
	}, nil, "content-length", "3")
}

func TestServer_StateTransitions(t *testing.T) {
	var st *serverTester
	inHandler := make(chan bool)
	writeData := make(chan bool)
	leaveHandler := make(chan bool)
	st = newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		inHandler <- true
		if st.stream(1) == nil {
			t.Errorf("nil stream 1 in handler")
		}
		if got, want := st.streamState(1), stateOpen; got != want {
			t.Errorf("in handler, state is %v; want %v", got, want)
		}
		writeData <- true
		if n, err := r.Body.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Errorf("body read = %d, %v; want 0, EOF", n, err)
		}
		if got, want := st.streamState(1), stateHalfClosedRemote; got != want {
			t.Errorf("in handler, state is %v; want %v", got, want)
		}

		<-leaveHandler
	})
	st.greet()
	if st.stream(1) != nil {
		t.Fatal("stream 1 should be empty")
	}
	if got := st.streamState(1); got != stateIdle {
		t.Fatalf("stream 1 should be idle; got %v", got)
	}

	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(":method", "POST"),
		EndStream:     false, // keep it open
		EndHeaders:    true,
	})
	<-inHandler
	<-writeData
	st.writeData(1, true, nil)

	leaveHandler <- true
	hf := st.wantHeaders()
	if !hf.StreamEnded() {
		t.Fatal("expected END_STREAM flag")
	}

	if got, want := st.streamState(1), stateClosed; got != want {
		t.Errorf("at end, state is %v; want %v", got, want)
	}
	if st.stream(1) != nil {
		t.Fatal("at end, stream 1 should be gone")
	}
}

// test HEADERS w/o EndHeaders + another HEADERS (should get rejected)
func TestServer_Rejects_HeadersNoEnd_Then_Headers(t *testing.T) {
	testServerRejectsConn(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    false,
		})
		st.writeHeaders(HeadersFrameParam{ // Not a continuation.
			StreamID:      3, // different stream.
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
		})
	})
}

// test HEADERS w/o EndHeaders + PING (should get rejected)
func TestServer_Rejects_HeadersNoEnd_Then_Ping(t *testing.T) {
	testServerRejectsConn(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    false,
		})
		if err := st.fr.WritePing(false, [8]byte{}); err != nil {
			t.Fatal(err)
		}
	})
}

// test HEADERS w/ EndHeaders + a continuation HEADERS (should get rejected)
func TestServer_Rejects_HeadersEnd_Then_Continuation(t *testing.T) {
	testServerRejectsConn(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
		})
		st.wantHeaders()
		if err := st.fr.WriteContinuation(1, true, encodeHeaderNoImplicit(t, "foo", "bar")); err != nil {
			t.Fatal(err)
		}
	})
}

// test HEADERS w/o EndHeaders + a continuation HEADERS on wrong stream ID
func TestServer_Rejects_HeadersNoEnd_Then_ContinuationWrongStream(t *testing.T) {
	testServerRejectsConn(t, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    false,
		})
		if err := st.fr.WriteContinuation(3, true, encodeHeaderNoImplicit(t, "foo", "bar")); err != nil {
			t.Fatal(err)
		}
	})
}

// No HEADERS on stream 0.
func TestServer_Rejects_Headers0(t *testing.T) {
	testServerRejectsConn(t, func(st *serverTester) {
		st.fr.AllowIllegalWrites = true
		st.writeHeaders(HeadersFrameParam{
			StreamID:      0,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
		})
	})
}

// No CONTINUATION on stream 0.
func TestServer_Rejects_Continuation0(t *testing.T) {
	testServerRejectsConn(t, func(st *serverTester) {
		st.fr.AllowIllegalWrites = true
		if err := st.fr.WriteContinuation(0, true, st.encodeHeader()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestServer_Rejects_PushPromise(t *testing.T) {
	testServerRejectsConn(t, func(st *serverTester) {
		pp := PushPromiseParam{
			StreamID:  1,
			PromiseID: 3,
		}
		if err := st.fr.WritePushPromise(pp); err != nil {
			t.Fatal(err)
		}
	})
}

// testServerRejectsConn tests that the server hangs up with a GOAWAY
// frame and a server close after the client does something
// deserving a CONNECTION_ERROR.
func testServerRejectsConn(t *testing.T, writeReq func(*serverTester)) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {})
	st.addLogFilter("connection error: PROTOCOL_ERROR")
	defer st.Close()
	st.greet()
	writeReq(st)

	st.wantGoAway()
	errc := make(chan error, 1)
	go func() {
		fr, err := st.fr.ReadFrame()
		if err == nil {
			err = fmt.Errorf("got frame of type %T", fr)
		}
		errc <- err
	}()
	select {
	case err := <-errc:
		if err != io.EOF {
			t.Errorf("ReadFrame = %v; want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for disconnect")
	}
}

// testServerRejectsStream tests that the server sends a RST_STREAM with the provided
// error code after a client sends a bogus request.
func testServerRejectsStream(t *testing.T, code ErrCode, writeReq func(*serverTester)) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {})
	defer st.Close()
	st.greet()
	writeReq(st)
	st.wantRSTStream(1, code)
}

// testServerRequest sets up an idle HTTP/2 connection and lets you
// write a single request with writeReq, and then verify that the
// *http.Request is built correctly in checkReq.
func testServerRequest(t *testing.T, writeReq func(*serverTester), checkReq func(*http.Request)) {
	gotReq := make(chan bool, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			t.Fatal("nil Body")
		}
		checkReq(r)
		gotReq <- true
	})
	defer st.Close()

	st.greet()
	writeReq(st)

	select {
	case <-gotReq:
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for request")
	}
}

func getSlash(st *serverTester) { st.bodylessReq1() }

func TestServer_Response_NoData(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		// Nothing.
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if !hf.StreamEnded() {
			t.Fatal("want END_STREAM flag")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
	})
}

func TestServer_Response_NoData_Header_FooBar(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Foo-Bar", "some-value")
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if !hf.StreamEnded() {
			t.Fatal("want END_STREAM flag")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"foo-bar", "some-value"},
			{"content-type", "text/plain; charset=utf-8"},
			{"content-length", "0"},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}
	})
}

func TestServer_Response_Data_Sniff_DoesntOverride(t *testing.T) {
	const msg = "<html>this is HTML."
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "foo/bar")
		io.WriteString(w, msg)
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("don't want END_STREAM, expecting data")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"content-type", "foo/bar"},
			{"content-length", strconv.Itoa(len(msg))},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}
		df := st.wantData()
		if !df.StreamEnded() {
			t.Error("expected DATA to have END_STREAM flag")
		}
		if got := string(df.Data()); got != msg {
			t.Errorf("got DATA %q; want %q", got, msg)
		}
	})
}

func TestServer_Response_TransferEncoding_chunked(t *testing.T) {
	const msg = "hi"
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Transfer-Encoding", "chunked") // should be stripped
		io.WriteString(w, msg)
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"content-type", "text/plain; charset=utf-8"},
			{"content-length", strconv.Itoa(len(msg))},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}
	})
}

// Header accessed only after the initial write.
func TestServer_Response_Data_IgnoreHeaderAfterWrite_After(t *testing.T) {
	const msg = "<html>this is HTML."
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		io.WriteString(w, msg)
		w.Header().Set("foo", "should be ignored")
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("unexpected END_STREAM")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"content-type", "text/html; charset=utf-8"},
			{"content-length", strconv.Itoa(len(msg))},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}
	})
}

// Header accessed before the initial write and later mutated.
func TestServer_Response_Data_IgnoreHeaderAfterWrite_Overwrite(t *testing.T) {
	const msg = "<html>this is HTML."
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("foo", "proper value")
		io.WriteString(w, msg)
		w.Header().Set("foo", "should be ignored")
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("unexpected END_STREAM")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"foo", "proper value"},
			{"content-type", "text/html; charset=utf-8"},
			{"content-length", strconv.Itoa(len(msg))},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}
	})
}

func TestServer_Response_Data_SniffLenType(t *testing.T) {
	const msg = "<html>this is HTML."
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		io.WriteString(w, msg)
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("don't want END_STREAM, expecting data")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"content-type", "text/html; charset=utf-8"},
			{"content-length", strconv.Itoa(len(msg))},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}
		df := st.wantData()
		if !df.StreamEnded() {
			t.Error("expected DATA to have END_STREAM flag")
		}
		if got := string(df.Data()); got != msg {
			t.Errorf("got DATA %q; want %q", got, msg)
		}
	})
}

func TestServer_Response_Header_Flush_MidWrite(t *testing.T) {
	const msg = "<html>this is HTML"
	const msg2 = ", and this is the next chunk"
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		io.WriteString(w, msg)
		w.(http.Flusher).Flush()
		io.WriteString(w, msg2)
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("unexpected END_STREAM flag")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"content-type", "text/html; charset=utf-8"}, // sniffed
			// and no content-length
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}
		{
			df := st.wantData()
			if df.StreamEnded() {
				t.Error("unexpected END_STREAM flag")
			}
			if got := string(df.Data()); got != msg {
				t.Errorf("got DATA %q; want %q", got, msg)
			}
		}
		{
			df := st.wantData()
			if !df.StreamEnded() {
				t.Error("wanted END_STREAM flag on last data chunk")
			}
			if got := string(df.Data()); got != msg2 {
				t.Errorf("got DATA %q; want %q", got, msg2)
			}
		}
	})
}

func TestServer_Response_LargeWrite(t *testing.T) {
	const size = 1 << 20
	const maxFrameSize = 16 << 10
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		n, err := w.Write(bytes.Repeat([]byte("a"), size))
		if err != nil {
			return fmt.Errorf("Write error: %v", err)
		}
		if n != size {
			return fmt.Errorf("wrong size %d from Write", n)
		}
		return nil
	}, func(st *serverTester) {
		if err := st.fr.WriteSettings(
			Setting{SettingInitialWindowSize, 0},
			Setting{SettingMaxFrameSize, maxFrameSize},
		); err != nil {
			t.Fatal(err)
		}
		st.wantSettingsAck()

		getSlash(st) // make the single request

		// Give the handler quota to write:
		if err := st.fr.WriteWindowUpdate(1, size); err != nil {
			t.Fatal(err)
		}
		// Give the handler quota to write to connection-level
		// window as well
		if err := st.fr.WriteWindowUpdate(0, size); err != nil {
			t.Fatal(err)
		}
		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("unexpected END_STREAM flag")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"content-type", "text/plain; charset=utf-8"}, // sniffed
			// and no content-length
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}
		var bytes, frames int
		for {
			df := st.wantData()
			bytes += len(df.Data())
			frames++
			for _, b := range df.Data() {
				if b != 'a' {
					t.Fatal("non-'a' byte seen in DATA")
				}
			}
			if df.StreamEnded() {
				break
			}
		}
		if bytes != size {
			t.Errorf("Got %d bytes; want %d", bytes, size)
		}
		if want := int(size / maxFrameSize); frames < want || frames > want*2 {
			t.Errorf("Got %d frames; want %d", frames, size)
		}
	})
}

// Test that the handler can't write more than the client allows
func TestServer_Response_LargeWrite_FlowControlled(t *testing.T) {
	const size = 1 << 20
	const maxFrameSize = 16 << 10
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.(http.Flusher).Flush()
		n, err := w.Write(bytes.Repeat([]byte("a"), size))
		if err != nil {
			return fmt.Errorf("Write error: %v", err)
		}
		if n != size {
			return fmt.Errorf("wrong size %d from Write", n)
		}
		return nil
	}, func(st *serverTester) {
		// Set the window size to something explicit for this test.
		// It's also how much initial data we expect.
		const initWindowSize = 123
		if err := st.fr.WriteSettings(
			Setting{SettingInitialWindowSize, initWindowSize},
			Setting{SettingMaxFrameSize, maxFrameSize},
		); err != nil {
			t.Fatal(err)
		}
		st.wantSettingsAck()

		getSlash(st) // make the single request
		defer func() { st.fr.WriteRSTStream(1, ErrCodeCancel) }()

		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("unexpected END_STREAM flag")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}

		df := st.wantData()
		if got := len(df.Data()); got != initWindowSize {
			t.Fatalf("Initial window size = %d but got DATA with %d bytes", initWindowSize, got)
		}

		for _, quota := range []int{1, 13, 127} {
			if err := st.fr.WriteWindowUpdate(1, uint32(quota)); err != nil {
				t.Fatal(err)
			}
			df := st.wantData()
			if int(quota) != len(df.Data()) {
				t.Fatalf("read %d bytes after giving %d quota", len(df.Data()), quota)
			}
		}

		if err := st.fr.WriteRSTStream(1, ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	})
}

// Test that the handler blocked in a Write is unblocked if the server sends a RST_STREAM.
func TestServer_Response_RST_Unblocks_LargeWrite(t *testing.T) {
	const size = 1 << 20
	const maxFrameSize = 16 << 10
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.(http.Flusher).Flush()
		errc := make(chan error, 1)
		go func() {
			_, err := w.Write(bytes.Repeat([]byte("a"), size))
			errc <- err
		}()
		select {
		case err := <-errc:
			if err == nil {
				return errors.New("unexpected nil error from Write in handler")
			}
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("timeout waiting for Write in handler")
		}
	}, func(st *serverTester) {
		if err := st.fr.WriteSettings(
			Setting{SettingInitialWindowSize, 0},
			Setting{SettingMaxFrameSize, maxFrameSize},
		); err != nil {
			t.Fatal(err)
		}
		st.wantSettingsAck()

		getSlash(st) // make the single request

		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("unexpected END_STREAM flag")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}

		if err := st.fr.WriteRSTStream(1, ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	})
}

func TestServer_Response_Empty_Data_Not_FlowControlled(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.(http.Flusher).Flush()
		// Nothing; send empty DATA
		return nil
	}, func(st *serverTester) {
		// Handler gets no data quota:
		if err := st.fr.WriteSettings(Setting{SettingInitialWindowSize, 0}); err != nil {
			t.Fatal(err)
		}
		st.wantSettingsAck()

		getSlash(st) // make the single request

		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("unexpected END_STREAM flag")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}

		df := st.wantData()
		if got := len(df.Data()); got != 0 {
			t.Fatalf("unexpected %d DATA bytes; want 0", got)
		}
		if !df.StreamEnded() {
			t.Fatal("DATA didn't have END_STREAM")
		}
	})
}

func TestServer_Response_Automatic100Continue(t *testing.T) {
	const msg = "foo"
	const reply = "bar"
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		if v := r.Header.Get("Expect"); v != "" {
			t.Errorf("Expect header = %q; want empty", v)
		}
		buf := make([]byte, len(msg))
		// This read should trigger the 100-continue being sent.
		if n, err := io.ReadFull(r.Body, buf); err != nil || n != len(msg) || string(buf) != msg {
			return fmt.Errorf("ReadFull = %q, %v; want %q, nil", buf[:n], err, msg)
		}
		_, err := io.WriteString(w, reply)
		return err
	}, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader(":method", "POST", "expect", "100-continue"),
			EndStream:     false,
			EndHeaders:    true,
		})
		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("unexpected END_STREAM flag")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "100"},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Fatalf("Got headers %v; want %v", goth, wanth)
		}

		// Okay, they sent status 100, so we can send our
		// gigantic and/or sensitive "foo" payload now.
		st.writeData(1, true, []byte(msg))

		st.wantWindowUpdate(0, uint32(len(msg)))

		hf = st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("expected data to follow")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		goth = st.decodeHeader(hf.HeaderBlockFragment())
		wanth = [][2]string{
			{":status", "200"},
			{"content-type", "text/plain; charset=utf-8"},
			{"content-length", strconv.Itoa(len(reply))},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}

		df := st.wantData()
		if string(df.Data()) != reply {
			t.Errorf("Client read %q; want %q", df.Data(), reply)
		}
		if !df.StreamEnded() {
			t.Errorf("expect data stream end")
		}
	})
}

func TestServer_HandlerWriteErrorOnDisconnect(t *testing.T) {
	errc := make(chan error, 1)
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		p := []byte("some data.\n")
		for {
			_, err := w.Write(p)
			if err != nil {
				errc <- err
				return nil
			}
		}
	}, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     false,
			EndHeaders:    true,
		})
		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("unexpected END_STREAM flag")
		}
		if !hf.HeadersEnded() {
			t.Fatal("want END_HEADERS flag")
		}
		// Close the connection and wait for the handler to (hopefully) notice.
		st.cc.Close()
		select {
		case <-errc:
		case <-time.After(5 * time.Second):
			t.Error("timeout")
		}
	})
}

// So many response headers that the server needs to use CONTINUATION frames:
func TestServer_Response_ManyHeaders_With_Continuation(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		h := w.Header()
		for i := 0; i < 5000; i++ {
			h.Set(fmt.Sprintf("x-header-%d", i), fmt.Sprintf("x-value-%d", i))
		}
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if hf.HeadersEnded() {
			t.Fatal("got unwanted END_HEADERS flag")
		}
		n := 0
		for {
			n++
			cf := st.wantContinuation()
			if cf.HeadersEnded() {
				break
			}
		}
		if n < 5 {
			t.Errorf("Only got %d CONTINUATION frames; expected 5+ (currently 6)", n)
		}
	})
}

// This previously crashed (reported by Mathieu Lonjaret as observed
// while using Camlistore) because we got a DATA frame from the client
// after the handler exited and our logic at the time was wrong,
// keeping a stream in the map in stateClosed, which tickled an
// invariant check later when we tried to remove that stream (via
// defer sc.closeAllStreamsOnConnClose) when the serverConn serve loop
// ended.
func TestServer_NoCrash_HandlerClose_Then_ClientClose(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		// nothing
		return nil
	}, func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: st.encodeHeader(),
			EndStream:     false, // DATA is coming
			EndHeaders:    true,
		})
		hf := st.wantHeaders()
		if !hf.HeadersEnded() || !hf.StreamEnded() {
			t.Fatalf("want END_HEADERS+END_STREAM, got %v", hf)
		}

		// Sent when the a Handler closes while a client has
		// indicated it's still sending DATA:
		st.wantRSTStream(1, ErrCodeNo)

		// Now the handler has ended, so it's ended its
		// stream, but the client hasn't closed its side
		// (stateClosedLocal).  So send more data and verify
		// it doesn't crash with an internal invariant panic, like
		// it did before.
		st.writeData(1, true, []byte("foo"))

		// Get our flow control bytes back, since the handler didn't get them.
		st.wantWindowUpdate(0, uint32(len("foo")))

		// Sent after a peer sends data anyway (admittedly the
		// previous RST_STREAM might've still been in-flight),
		// but they'll get the more friendly 'cancel' code
		// first.
		st.wantRSTStream(1, ErrCodeStreamClosed)

		// Set up a bunch of machinery to record the panic we saw
		// previously.
		var (
			panMu    sync.Mutex
			panicVal interface{}
		)

		testHookOnPanicMu.Lock()
		testHookOnPanic = func(sc *serverConn, pv interface{}) bool {
			panMu.Lock()
			panicVal = pv
			panMu.Unlock()
			return true
		}
		testHookOnPanicMu.Unlock()

		// Now force the serve loop to end, via closing the connection.
		st.cc.Close()
		select {
		case <-st.sc.doneServing:
			// Loop has exited.
			panMu.Lock()
			got := panicVal
			panMu.Unlock()
			if got != nil {
				t.Errorf("Got panic: %v", got)
			}
		case <-time.After(5 * time.Second):
			t.Error("timeout")
		}
	})
}

func TestServer_Rejects_TLS10(t *testing.T) { testRejectTLS(t, tls.VersionTLS10) }
func TestServer_Rejects_TLS11(t *testing.T) { testRejectTLS(t, tls.VersionTLS11) }

func testRejectTLS(t *testing.T, max uint16) {
	st := newServerTester(t, nil, func(c *tls.Config) {
		c.MaxVersion = max
	})
	defer st.Close()
	gf := st.wantGoAway()
	if got, want := gf.ErrCode, ErrCodeInadequateSecurity; got != want {
		t.Errorf("Got error code %v; want %v", got, want)
	}
}

func TestServer_Rejects_TLSBadCipher(t *testing.T) {
	st := newServerTester(t, nil, func(c *tls.Config) {
		// Only list bad ones:
		c.CipherSuites = []uint16{
			tls.TLS_RSA_WITH_RC4_128_SHA,
			tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
			tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		}
	})
	defer st.Close()
	gf := st.wantGoAway()
	if got, want := gf.ErrCode, ErrCodeInadequateSecurity; got != want {
		t.Errorf("Got error code %v; want %v", got, want)
	}
}

func TestServer_Advertises_Common_Cipher(t *testing.T) {
	const requiredSuite = tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
	st := newServerTester(t, nil, func(c *tls.Config) {
		// Have the client only support the one required by the spec.
		c.CipherSuites = []uint16{requiredSuite}
	}, func(ts *util.MockServer) {
		var srv *http.Server = ts.Config
		// Have the server configured with no specific cipher suites.
		// This tests that Go's defaults include the required one.
		srv.TLSConfig = nil
	})
	defer st.Close()
	st.greet()
}

func (st *serverTester) onHeaderField(f hpack.HeaderField) error {
	if f.Name == "date" {
		return nil
	}
	st.decodedHeaders = append(st.decodedHeaders, [2]string{f.Name, f.Value})
	return nil
}

func (st *serverTester) decodeHeader(headerBlock []byte) (pairs [][2]string) {
	st.decodedHeaders = nil
	if _, err := st.hpackDec.Write(headerBlock); err != nil {
		st.t.Fatalf("hpack decoding error: %v", err)
	}
	if err := st.hpackDec.Close(); err != nil {
		st.t.Fatalf("hpack decoding error: %v", err)
	}
	return st.decodedHeaders
}

// testServerResponse sets up an idle HTTP/2 connection and lets you
// write a single request with writeReq, and then reply to it in some way with the provided handler,
// and then verify the output with the serverTester again (assuming the handler returns nil)
func testServerResponse(t testing.TB,
	handler func(http.ResponseWriter, *http.Request) error,
	client func(*serverTester),
) {
	errc := make(chan error, 1)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			t.Fatal("nil Body")
		}
		errc <- handler(w, r)
	})
	defer st.Close()

	donec := make(chan bool)
	go func() {
		defer close(donec)
		st.greet()
		client(st)
	}()

	select {
	case <-donec:
		return
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Error in handler: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for handler to finish")
	}
}

// readBodyHandler returns an http Handler func that reads len(want)
// bytes from r.Body and fails t if the contents read were not
// the value of want.
func readBodyHandler(t *testing.T, want string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, len(want))
		_, err := io.ReadFull(r.Body, buf)
		if err != nil {
			t.Error(err)
			return
		}
		if string(buf) != want {
			t.Errorf("read %q; want %q", buf, want)
		}
	}
}

func TestCompressionErrorOnClose(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		// No response body.
	})
	st.addLogFilter("connection error: COMPRESSION_ERROR")
	defer st.Close()
	st.greet()

	hbf := st.encodeHeader("foo", "bar")
	hbf = hbf[:len(hbf)-1] // truncate one byte from the end, so hpack.Decoder.Close fails.
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: hbf,
		EndStream:     true,
		EndHeaders:    true,
	})
	ga := st.wantGoAway()
	if ga.ErrCode != ErrCodeCompression {
		t.Errorf("GOAWAY err = %v; want ErrCodeCompression", ga.ErrCode)
	}
}

// test that a server handler can read trailers from a client
func TestServerReadsTrailers(t *testing.T) {
	const testBody = "some test body"
	writeReq := func(st *serverTester) {
		st.writeHeaders(HeadersFrameParam{
			StreamID:      1, // clients send odd numbers
			BlockFragment: st.encodeHeader("trailer", "Foo, Bar", "trailer", "Baz"),
			EndStream:     false,
			EndHeaders:    true,
		})
		st.writeData(1, false, []byte(testBody))
		st.writeHeaders(HeadersFrameParam{
			StreamID: 1, // clients send odd numbers
			BlockFragment: st.encodeHeaderRaw(
				"foo", "foov",
				"bar", "barv",
				"baz", "bazv",
				"surprise", "wasn't declared; shouldn't show up",
			),
			EndStream:  true,
			EndHeaders: true,
		})
	}
	checkReq := func(r *http.Request) {
		wantTrailer := http.Header{
			"Foo": nil,
			"Bar": nil,
			"Baz": nil,
		}
		if !reflect.DeepEqual(r.Trailer, wantTrailer) {
			t.Errorf("initial Trailer = %v; want %v", r.Trailer, wantTrailer)
		}
		slurp, err := ioutil.ReadAll(r.Body)
		if string(slurp) != testBody {
			t.Errorf("read body %q; want %q", slurp, testBody)
		}
		if err != nil {
			t.Fatalf("Body slurp: %v", err)
		}
		wantTrailerAfter := http.Header{
			"Foo": {"foov"},
			"Bar": {"barv"},
			"Baz": {"bazv"},
		}
		if !reflect.DeepEqual(r.Trailer, wantTrailerAfter) {
			t.Errorf("final Trailer = %v; want %v", r.Trailer, wantTrailerAfter)
		}
	}
	testServerRequest(t, writeReq, checkReq)
}

// test that a server handler can send trailers
func testServerWritesTrailers(t *testing.T, withFlush bool) {
	// See https://httpwg.github.io/specs/rfc7540.html#rfc.section.8.1.3
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Trailer", "Server-Trailer-A, Server-Trailer-B")
		w.Header().Add("Trailer", "Server-Trailer-C")
		w.Header().Add("Trailer", "Transfer-Encoding, Content-Length, Trailer") // filtered

		// Regular headers:
		w.Header().Set("Foo", "Bar")
		w.Header().Set("Content-Length", "5") // len("Hello")

		io.WriteString(w, "Hello")
		if withFlush {
			w.(http.Flusher).Flush()
		}
		w.Header().Set("Server-Trailer-A", "valuea")
		w.Header().Set("Server-Trailer-C", "valuec") // skipping B
		// After a flush, random keys like Server-Surprise shouldn't show up:
		w.Header().Set("Server-Surpise", "surprise! this isn't predeclared!")
		// But we do permit promoting keys to trailers after a
		// flush if they start with the magic
		// otherwise-invalid "Trailer:" prefix:
		w.Header().Set("Trailer:Post-Header-Trailer", "hi1")
		w.Header().Set("Trailer:post-header-trailer2", "hi2")
		w.Header().Set("Trailer:Range", "invalid")
		w.Header().Set("Trailer:Foo\x01Bogus", "invalid")
		w.Header().Set("Transfer-Encoding", "should not be included; Forbidden by RFC 2616 14.40")
		w.Header().Set("Content-Length", "should not be included; Forbidden by RFC 2616 14.40")
		w.Header().Set("Trailer", "should not be included; Forbidden by RFC 2616 14.40")
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if hf.StreamEnded() {
			t.Fatal("response HEADERS had END_STREAM")
		}
		if !hf.HeadersEnded() {
			t.Fatal("response HEADERS didn't have END_HEADERS")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"foo", "Bar"},
			{"trailer", "Server-Trailer-A, Server-Trailer-B"},
			{"trailer", "Server-Trailer-C"},
			{"trailer", "Transfer-Encoding, Content-Length, Trailer"},
			{"content-type", "text/plain; charset=utf-8"},
			{"content-length", "5"},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Header mismatch.\n got: %v\nwant: %v", goth, wanth)
		}
		df := st.wantData()
		if string(df.Data()) != "Hello" {
			t.Fatalf("Client read %q; want Hello", df.Data())
		}
		if df.StreamEnded() {
			t.Fatalf("data frame had STREAM_ENDED")
		}
		tf := st.wantHeaders() // for the trailers
		if !tf.StreamEnded() {
			t.Fatalf("trailers HEADERS lacked END_STREAM")
		}
		if !tf.HeadersEnded() {
			t.Fatalf("trailers HEADERS lacked END_HEADERS")
		}
		wanth = [][2]string{
			{"post-header-trailer", "hi1"},
			{"post-header-trailer2", "hi2"},
			{"server-trailer-a", "valuea"},
			{"server-trailer-c", "valuec"},
		}
		goth = st.decodeHeader(tf.HeaderBlockFragment())
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Header mismatch.\n got: %v\nwant: %v", goth, wanth)
		}
	})
}

// validate transmitted header field names & values
// golang.org/issue/14048
func TestServerDoesntWriteInvalidHeaders(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Add("OK1", "x")
		w.Header().Add("Bad:Colon", "x") // colon (non-token byte) in key
		w.Header().Add("Bad1\x00", "x")  // null in key
		w.Header().Add("Bad2", "x\x00y") // null in value
		return nil
	}, func(st *serverTester) {
		getSlash(st)
		hf := st.wantHeaders()
		if !hf.StreamEnded() {
			t.Error("response HEADERS lacked END_STREAM")
		}
		if !hf.HeadersEnded() {
			t.Fatal("response HEADERS didn't have END_HEADERS")
		}
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "200"},
			{"ok1", "x"},
			{"content-type", "text/plain; charset=utf-8"},
			{"content-length", "0"},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Header mismatch.\n got: %v\nwant: %v", goth, wanth)
		}
	})
}

func BenchmarkServerGets(b *testing.B) {
	defer disableGoroutineTracking()()
	b.ReportAllocs()

	const msg = "Hello, world"
	st := newServerTester(b, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, msg)
	})
	defer st.Close()
	st.greet()

	// Give the server quota to reply. (plus it has the the 64KB)
	if err := st.fr.WriteWindowUpdate(0, uint32(b.N*len(msg))); err != nil {
		b.Fatal(err)
	}

	for i := 0; i < b.N; i++ {
		id := 1 + uint32(i)*2
		st.writeHeaders(HeadersFrameParam{
			StreamID:      id,
			BlockFragment: st.encodeHeader(),
			EndStream:     true,
			EndHeaders:    true,
		})
		st.wantHeaders()
		df := st.wantData()
		if !df.StreamEnded() {
			b.Fatalf("DATA didn't have END_STREAM; got %v", df)
		}
	}
}

func BenchmarkServerPosts(b *testing.B) {
	defer disableGoroutineTracking()()
	b.ReportAllocs()

	const msg = "Hello, world"
	st := newServerTester(b, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, msg)
	})
	defer st.Close()
	st.greet()

	// Give the server quota to reply. (plus it has the the 64KB)
	if err := st.fr.WriteWindowUpdate(0, uint32(b.N*len(msg))); err != nil {
		b.Fatal(err)
	}

	for i := 0; i < b.N; i++ {
		id := 1 + uint32(i)*2
		st.writeHeaders(HeadersFrameParam{
			StreamID:      id,
			BlockFragment: st.encodeHeader(":method", "POST"),
			EndStream:     false,
			EndHeaders:    true,
		})
		st.writeData(id, true, nil)
		st.wantHeaders()
		df := st.wantData()
		if !df.StreamEnded() {
			b.Fatalf("DATA didn't have END_STREAM; got %v", df)
		}
	}
}

// go-fuzz bug, originally reported at https://github.com/bradfitz/http2/issues/53
// Verify we don't hang.
func TestIssue53(t *testing.T) {
	const data = "PRI * HTTP/2.0\r\n\r\nSM" +
		"\r\n\r\n\x00\x00\x00\x01\ainfinfin\ad"
	s := &http.Server{
		ErrorLog: log.New(io.MultiWriter(stderrv(), twriter{t: t}), "", log.LstdFlags),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Write([]byte("hello"))
		}),
	}
	s2 := &Server{
		MaxReadFrameSize:             1 << 16,
		PermitProhibitedCipherSuites: true,
	}
	c := &issue53Conn{[]byte(data), false, false}
	s2.ServeConn(c, &ServeConnOpts{BaseConfig: s})
	if !c.closed {
		t.Fatal("connection is not closed")
	}
}

type issue53Conn struct {
	data    []byte
	closed  bool
	written bool
}

func (c *issue53Conn) Read(b []byte) (n int, err error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n = copy(b, c.data)
	c.data = c.data[n:]
	return
}

func (c *issue53Conn) Write(b []byte) (n int, err error) {
	c.written = true
	return len(b), nil
}

func (c *issue53Conn) Close() error {
	c.closed = true
	return nil
}

func (c *issue53Conn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49706}
}
func (c *issue53Conn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49706}
}
func (c *issue53Conn) SetDeadline(t time.Time) error      { return nil }
func (c *issue53Conn) SetReadDeadline(t time.Time) error  { return nil }
func (c *issue53Conn) SetWriteDeadline(t time.Time) error { return nil }

// golang.org/issue/12895
func TestConfigureServer(t *testing.T) {
	tests := []struct {
		name    string
		in      http.Server
		wantErr string
	}{
		{
			name: "empty server",
			in:   http.Server{},
		},
		{
			name: "just the required cipher suite",
			in: http.Server{
				TLSConfig: &tls.Config{
					CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
				},
			},
		},
		{
			name: "required after bad",
			in: http.Server{
				TLSConfig: &tls.Config{
					CipherSuites: []uint16{tls.TLS_RSA_WITH_RC4_128_SHA, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
				},
			},
			wantErr: "contains an HTTP/2-approved cipher suite (0xc02f), but it comes after",
		},
		{
			name: "bad after required",
			in: http.Server{
				TLSConfig: &tls.Config{
					CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_RSA_WITH_RC4_128_SHA},
				},
			},
		},
	}
	for _, tt := range tests {
		err := ConfigureServer(&tt.in, nil)
		if (err != nil) != (tt.wantErr != "") {
			if tt.wantErr != "" {
				t.Errorf("%s: success, but want error", tt.name)
			} else {
				t.Errorf("%s: unexpected error: %v", tt.name, err)
			}
		}
		if err != nil && tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v; want substring %q", tt.name, err, tt.wantErr)
		}
		if err == nil && !tt.in.TLSConfig.PreferServerCipherSuites {
			t.Errorf("%s: PreferServerCipherSuite is false; want true", tt.name)
		}
	}
}

func TestServerRejectHeadWithBody(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		// No response body.
	})
	defer st.Close()
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: st.encodeHeader(":method", "HEAD"),
		EndStream:     false, // what we're testing, a bogus HEAD request with body
		EndHeaders:    true,
	})
	st.wantRSTStream(1, ErrCodeProtocol)
}

func TestServerNoAutoContentLengthOnHead(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		// No response body. (or smaller than one frame)
	})
	defer st.Close()
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1, // clients send odd numbers
		BlockFragment: st.encodeHeader(":method", "HEAD"),
		EndStream:     true,
		EndHeaders:    true,
	})
	h := st.wantHeaders()
	headers := st.decodeHeader(h.HeaderBlockFragment())
	want := [][2]string{
		{":status", "200"},
		{"content-type", "text/plain; charset=utf-8"},
	}
	if !reflect.DeepEqual(headers, want) {
		t.Errorf("Headers mismatch.\n got: %q\nwant: %q\n", headers, want)
	}
}

// golang.org/issue/13495
func TestServerNoDuplicateContentType(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = []string{""}
		fmt.Fprintf(w, "<html><head></head><body>hi</body></html>")
	})
	defer st.Close()
	st.greet()
	st.writeHeaders(HeadersFrameParam{
		StreamID:      1,
		BlockFragment: st.encodeHeader(),
		EndStream:     true,
		EndHeaders:    true,
	})
	h := st.wantHeaders()
	headers := st.decodeHeader(h.HeaderBlockFragment())
	want := [][2]string{
		{":status", "200"},
		{"content-type", ""},
		{"content-length", "41"},
	}
	if !reflect.DeepEqual(headers, want) {
		t.Errorf("Headers mismatch.\n got: %q\nwant: %q\n", headers, want)
	}
}

func disableGoroutineTracking() (restore func()) {
	old := gotrack.DebugGoroutines
	gotrack.DebugGoroutines = false
	return func() { gotrack.DebugGoroutines = old }
}

func BenchmarkServer_GetRequest(b *testing.B) {
	defer disableGoroutineTracking()()
	b.ReportAllocs()
	const msg = "Hello, world."
	st := newServerTester(b, func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(ioutil.Discard, r.Body)
		if err != nil || n > 0 {
			b.Errorf("Read %d bytes, error %v; want 0 bytes.", n, err)
		}
		io.WriteString(w, msg)
	})
	defer st.Close()

	st.greet()
	// Give the server quota to reply. (plus it has the the 64KB)
	if err := st.fr.WriteWindowUpdate(0, uint32(b.N*len(msg))); err != nil {
		b.Fatal(err)
	}
	hbf := st.encodeHeader(":method", "GET")
	for i := 0; i < b.N; i++ {
		streamID := uint32(1 + 2*i)
		st.writeHeaders(HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: hbf,
			EndStream:     true,
			EndHeaders:    true,
		})
		st.wantHeaders()
		st.wantData()
	}
}

func BenchmarkServer_PostRequest(b *testing.B) {
	defer disableGoroutineTracking()()
	b.ReportAllocs()
	const msg = "Hello, world."
	st := newServerTester(b, func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(ioutil.Discard, r.Body)
		if err != nil || n > 0 {
			b.Errorf("Read %d bytes, error %v; want 0 bytes.", n, err)
		}
		io.WriteString(w, msg)
	})
	defer st.Close()
	st.greet()
	// Give the server quota to reply. (plus it has the the 64KB)
	if err := st.fr.WriteWindowUpdate(0, uint32(b.N*len(msg))); err != nil {
		b.Fatal(err)
	}
	hbf := st.encodeHeader(":method", "POST")
	for i := 0; i < b.N; i++ {
		streamID := uint32(1 + 2*i)
		st.writeHeaders(HeadersFrameParam{
			StreamID:      streamID,
			BlockFragment: hbf,
			EndStream:     false,
			EndHeaders:    true,
		})
		st.writeData(streamID, true, nil)
		st.wantHeaders()
		st.wantData()
	}
}

type connStateConn struct {
	net.Conn
	cs tls.ConnectionState
}

func (c connStateConn) ConnectionState() tls.ConnectionState { return c.cs }

// golang.org/issue/12737 -- handle any net.Conn, not just
// *tls.Conn.
func TestServerHandleCustomConn(t *testing.T) {
	var s Server
	c1, c2 := net.Pipe()
	clientDone := make(chan struct{})
	handlerDone := make(chan struct{})
	var req *http.Request
	go func() {
		defer close(clientDone)
		defer c2.Close()
		fr := NewFramer(c2, c2)
		io.WriteString(c2, ClientPreface)
		fr.WriteSettings()
		fr.WriteSettingsAck()
		f, err := fr.ReadFrame()
		if err != nil {
			t.Error(err)
			return
		}
		if sf, ok := f.(*SettingsFrame); !ok || sf.IsAck() {
			t.Errorf("Got %v; want non-ACK SettingsFrame", summarizeFrame(f))
			return
		}
		f, err = fr.ReadFrame()
		if err != nil {
			t.Error(err)
			return
		}
		if sf, ok := f.(*SettingsFrame); !ok || !sf.IsAck() {
			t.Errorf("Got %v; want ACK SettingsFrame", summarizeFrame(f))
			return
		}
		var henc hpackEncoder
		fr.WriteHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: henc.encodeHeaderRaw(t, ":method", "GET", ":path", "/", ":scheme", "https", ":authority", "foo.com"),
			EndStream:     true,
			EndHeaders:    true,
		})
		go io.Copy(ioutil.Discard, c2)
		<-handlerDone
	}()
	const testString = "my custom ConnectionState"
	fakeConnState := tls.ConnectionState{
		ServerName: testString,
		Version:    tls.VersionTLS12,
	}
	go s.ServeConn(connStateConn{c1, fakeConnState}, &ServeConnOpts{
		BaseConfig: &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				req = r
			}),
                        ReadTimeout: 5*time.Second,
		}})
	select {
	case <-clientDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for handler")
	}
	if req.TLS == nil {
		t.Fatalf("Request.TLS is nil. Got: %#v", req)
	}
	if req.TLS.ServerName != testString {
		t.Fatalf("Request.TLS = %+v; want ServerName of %q", req.TLS, testString)
	}
}

// golang.org/issue/14214
func TestServer_Rejects_ConnHeaders(t *testing.T) {
	testServerResponse(t, func(w http.ResponseWriter, r *http.Request) error {
		t.Errorf("should not get to Handler")
		return nil
	}, func(st *serverTester) {
		st.bodylessReq1("connection", "foo")
		hf := st.wantHeaders()
		goth := st.decodeHeader(hf.HeaderBlockFragment())
		wanth := [][2]string{
			{":status", "400"},
			{"content-type", "text/plain; charset=utf-8"},
			{"content-length", "51"},
		}
		if !reflect.DeepEqual(goth, wanth) {
			t.Errorf("Got headers %v; want %v", goth, wanth)
		}
	})
}

func TestCheckValidHTTP2Request(t *testing.T) {
	tests := []struct {
		req  *http.Request
		want error
	}{
		{
			req:  &http.Request{Header: http.Header{"Te": {"trailers"}}},
			want: nil,
		},
		{
			req:  &http.Request{Header: http.Header{"Te": {"trailers", "bogus"}}},
			want: errors.New(`request header "TE" may only be "trailers" in HTTP/2`),
		},
		{
			req:  &http.Request{Header: http.Header{"Foo": {""}}},
			want: nil,
		},
		{
			req:  &http.Request{Header: http.Header{"Connection": {""}}},
			want: errors.New(`request header "Connection" is not valid in HTTP/2`),
		},
		{
			req:  &http.Request{Header: http.Header{"Proxy-Connection": {""}}},
			want: errors.New(`request header "Proxy-Connection" is not valid in HTTP/2`),
		},
		{
			req:  &http.Request{Header: http.Header{"Keep-Alive": {""}}},
			want: errors.New(`request header "Keep-Alive" is not valid in HTTP/2`),
		},
		{
			req:  &http.Request{Header: http.Header{"Upgrade": {""}}},
			want: errors.New(`request header "Upgrade" is not valid in HTTP/2`),
		},
	}
	for i, tt := range tests {
		got := checkValidHTTP2Request(tt.req)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%d. checkValidHTTP2Request = %v; want %v", i, got, tt.want)
		}
	}
}

type twriter struct {
	t  testing.TB
	st *serverTester // optional
}

func (w twriter) Write(p []byte) (n int, err error) {
	if w.st != nil {
		ps := string(p)
		for _, phrase := range w.st.logFilter {
			if strings.Contains(ps, phrase) {
				return len(p), nil // no logging
			}
		}
	}
	w.t.Logf("%s", p)
	return len(p), nil
}

// LocalhostCert is a PEM-encoded TLS cert with SAN IPs
// "127.0.0.1" and "[::1]", expiring at Jan 29 16:00:00 2084 GMT.
// generated from src/crypto/tls:
// go run generate_cert.go  --rsa-bits 1024 --host 127.0.0.1,::1,example.com --ca --start-date "Jan 1 00:00:00 1970" --duration=1000000h
var localhostCert = []byte(`-----BEGIN CERTIFICATE-----
MIICEzCCAXygAwIBAgIQMIMChMLGrR+QvmQvpwAU6zANBgkqhkiG9w0BAQsFADAS
MRAwDgYDVQQKEwdBY21lIENvMCAXDTcwMDEwMTAwMDAwMFoYDzIwODQwMTI5MTYw
MDAwWjASMRAwDgYDVQQKEwdBY21lIENvMIGfMA0GCSqGSIb3DQEBAQUAA4GNADCB
iQKBgQDuLnQAI3mDgey3VBzWnB2L39JUU4txjeVE6myuDqkM/uGlfjb9SjY1bIw4
iA5sBBZzHi3z0h1YV8QPuxEbi4nW91IJm2gsvvZhIrCHS3l6afab4pZBl2+XsDul
rKBxKKtD1rGxlG4LjncdabFn9gvLZad2bSysqz/qTAUStTvqJQIDAQABo2gwZjAO
BgNVHQ8BAf8EBAMCAqQwEwYDVR0lBAwwCgYIKwYBBQUHAwEwDwYDVR0TAQH/BAUw
AwEB/zAuBgNVHREEJzAlggtleGFtcGxlLmNvbYcEfwAAAYcQAAAAAAAAAAAAAAAA
AAAAATANBgkqhkiG9w0BAQsFAAOBgQCEcetwO59EWk7WiJsG4x8SY+UIAA+flUI9
tyC4lNhbcF2Idq9greZwbYCqTTTr2XiRNSMLCOjKyI7ukPoPjo16ocHj+P3vZGfs
h1fIw3cSS2OolhloGw/XM6RWPWtPAlGykKLciQrBru5NAPvCMsb/I1DAceTiotQM
fblo6RBxUQ==
-----END CERTIFICATE-----`)

// LocalhostKey is the private key for localhostCert.
var localhostKey = []byte(`-----BEGIN RSA PRIVATE KEY-----
MIICXgIBAAKBgQDuLnQAI3mDgey3VBzWnB2L39JUU4txjeVE6myuDqkM/uGlfjb9
SjY1bIw4iA5sBBZzHi3z0h1YV8QPuxEbi4nW91IJm2gsvvZhIrCHS3l6afab4pZB
l2+XsDulrKBxKKtD1rGxlG4LjncdabFn9gvLZad2bSysqz/qTAUStTvqJQIDAQAB
AoGAGRzwwir7XvBOAy5tM/uV6e+Zf6anZzus1s1Y1ClbjbE6HXbnWWF/wbZGOpet
3Zm4vD6MXc7jpTLryzTQIvVdfQbRc6+MUVeLKwZatTXtdZrhu+Jk7hx0nTPy8Jcb
uJqFk541aEw+mMogY/xEcfbWd6IOkp+4xqjlFLBEDytgbIECQQDvH/E6nk+hgN4H
qzzVtxxr397vWrjrIgPbJpQvBsafG7b0dA4AFjwVbFLmQcj2PprIMmPcQrooz8vp
jy4SHEg1AkEA/v13/5M47K9vCxmb8QeD/asydfsgS5TeuNi8DoUBEmiSJwma7FXY
fFUtxuvL7XvjwjN5B30pNEbc6Iuyt7y4MQJBAIt21su4b3sjXNueLKH85Q+phy2U
fQtuUE9txblTu14q3N7gHRZB4ZMhFYyDy8CKrN2cPg/Fvyt0Xlp/DoCzjA0CQQDU
y2ptGsuSmgUtWj3NM9xuwYPm+Z/F84K6+ARYiZ6PYj013sovGKUFfYAqVXVlxtIX
qyUBnu3X9ps8ZfjLZO7BAkEAlT4R5Yl6cGhaJQYZHOde3JEMhNRcVFMO8dJDaFeo
f9Oeos0UUothgiDktdQHxdNEwLjQf7lJJBzV+5OtwswCWA==
-----END RSA PRIVATE KEY-----`)
