package model

import "strings"

// SSWebSocketPlugin reports whether a shadowsocks node uses v2ray-plugin /
// xray-plugin in websocket mode and returns the ws path and host from
// plugin_opts (SIP003 format, e.g. "mode=websocket;path=/mm;host=a.com").
// Plugin TLS ("tls" option) is reported via tls so callers can refuse to
// serve it as plain ws.
func SSWebSocketPlugin(n *NodeSpec) (path, host string, tls, ok bool) {
	if n == nil || n.Protocol != "shadowsocks" {
		return "", "", false, false
	}
	switch strings.ToLower(strings.TrimSpace(n.Plugin)) {
	case "v2ray-plugin", "xray-plugin":
	default:
		return "", "", false, false
	}
	mode := "websocket" // v2ray-plugin default
	for _, opt := range strings.Split(n.PluginOpt, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(opt), "=")
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "mode":
			mode = strings.ToLower(strings.TrimSpace(v))
		case "path":
			path = strings.TrimSpace(v)
		case "host":
			host = strings.TrimSpace(v)
		case "tls":
			tls = true
		}
	}
	if mode != "websocket" {
		return "", "", false, false
	}
	if path == "" {
		path = "/"
	}
	return path, host, tls, true
}

// EffectiveNetwork returns the transport the kernel actually serves:
// shadowsocks + websocket plugin is treated as "ws".
func EffectiveNetwork(n *NodeSpec) string {
	if n == nil {
		return ""
	}
	if _, _, _, ok := SSWebSocketPlugin(n); ok {
		return "ws"
	}
	return n.Network
}
