// Command mockupstream is a test-only fake OpenAI upstream used for
// end-to-end verification of anti-loop-proxy. It is NOT part of the
// shipped image.
//
// Endpoints:
//
//	POST /v1/chat/completions
//	  body {"stream": true,  "loop": true}  -> SSE with a repeating span
//	  body {"stream": true}                 -> SSE without duplication
//	  body {"stream": false}                -> plain JSON response
//
// Listens on :9000 by default (override with -addr).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const loopSpan = "The quick brown fox " // 20 runes, repeated 5x

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
			Loop   bool `json:"loop"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"cmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunk := func(content string) {
			payload := fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, content)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			if fl != nil {
				fl.Flush()
			}
		}
		chunk("") // role-only style empty delta
		if req.Loop {
			for i := 0; i < 5; i++ {
				chunk(loopSpan)
				time.Sleep(20 * time.Millisecond)
			}
		} else {
			chunk("Hello ")
			chunk("world, ")
			chunk("this is a normal stream.")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})

	// #nosec G114 -- test-only mock upstream; no timeout configuration needed.
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "mockupstream: %v\n", err)
		os.Exit(1)
	}
}
