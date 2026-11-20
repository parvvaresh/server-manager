package main

// A small WebSocket server (RFC 6455): just what the dashboard's terminal
// needs, so sshm keeps to the standard library. It speaks to one browser on
// the loopback interface, so there are no extensions and no subprotocols,
// and messages are capped at wsMaxMessage.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	wsContinuation = 0x0
	wsText         = 0x1
	wsBinary       = 0x2
	wsClose        = 0x8
	wsPing         = 0x9
	wsPong         = 0xa

	wsMaxMessage = 1 << 20
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

var errWSProtocol = errors.New("websocket protocol error")

type wsConn struct {
	conn      net.Conn
	br        *bufio.Reader
	wmu       sync.Mutex
	closeOnce sync.Once
}

// headerHas reports whether a comma separated header lists token.
func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), token) {
				return true
			}
		}
	}
	return false
}

func wsAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// wsUpgrade completes the opening handshake and takes over the connection.
// On failure it has already answered the request.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if r.Method != http.MethodGet || !headerHas(r.Header, "Connection", "upgrade") ||
		!headerHas(r.Header, "Upgrade", "websocket") {
		http.Error(w, "expected a WebSocket upgrade", http.StatusBadRequest)
		return nil, errWSProtocol
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported WebSocket version", http.StatusUpgradeRequired)
		return nil, errWSProtocol
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if k, err := base64.StdEncoding.DecodeString(key); err != nil || len(k) != 16 {
		http.Error(w, "bad Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errWSProtocol
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot upgrade this connection", http.StatusInternalServerError)
		return nil, errWSProtocol
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, br: brw.Reader}, nil
}

// ReadMessage returns the next text or binary message, answering pings on
// the way. It returns io.EOF once the browser closes the connection.
func (c *wsConn) ReadMessage() (op byte, msg []byte, err error) {
	for {
		fin, fop, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch fop {
		case wsPing:
			if err := c.writeFrame(wsPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case wsPong:
			continue
		case wsClose:
			if len(payload) > 2 {
				payload = payload[:2]
			}
			_ = c.writeFrame(wsClose, payload)
			return 0, nil, io.EOF
		case wsContinuation:
			if op == 0 {
				return 0, nil, errWSProtocol
			}
		case wsText, wsBinary:
			if op != 0 {
				return 0, nil, errWSProtocol
			}
			op = fop
		default:
			return 0, nil, errWSProtocol
		}
		if len(msg)+len(payload) > wsMaxMessage {
			return 0, nil, errWSProtocol
		}
		msg = append(msg, payload...)
		if fin {
			return op, msg, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0f
	if h[0]&0x70 != 0 || h[1]&0x80 == 0 {
		// Reserved bits need an extension we never agreed to, and every
		// frame from a browser must be masked.
		return false, 0, nil, errWSProtocol
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= wsClose && (n > 125 || !fin) {
		return false, 0, nil, errWSProtocol
	}
	if n > wsMaxMessage {
		return false, 0, nil, errWSProtocol
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return fin, op, payload, nil
}

// WriteMessage sends one unfragmented message. It is safe to call from
// several goroutines.
func (c *wsConn) WriteMessage(op byte, p []byte) error {
	return c.writeFrame(op, p)
}

func (c *wsConn) writeFrame(op byte, p []byte) error {
	hdr := make([]byte, 0, 10)
	hdr = append(hdr, 0x80|op)
	switch n := len(p); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	bufs := net.Buffers{hdr, p}
	_, err := bufs.WriteTo(c.conn)
	return err
}

// Close sends a normal close frame and drops the connection.
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		_ = c.writeFrame(wsClose, []byte{0x03, 0xe8}) // 1000: normal closure
		err = c.conn.Close()
	})
	return err
}
