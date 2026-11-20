package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A program on the pseudo-terminal sees a terminal of the size we set, and
// sees it change.
func TestPTYSizeAndOutput(t *testing.T) {
	master, tty, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := setWinsize(master, 101, 33); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `stty size; read x; stty size; echo "got $x"`)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tty.Close()

	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(master) // ends with EIO once sh exits
		out <- string(b)
	}()
	time.Sleep(300 * time.Millisecond)
	if err := setWinsize(master, 120, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := master.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-out:
		s = strings.ReplaceAll(s, "\r", "")
		for _, want := range []string{"33 101\n", "40 120\n", "got hello\n"} {
			if !strings.Contains(s, want) {
				t.Errorf("output %q lacks %q", s, want)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading the terminal did not end after the program exited")
	}
}

// Closing the master wakes a Read that is waiting on it.
func TestPTYCloseUnblocksRead(t *testing.T) {
	master, tty, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	done := make(chan error)
	go func() {
		_, err := master.Read(make([]byte, 16))
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	master.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a closed terminal")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read stayed blocked after Close")
	}
}

func TestWSAcceptKey(t *testing.T) {
	// The example from RFC 6455, section 1.3.
	if got := wsAcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept key %q", got)
	}
}

// wsClientFrame builds a masked frame, as a browser sends it.
func wsClientFrame(fin bool, op byte, p []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	f := []byte{b0}
	switch n := len(p); {
	case n < 126:
		f = append(f, 0x80|byte(n))
	case n <= 0xffff:
		f = append(f, 0x80|126, byte(n>>8), byte(n))
	default:
		f = append(f, 0x80|127)
		f = binary.BigEndian.AppendUint64(f, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	f = append(f, mask[:]...)
	for i, c := range p {
		f = append(f, c^mask[i&3])
	}
	return f
}

// readServerFrame reads one unmasked frame from the server.
func readServerFrame(t *testing.T, br *bufio.Reader) (byte, []byte) {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		t.Fatal(err)
	}
	if h[1]&0x80 != 0 {
		t.Fatal("server frames must not be masked")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		io.ReadFull(br, b[:])
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		io.ReadFull(br, b[:])
		n = binary.BigEndian.Uint64(b[:])
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(br, p); err != nil {
		t.Fatal(err)
	}
	return h[0] & 0x0f, p
}

// An echo server: the handshake, fragmented and large messages, pings and
// the closing handshake all behave.
func TestWSEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := wsUpgrade(w, r)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			op, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if err := ws.WriteMessage(op, msg); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("handshake: %d %v", resp.StatusCode, resp.Header)
	}

	// a plain text message
	conn.Write(wsClientFrame(true, wsText, []byte("hi")))
	if op, p := readServerFrame(t, br); op != wsText || string(p) != "hi" {
		t.Fatalf("echo: %d %q", op, p)
	}
	// a ping in the middle of a fragmented binary message
	conn.Write(wsClientFrame(false, wsBinary, []byte("ab")))
	conn.Write(wsClientFrame(true, wsPing, []byte("p")))
	conn.Write(wsClientFrame(true, wsContinuation, []byte("cd")))
	if op, p := readServerFrame(t, br); op != wsPong || string(p) != "p" {
		t.Fatalf("pong: %d %q", op, p)
	}
	if op, p := readServerFrame(t, br); op != wsBinary || string(p) != "abcd" {
		t.Fatalf("fragmented: %d %q", op, p)
	}
	// 16 and 64 bit lengths
	for _, n := range []int{300, 70000} {
		big := bytes.Repeat([]byte{'x'}, n)
		conn.Write(wsClientFrame(true, wsBinary, big))
		if _, p := readServerFrame(t, br); !bytes.Equal(p, big) {
			t.Fatalf("%d byte message came back as %d bytes", n, len(p))
		}
	}
	// closing handshake
	conn.Write(wsClientFrame(true, wsClose, []byte{0x03, 0xe8}))
	if op, _ := readServerFrame(t, br); op != wsClose {
		t.Fatalf("expected a close frame, got %d", op)
	}
}

// Unmasked frames from a client are refused.
func TestWSRejectsUnmasked(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	ws := &wsConn{conn: c2, br: bufio.NewReader(c2)}
	go c1.Write([]byte{0x81, 0x02, 'h', 'i'})
	if _, _, err := ws.ReadMessage(); err != errWSProtocol {
		t.Fatalf("got %v", err)
	}
}
