// Command faults sits in front of every Domain's federation inbound endpoint
// in the local stack and, when a story asks, makes the network misbehave the
// way a real one does: an answer that never comes back, or a refusal the API
// did not give.
//
// Each Domain's nginx sends POST /api/v1/federation/inbound here with the
// Domain in X-Faults-Domain, and by default it is passed straight through to
// that Domain's API, unchanged, Host header included. The stories arm rules on
// the control listener (published on 127.0.0.1:8026):
//
//	POST   /faults  {"domain":"example.com","from":"103mail.com","action":"lose","count":1}
//	POST   /faults  {"domain":"example.com","action":"answer","status":429,
//	                 "error":"rate_limited","retryAfter":60,"count":0}
//	GET    /faults  lists the armed rules
//	DELETE /faults  clears them all; ?domain=example.com clears one Domain's
//
// "lose" forwards the request, so the Domain does accept it, and then answers
// 502 as if the answer was lost on the way back. "answer" refuses without
// forwarding, with the status, {"error": ...} and, when given, Retry-After.
// "from" narrows a rule to one sending Domain (X-Federation-Sender). A count
// of 0 applies until cleared; otherwise the rule is used up after that many
// requests. Nothing here is part of the server: it is a control of the test
// stack (ADR 20).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address for federation traffic")
	control := flag.String("control", ":8026", "listen address for the controls")
	upstreams := flag.String("upstreams", os.Getenv("FAULTS_UPSTREAMS"),
		"comma-separated domain=host:port pairs naming each Domain's API")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	p, err := newProxy(*upstreams, log)
	if err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
	go func() {
		log.Info("fault controls listening", "addr", *control)
		if err := http.ListenAndServe(*control, p.controls()); err != nil {
			log.Error("fatal", "error", err)
			os.Exit(1)
		}
	}()
	log.Info("fault proxy listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, p); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// rule is one armed fault.
type rule struct {
	Domain     string `json:"domain"`
	From       string `json:"from,omitempty"`
	Action     string `json:"action"`
	Status     int    `json:"status,omitempty"`
	Error      string `json:"error,omitempty"`
	RetryAfter int    `json:"retryAfter,omitempty"`
	// Count is how many requests the rule still applies to; 0 is until
	// cleared.
	Count int `json:"count"`
}

type proxy struct {
	log       *slog.Logger
	upstreams map[string]*httputil.ReverseProxy

	mu    sync.Mutex
	rules []rule
}

func newProxy(upstreams string, log *slog.Logger) (*proxy, error) {
	p := &proxy{log: log, upstreams: map[string]*httputil.ReverseProxy{}}
	for _, pair := range strings.Split(upstreams, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		domain, host, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			return nil, fmt.Errorf("upstream %q is not domain=host:port", pair)
		}
		target, err := url.Parse("http://" + host)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: %w", pair, err)
		}
		rp := httputil.NewSingleHostReverseProxy(target)
		// The API reads the Destination from Host, so it is kept as nginx
		// sent it rather than rewritten to the upstream's.
		director := rp.Director
		rp.Director = func(r *http.Request) {
			host := r.Host
			director(r)
			r.Host = host
		}
		p.upstreams[strings.ToLower(domain)] = rp
	}
	if len(p.upstreams) == 0 {
		return nil, fmt.Errorf("no upstreams; set FAULTS_UPSTREAMS")
	}
	return p, nil
}

// take finds the first rule for a request and uses it up by one.
func (p *proxy) take(domain, from string) (rule, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, r := range p.rules {
		if r.Domain != domain || (r.From != "" && r.From != from) {
			continue
		}
		if r.Count > 0 {
			if p.rules[i].Count--; p.rules[i].Count == 0 {
				p.rules = append(p.rules[:i], p.rules[i+1:]...)
			}
		}
		return r, true
	}
	return rule{}, false
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(r.Header.Get("X-Faults-Domain"))
	upstream, ok := p.upstreams[domain]
	if !ok {
		http.Error(w, "no upstream for "+domain, http.StatusBadGateway)
		return
	}
	r.Header.Del("X-Faults-Domain")
	from := strings.ToLower(r.Header.Get("X-Federation-Sender"))
	fault, armed := p.take(domain, from)
	if !armed {
		upstream.ServeHTTP(w, r)
		return
	}
	p.log.Info("fault", "domain", domain, "from", from, "action", fault.Action)
	switch fault.Action {
	case "answer":
		body := map[string]any{"error": fault.Error}
		if fault.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(fault.RetryAfter))
			body["retry_after_seconds"] = fault.RetryAfter
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(fault.Status)
		_ = json.NewEncoder(w).Encode(body)
	case "lose":
		// The Domain gets the request and answers; the answer is dropped.
		upstream.ServeHTTP(discard{header: http.Header{}}, r)
		http.Error(w, "the answer was lost", http.StatusBadGateway)
	default:
		upstream.ServeHTTP(w, r)
	}
}

// discard is a ResponseWriter that throws the answer away.
type discard struct{ header http.Header }

func (d discard) Header() http.Header         { return d.header }
func (d discard) Write(b []byte) (int, error) { return len(b), nil }
func (d discard) WriteHeader(int)             {}

func (p *proxy) controls() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /faults", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(append([]rule{}, p.rules...))
	})
	mux.HandleFunc("POST /faults", func(w http.ResponseWriter, r *http.Request) {
		var f rule
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.Domain, f.From = strings.ToLower(f.Domain), strings.ToLower(f.From)
		if _, ok := p.upstreams[f.Domain]; !ok {
			http.Error(w, "unknown domain "+f.Domain, http.StatusBadRequest)
			return
		}
		if f.Action != "lose" && (f.Action != "answer" || f.Status < 400 || f.Error == "") {
			http.Error(w, `action is "lose", or "answer" with a 4xx or 5xx status and an error`, http.StatusBadRequest)
			return
		}
		if f.Count < 0 {
			http.Error(w, "count is 0 (until cleared) or more", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.rules = append(p.rules, f)
		p.mu.Unlock()
		p.log.Info("armed", "rule", f)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /faults", func(w http.ResponseWriter, r *http.Request) {
		domain := strings.ToLower(r.URL.Query().Get("domain"))
		p.mu.Lock()
		kept := p.rules[:0]
		for _, f := range p.rules {
			if domain != "" && f.Domain != domain {
				kept = append(kept, f)
			}
		}
		p.rules = kept
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
