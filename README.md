# Subnet Burst Block

A Traefik middleware plugin that groups clients into subnets (a `/16` by default) and blocks a whole subnet for a while once it sends a burst of requests.

This is a useful tool for blocking a swarm of bots or crawlers that don't otherwise have reliable patterns in their request fingerprints IE bots that use headless browsers, vary IPs, user-agents over time, etc...the common denominator for their crawling/scraping activity is *usually* that they use multiple clients to parallelize the work and these instances all originate from the same cloud provider and share, at same level, a common subnet.

Using this tool you can bucket IPs **by subnet** and then block the whole (configurable) subnet for a period of time to block their requests.

Attach this plugin/middleware only to the router you want to protect. Everything else, including your actual site, stays reachable.

## How it works

1. Each request's client IP is reduced to its subnet (`prefixLengthV4` / `prefixLengthV6`).
2. Requests whose path starts with one of `countPaths` are recorded in that subnet's sliding window of `windowSeconds`.
3. When the window holds `threshold` entries, the subnet is blocked for `blockSeconds`. With `distinctIPs: true` (the default), an entry is a distinct client IP, so one visitor reloading a page never triggers a block.
4. While blocked, **every** request from the subnet that passes through the middleware gets `statusCode` (default `403`).

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
    bot-burst:
      plugin:
        subnetburst:
          countPaths:
            - /foo
          threshold: 3
          windowSeconds: 120
          blockSeconds: 86400

  routers:
    myService:
      rule: Host(`subdomain.example.com`)
      middlewares: [bot-burst]
      service: myService
```

Docker labels:

```yaml
labels:
  - traefik.http.middlewares.bot-burst.plugin.subnetburst.countPaths=/foo
  - traefik.http.middlewares.bot-burst.plugin.subnetburst.threshold=3
  - traefik.http.routers.myService.middlewares=bot-burst
```

## Logs

The plugin writes to Traefik's stdout in a format close to Traefik's own:

```
2026-11-17T18:02:11Z INF middlewareName=analytics-burst@file msg="blocking subnet=195.64.0.0/16 for 24h0m0s: 3 distinct IPs in 2m0s (195.64.119.47,195.64.115.161,195.64.3.3)"
```

`INF` lines are always written: startup config, each subnet that gets blocked, and each block that expires.

## Debug Logs and Tuning Configuration

Set `debug: true` to also see `DBG` lines for every request: the client IP and subnet, whether the path was counted, the subnet's count against the threshold, blocked requests with time remaining, ignored or invalid IP headers, and cleanup of idle subnets.

<details>

<summary>Example</summary>

```
2026-11-18T02:02:15Z DBG middlewareName=analytics-burst@file msg="passing request: counted subnet=195.64.0.0/16 ip=195.64.115.77 path=/foo requests=1/4 window=2s"
2026-11-18T02:02:16Z DBG middlewareName=analytics-burst@file msg="passing request: path not counted subnet=194.26.0.0/16 ip=194.26.202.193 path=/bar"
...
2026-11-18T02:02:19Z INF middlewareName=analytics-burst@file msg="blocking subnet=195.64.0.0/16 for 10m0s: 4 requests in 2s (195.64.115.77,195.64.115.78,195.64.115.79,195.64.115.80)"
```
</details>

If you don't already have a clear subnet prefix pattern and time window for bursts then turn on `debug: true` and wait for a burst to occur, then analyze the logs above to determine how many IPs are making requests within X seconds. Fine tune your configuration based on this.

## Getting the right client IP

By default the plugin uses the connection address. That is correct when Traefik faces the internet directly.

If Traefik sits behind another proxy or CDN, every request appears to come from that proxy, and the plugin would group your whole audience into one subnet. Set `ipHeader` in that case:

- **Behind a proxy you configured in Traefik's `forwardedHeaders.trustedIPs`:** use `ipHeader: X-Real-Ip`. Traefik sets this header itself and drops client-supplied values from untrusted sources, so it is safe without `trustedProxies`.
- **Behind Cloudflare or another CDN:** use the CDN's header (for example `CF-Connecting-IP`) and list the CDN's ranges in `trustedProxies`. Otherwise anyone who reaches Traefik directly can send a fake header and get another network blocked.

For list headers like `X-Forwarded-For`, the plugin uses the last entry, which is the one added by the nearest proxy.

## Limits

- **State is in memory.** Counters and active blocks survive Traefik config reloads, but not a Traefik restart.
- **No sharing between replicas.** Each Traefik instance keeps its own counters. With several replicas behind a load balancer, a burst is split between them and takes longer to trigger.
- **A subnet is a blunt instrument.** A mobile carrier (CGNAT) or large corporate network can put many real users in one `/16`. On something like an analytics endpoint the cost is a few uncounted visits but be conservative in the subnet use and sliding window before attaching this to anything users seriously need.

## Development

```sh
go test -race ./...

# Load the plugin from source into a real Traefik (Yaegi) and replay a burst
cd testdata && docker compose up -d && ./e2e.sh; docker compose down
```
