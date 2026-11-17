# Subnet Burst Block

A Traefik middleware plugin that groups clients into subnets (a `/16` by default) and blocks a whole subnet for a while once it sends a burst of requests.

It was built to keep data-center bot swarms out of self-hosted analytics. These bots run real headless browsers, rotate through dozens of IPs in a handful of `/16` networks, use generic user agents, and arrive in short daily bursts. Per-IP rate limits never trip and user-agent lists don't match, but "3 different IPs from the same `/16` loaded `script.js` within 2 minutes" catches them reliably.

Attach it only to the router you want to protect (for example your analytics host). Everything else, including your actual site, stays reachable.

## How it works

1. Each request's client IP is reduced to its subnet (`prefixLengthV4` / `prefixLengthV6`).
2. Requests whose path starts with one of `countPaths` are recorded in that subnet's sliding window of `windowSeconds`.
3. When the window holds `threshold` entries, the subnet is blocked for `blockSeconds`. With `distinctIPs: true` (the default), an entry is a distinct client IP, so one visitor reloading a page never triggers a block.
4. While blocked, **every** request from the subnet that passes through the middleware gets `statusCode` (default `403`), not only the counted paths.

The request that reaches the threshold is blocked too. With `threshold: 3`, the first two requests in a burst get through.

## Configuration

| Option | Default | Description |
| --- | --- | --- |
| `prefixLengthV4` | `16` | Subnet size IPv4 clients are grouped by (1-32). |
| `prefixLengthV6` | `48` | Subnet size IPv6 clients are grouped by (1-128). |
| `countPaths` | all paths | Path prefixes that count toward a burst. Blocking still applies to every path. |
| `threshold` | `3` | Counted requests (or distinct IPs) within the window that trigger a block. |
| `windowSeconds` | `120` | Length of the sliding window. |
| `distinctIPs` | `true` | Count distinct client IPs in the subnet instead of raw requests. |
| `blockSeconds` | `3600` | How long a triggered subnet stays blocked. |
| `statusCode` | `403` | Status returned to blocked requests. `404` works too if you'd rather not advertise the block. |
| `allowedCIDRs` | none | IPs or CIDRs that are never counted or blocked (uptime monitors, your own network). |
| `ipHeader` | none | Read the client IP from this header instead of the connection address. See below. |
| `trustedProxies` | none | When set, `ipHeader` is only honored for connections from these CIDRs. |
| `debug` | `false` | Log every decision this middleware makes at `DBG` level. Plugins don't inherit Traefik's global log level, so this is set per middleware. Expect one or more lines per request. |

## Install

Static configuration:

```yaml
experimental:
  plugins:
    subnetburst:
      moduleName: github.com/FoxxMD/traefik-subnet-burst
      version: v0.1.0
```

Dynamic configuration, protecting an analytics backend:

```yaml
http:
  middlewares:
    analytics-burst:
      plugin:
        subnetburst:
          countPaths:
            - /api/script.js
          threshold: 3
          windowSeconds: 120
          blockSeconds: 86400

  routers:
    analytics:
      rule: Host(`analytics.example.com`)
      middlewares: [analytics-burst]
      service: analytics
```

Docker labels:

```yaml
labels:
  - traefik.http.middlewares.analytics-burst.plugin.subnetburst.countPaths=/api/script.js
  - traefik.http.middlewares.analytics-burst.plugin.subnetburst.threshold=3
  - traefik.http.routers.analytics.middlewares=analytics-burst
```

## Logs

The plugin writes to Traefik's stdout in a format close to Traefik's own:

```
2026-11-17T18:02:11Z INF middlewareName=analytics-burst@file msg="blocking subnet=195.64.0.0/16 for 24h0m0s: 3 distinct IPs in 2m0s (195.64.119.47,195.64.115.161,195.64.3.3)"
```

`INF` lines are always written: startup config, each subnet that gets blocked, and each block that expires. Set `debug: true` to also see `DBG` lines for every request: the client IP and subnet, whether the path was counted, the subnet's count against the threshold, blocked requests with time remaining, ignored or invalid IP headers, and cleanup of idle subnets.

## Getting the right client IP

By default the plugin uses the connection address. That is correct when Traefik faces the internet directly.

If Traefik sits behind another proxy or CDN, every request appears to come from that proxy, and the plugin would group your whole audience into one subnet. Set `ipHeader` in that case:

- **Behind a proxy you configured in Traefik's `forwardedHeaders.trustedIPs`:** use `ipHeader: X-Real-Ip`. Traefik sets this header itself and drops client-supplied values from untrusted sources, so it is safe without `trustedProxies`.
- **Behind Cloudflare or another CDN:** use the CDN's header (for example `CF-Connecting-IP`) and list the CDN's ranges in `trustedProxies`. Otherwise anyone who reaches Traefik directly can send a fake header and get another network blocked.

For list headers like `X-Forwarded-For`, the plugin uses the last entry, which is the one added by the nearest proxy.

## Limits

- **State is in memory.** Counters and active blocks survive Traefik config reloads, but not a Traefik restart.
- **No sharing between replicas.** Each Traefik instance keeps its own counters. With several replicas behind a load balancer, a burst is split between them and takes longer to trigger.
- **A subnet is a blunt instrument.** A mobile carrier (CGNAT) or large corporate network can put many real users in one `/16`. On an analytics endpoint the cost is a few uncounted visits. Think harder before attaching this to anything users need.

## Development

```sh
go test -race ./...

# Load the plugin from source into a real Traefik (Yaegi) and replay a burst
cd testdata && docker compose up -d && ./e2e.sh; docker compose down
```
