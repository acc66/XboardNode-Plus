package model

import "testing"

func TestSSWebSocketPluginSelectsXray(t *testing.T) {
	n := &NodeSpec{Protocol: "shadowsocks", Plugin: "v2ray-plugin", PluginOpt: "mode=websocket;path=/mm;host=a.com"}
	path, host, tls, ok := SSWebSocketPlugin(n)
	if !ok || path != "/mm" || host != "a.com" || tls {
		t.Fatalf("got %q %q %v %v", path, host, tls, ok)
	}
	if EffectiveNetwork(n) != "ws" {
		t.Fatal("effective network should be ws")
	}
	if k, _ := ResolveEffectiveKernelType(n, KernelAuto); k != KernelXray {
		t.Fatalf("auto kernel = %s, want xray", k)
	}
	if k, _ := ResolveEffectiveKernelType(&NodeSpec{Protocol: "shadowsocks"}, KernelAuto); k != KernelSingBox {
		t.Fatalf("plain ss auto kernel = %s, want singbox", k)
	}
	if _, _, _, ok := SSWebSocketPlugin(&NodeSpec{Protocol: "shadowsocks", Plugin: "obfs-local", PluginOpt: "obfs=http"}); ok {
		t.Fatal("obfs is not websocket")
	}
}
