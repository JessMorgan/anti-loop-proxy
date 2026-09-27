# anti-loop-proxy

anti-loop-proxy is an OpenAI-compatible reverse proxy that cuts off streaming
responses when the model starts repeating itself. For every request it
forwards to an upstream LLM API, it watches the streamed text for duplicated
content spans; when a span repeats enough times with configurable thresholds
met, it terminates the stream and signals the cut with a custom `anti_loop`
event. Everything else — non-streaming responses, non-SSE responses, requests
without `"stream": true` — passes through untouched, byte-for-byte. It is a
thin, stateless guard, not a caching layer, not a client library, and it does
not modify request payloads.

## How it works

The proxy sniffs the request body for the top-level `"stream"` boolean (bodies
over 10 MB are never read and are passed through unfiltered). When a request
has `stream: true` and the upstream answers with `Content-Type:
text/event-stream`, the response body is replaced by a stream filter. The
filter reads the SSE body line-by-line and forwards every line byte-for-byte
to the client while feeding each `choices[].delta.content` fragment into a
per-choice detector.

The detector reports a trigger when a span of length `[min_len, max_len]`
runes has at least `min_count` occurrences, with at most `max_gap`
non-duplicated runes between consecutive occurrences (overlapping occurrences
do not chain). On trigger the filter emits an `anti_loop` event followed by
`data: [DONE]` and closes the upstream connection; the client stream ends
there. Non-streaming requests, non-SSE responses, and any SSE line that is
not a `data:` line are passed through byte-for-byte with no interpretation.

## Quickstart (Docker)

Build the image:

```sh
docker build -t anti-loop-proxy:latest .
```

Run with the upstream set via environment variable:

```sh
docker run --rm -p 8080:8080 \
  -e ANTI_LOOP_UPSTREAM=https://api.openai.com \
  anti-loop-proxy:latest
```

Or with Docker Compose (create a `docker-compose.yaml` that sets
`ANTI_LOOP_UPSTREAM` for the `anti-loop-proxy:latest` image):

```sh
docker compose up
```

Example normal (non-stream) request — forwarded and passed through as-is:

```sh
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Say hi"}]}'
```

The upstream base URL path is preserved and joined with the request path:
with `ANTI_LOOP_UPSTREAM=https://api.openai.com`, a request to
`/v1/chat/completions` reaches `https://api.openai.com/v1/chat/completions`.
Point the upstream at the root (without the `/v1` suffix) and have clients
send `/v1/...` paths — the usual OpenAI layout.

## Configuration

Values are resolved from, in order of precedence, **environment variables >
`config.yaml` > defaults**: `env > config.yaml > defaults`.

The config file is looked up at `/etc/anti-loop-proxy/config.yaml`, then
`./config.yaml` (a missing file is not an error). Set
`ANTI_LOOP_CONFIG_FILE` to use a custom path (when set, only that path is
consulted). See `config.example.yaml` for an annotated full example.

| Parameter | Env var | YAML key | Default | Description |
|---|---|---|---|---|
| Listen address | `ANTI_LOOP_LISTEN` | `listen` | `:8080` | Address the proxy binds. `host:port`; a bare host gets port `8080` appended. |
| Upstream base URL | `ANTI_LOOP_UPSTREAM` | `upstream` | *required* | Absolute `http(s)://` URL of the upstream. The proxy refuses to start without it. |
| Upstream API key override | `ANTI_LOOP_UPSTREAM_API_KEY` | `upstream_api_key` | *(empty — passthrough client key)* | When set, replaces the `Authorization: Bearer` header with this key for upstream requests. |
| Min duplication count | `ANTI_LOOP_MIN_COUNT` | `min_count` | `4` | Occurrences of a span required to trigger. Must be >= 2. |
| Min span length (runes) | `ANTI_LOOP_MIN_LEN` | `min_len` | `12` | Shortest span considered. Must be >= 1. |
| Max span length (runes) | `ANTI_LOOP_MAX_LEN` | `max_len` | `200` | Longest span considered. Must be >= `min_len` and <= 4096. |
| Max non-dup runes between repeats | `ANTI_LOOP_MAX_GAP` | `max_gap` | `0` | Tolerance of non-duplicated runes between consecutive occurrences. 0 = adjacent. Must be >= 0. |
| Config file path | `ANTI_LOOP_CONFIG_FILE` | — | `/etc/anti-loop-proxy/config.yaml`, then `./config.yaml` | Where the YAML config is read from. |
| Log level | `ANTI_LOOP_LOG_LEVEL` | `log_level` | `info` | One of `debug`, `info`, `warn`, `error`. |

Invalid values (wrong types, out-of-range numbers, malformed listen address,
bad log level, missing upstream) are a startup error: the process logs to
stderr and exits with status 1.

## The `anti_loop` event

When a stream is cut, the proxy appends this block to the SSE stream:

```
event: anti_loop
data: {"reason":"duplicate_content","count":N,"span_len":L,"span":"..."}

data: [DONE]

```

Field meanings:

- `reason` — always `duplicate_content`.
- `count` — how many occurrences of the span were counted (the triggering chain, including the latest occurrence).
- `span_len` — length of the repeated span, in runes.
- `span` — the repeated span text, truncated to at most 80 runes in the payload.

The HTTP response status remains `200 OK`; only the body is terminated
early. Clients that strictly parse OpenAI SSE should ignore the unknown
`event:` type; the trailing `data:` marks the stream's end.

## Operational notes

- **`GET /healthz`** returns `200` with body `ok` and is not proxied.
- **Logs** are structured JSON on stdout. Every request logs one line with
  `method`, `path`, `status`, `stream_cut` (boolean), and `duration_ms`.
  Stream cuts are additionally logged at `warn` level with `count`,
  `span_len`, and `span`.
- **Graceful shutdown** on `SIGTERM`/`SIGINT`: in-flight requests are allowed
  to finish for up to 10 seconds, then the server exits.
- **Memory**: per-stream detection buffers are bounded by the configured
  parameters (the scan only ever inspects a window of
  `max_len*(min_count+1) + max_gap*min_count + max_len` runes of trailing
  history, plus one streamed line in flight), so memory is bounded per
  stream, not by response length.

## Caveats

- Spans longer than `max_len` are not detectable — detection uses a bounded
  window, so an extremely long repetition will not trigger a cut.
- All length parameters (`min_len`, `max_len`, `span_len`, and the
  gap) are counted in **runes** (Unicode code points), so multibyte text is
  handled safely; a Latin letter and a CJK character both count as 1.
- Detection applies **only to streaming SSE responses** (`"stream": true`
  with a `text/event-stream` response). Non-streaming responses, even if
  full of repetition, are passed through unmodified.
- Requests whose body is known to exceed 10 MB are not sniffed and are never
  stream-filtered, even if they set `"stream": true`.
- The detector is per-choice: choices are tracked independently by their
  `index` field.

## Development

```sh
make build          # static binary at bin/anti-loop-proxy
make test           # go test ./...
make lint           # go vet ./...
make docker-build   # docker build -t anti-loop-proxy:latest .
```
