# httpproxy invariants

- The proxy listens only on loopback and reaches only `Config.Allow`
  addresses. Check credentials before the allowlist, and refuse before any
  dial, so an unauthenticated caller learns nothing about allowed upstreams.
- Every request carries a Client's Basic proxy credentials. Compare passwords
  in constant time. Never forward `Proxy-Authorization` upstream.
- TLS goes through `CONNECT` tunnels; the proxy never terminates TLS or sees
  upstream credentials. Plain HTTP uses absolute-form requests.
- A Client starts requests, tunnels, and upstream dials only while bound.
  Track every inbound and upstream connection under the binding that
  admitted it, and refuse to track once that binding's context has ended. A
  context ends before its cutoff runs, so every tracked connection is closed
  by the cutoff.
- An ended binding stays the Client's binding until `stop`, and `stop` waits
  for a cutoff that has started. No newer binding can exist while a cutoff
  runs, so the cutoff closes every connection the Client holds. Do not let
  `Bind` replace an ended binding before `stop`.
- When a binding's context ends, close all of the Client's connections. This
  is the package's purpose: it stops transfers in code the caller cannot
  cancel. `stop` refuses new requests, tunnels, and dials but keeps idle
  connections; traffic inside an established tunnel is invisible to the
  proxy and continues until a cutoff or Close.
- Each Client's plain-HTTP pool dials through the Client's binding. Go's
  transport keeps dialing after a request is cancelled, so the binding and
  the Client's lifetime, not the request, must cancel the dial. Every
  upstream dial ends when its binding ends or stops, or the Client or Proxy
  closes. `stop` cancels dials but leaves established connections open. Pool connections are tracked like any
  other and untrack themselves on Close; never call their Close with the
  proxy lock held.
- A Client's plain-HTTP pool keeps every idle connection. Its own
  concurrency bounds the pool and a cutoff or Close ends it; Go's default of
  two idle connections per host redials for each concurrent burst.
- Preserve the inbound raw query. `ReverseProxy` would drop parameters it
  cannot parse.
- Forwarded responses flush every upstream read immediately. Buffering would
  hold a slow response's headers until the cutoff.
- `Proxy.Close` waits for every tunnel goroutine; start tunnels under the
  proxy lock after checking `closed`.
