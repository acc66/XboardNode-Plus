# Custom Outbounds

## Quick Example

```json
{
  "custom_outbounds": [
    {
      "tag": "warp",
      "protocol": "wireguard",
      "settings": {
        "peers": [{"address": "162.159.195.1"}],
        "private_key": "aMzA..."
      }
    },
    {
      "tag": "us-proxy",
      "protocol": "socks",
      "settings": {"servers": [{"address": "1.2.3.4", "port": 1080}]},
      "proxy_tag": "warp"
    }
  ]
}
```

## Supported Protocols

| Protocol | Xray | Sing-box |
|----------|------|----------|
| vmess | ✅ | ✅ |
| vless | ✅ | ✅ |
| trojan | ✅ | ✅ |
| shadowsocks | ✅ | ✅ |
| socks | ✅ | ✅ |
| http | ✅ | ✅ |
| wireguard | ✅ | ✅ |
| tuic | ❌ | ✅ |
| hysteria2 | ❌ | ✅ |
| anytls | ❌ | ✅ |
| naive | ❌ | ✅ |
| mieru | ❌ | ✅ |

## Core Fields

| Field | Required | Description |
|-------|----------|-------------|
| `tag` | ✅ | Outbound tag, referenced in routes |
| `protocol` | ✅ | Protocol type |
| `settings` | ✅ | Protocol-specific configuration |
| `proxy_tag` | ❌ | Chain to another outbound (references its tag) |

## Best Practices

- **Prefer** `custom_outbounds`: panel-managed, cross-kernel compatible
- **Use** `kernel.custom_outbound` only: when native fields are needed

## Live updates (sing-box)

Panel changes are applied on the next successful configuration sync. New and
changed outbounds are installed before routing rules reference them; removed
outbounds are retired after the rules update. Proxy-chain dependencies are
loaded first and refreshed when their parent changes.

Outbound-only and route-only changes preserve the inbound listener, traffic
tracker, and unchanged outbounds. No service restart is required. Replacing or
removing an outbound may close sessions belonging to that outbound, depending
on the protocol. Invalid configurations still follow the existing reload error
and recovery path.

Install a build containing this fix once; older running binaries still need a
restart to apply newly added outbounds.
