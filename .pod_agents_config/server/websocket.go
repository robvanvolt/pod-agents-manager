package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*webSocketConn, error) {
	if !headerHasToken(r.Header, "Connection", "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("websocket upgrade required")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, fmt.Errorf("missing websocket key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("websocket hijack unsupported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	accept := webSocketAcceptKey(key)
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
	fmt.Fprintf(rw, "Upgrade: websocket\r\n")
	fmt.Fprintf(rw, "Connection: Upgrade\r\n")
	fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &webSocketConn{conn: conn, br: rw.Reader}, nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func webSocketAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func (ws *webSocketConn) Close() error {
	return ws.conn.Close()
}

func (ws *webSocketConn) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.WriteFrame(1, b)
}

func (ws *webSocketConn) WriteFrame(opcode byte, payload []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	header := []byte{0x80 | opcode}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 0xffff:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(len(payload)))
		header = append(header, ext[:]...)
	}
	if _, err := ws.conn.Write(header); err != nil {
		return err
	}
	_, err := ws.conn.Write(payload)
	return err
}

func (ws *webSocketConn) ReadText() ([]byte, error) {
	for {
		opcode, payload, err := ws.ReadFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 1:
			return payload, nil
		case 8:
			ws.WriteFrame(8, nil)
			return nil, io.EOF
		case 9:
			ws.WriteFrame(10, payload)
		}
	}
}

func (ws *webSocketConn) ReadFrame() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(ws.br, head[:]); err != nil {
		return 0, nil, err
	}
	opcode := head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(ws.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(ws.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 64*1024 {
		return 0, nil, fmt.Errorf("websocket frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(ws.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(ws.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

type webSocketConn struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
}
