package guard_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/guard"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/kernel/singbox"
	"github.com/cedar2025/xboard-node/internal/kernel/xray"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/gorilla/websocket"
)

// Exercise real VLESS WebSocket handshakes through the guard on both kernels.
func TestE2EVLESSWebSocket(t *testing.T) {
	t.Run("xray", func(t *testing.T) {
		testVLESSWebSocket(t, xray.New(config.KernelConfig{Type: "xray", LogLevel: "warn"}))
	})
	t.Run("singbox", func(t *testing.T) {
		testVLESSWebSocket(t, singbox.New(config.KernelConfig{Type: "singbox", LogLevel: "warn"}))
	})
}

func testVLESSWebSocket(t *testing.T, inner kernel.Kernel) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"
	k := guard.Wrap(inner, config.GuardConfig{
		Enabled: true, Blacklist: filepath.Join(t.TempDir(), "bl.txt"), RealIPHeader: "CF-Connecting-IP",
	})
	nc := &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: port, Network: "ws",
		NetworkSettings: map[string]any{"path": "/mm", "host": "ws.example.com"},
		CustomRouteRules: []model.CustomRouteRule{{
			Match:  model.RouteMatch{IPCIDRs: []string{"127.0.0.1/32"}},
			Action: model.RouteAction{Type: "direct"},
		}},
	}
	if err := k.Start(nc, []model.UserSpec{{ID: 7, UUID: uuid}}, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer k.Stop()
	url := "ws://127.0.0.1:" + strconv.Itoa(port)

	// Probe on the wrong path → banned, honeypot.
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Server") != "nginx/1.18.0" {
		t.Fatalf("probe got %v", resp.Header)
	}

	h := http.Header{"Host": {"ws.example.com"}, "Cf-Connecting-Ip": {"203.0.113.9"}, "X-Forwarded-For": {"6.6.6.6"}}
	ws, _, err := websocket.DefaultDialer.Dial(url+"/mm", h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	// VLESS request header: ver, uuid, addons=0, cmd=tcp, port, atyp=ipv4, addr
	var req bytes.Buffer
	req.WriteByte(0)
	id := mustUUID(t, uuid)
	req.Write(id[:])
	req.WriteByte(0)
	req.WriteByte(1)
	binary.Write(&req, binary.BigEndian, uint16(echo.Addr().(*net.TCPAddr).Port))
	req.WriteByte(1)
	req.Write([]byte{127, 0, 0, 1})
	req.WriteString("hello-guard")
	ws.WriteMessage(websocket.BinaryMessage, req.Bytes())

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	// Check both the VLESS response and payload echoed through the real kernel.
	var received []byte
	for len(received) < 2+len("hello-guard") {
		_, m, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read VLESS response: %v (received %q)", err, received)
		}
		received = append(received, m...)
	}
	if !bytes.Equal(received, append([]byte{0, 0}, []byte("hello-guard")...)) {
		t.Fatalf("VLESS echo response = %q", received)
	}
}

func mustUUID(t *testing.T, s string) (out [16]byte) {
	var hex []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			hex = append(hex, s[i])
		}
	}
	for i := 0; i < 16; i++ {
		v, err := strconv.ParseUint(string(hex[2*i:2*i+2]), 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = byte(v)
	}
	return
}
