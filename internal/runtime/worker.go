package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/egress"
)

// engine handles one request: request JSON in, response JSON out.
type engine interface {
	handle(ctx context.Context, req []byte) ([]byte, error)
	close()
}

// outboundClient keeps transport I/O separate from the VM boundary.
type outboundClient interface {
	Fetch(context.Context, egress.Request) (egress.Response, error)
	Close()
}

type worker struct {
	spec      workerSpec
	versionFP string // fingerprint of the version files, for document images
	log       *childLog
	data      *dataStore
	eng       engine
	js        *jsEngine      // nil for WASI
	network   outboundClient // shared by all JS request and WebSocket VMs
	bodies    bodyBudget     // request body bytes held at once
}

// listenerFD is the inherited listening Unix socket (cmd.ExtraFiles[0]).
const listenerFD = 3

// specAcknowledged runs after the worker acknowledged its spec. A variable
// for tests.
var specAcknowledged = func(workerSpec) {}

// WorkerMain is the entry point of "flats worker": it reads its spec from
// stdin, serves HTTP on the inherited socket and exits when stdin closes
// (the parent stopped it or died). A pre-started worker first receives a
// prewarm line, prepares what does not depend on a flat, and then waits for
// its spec; stdin EOF before the spec ends it quietly.
func WorkerMain(args []string) error {
	_ = args
	log := stderrLog
	start := time.Now()
	stdin := bufio.NewReaderSize(os.Stdin, 64<<10)
	line, err := stdin.ReadBytes('\n')
	prewarmed := false
	if err == nil && bytes.HasPrefix(line, []byte(`{"prewarm":`)) {
		var msg struct {
			Prewarm *prewarmSpec `json:"prewarm"`
		}
		if jerr := json.Unmarshal(line, &msg); jerr != nil || msg.Prewarm == nil {
			return fmt.Errorf("worker: bad prewarm line: %v", jerr)
		}
		startPrewarm(*msg.Prewarm)
		prewarmed = true
		line, err = stdin.ReadBytes('\n')
		start = time.Now() // the worker's startup begins with its spec
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(bytes.TrimSpace(line)) == 0 {
			// Stopped (or the parent died) before it was given a flat. Let a
			// compilation cache write in progress finish.
			if prewarmed {
				waitPrewarm()
			}
			return nil
		}
		return fmt.Errorf("worker: read spec: %w", err)
	}
	var spec workerSpec
	if err := json.Unmarshal(line, &spec); err != nil {
		return fmt.Errorf("worker: bad spec: %w", err)
	}
	// From here on the flat's startup has begun: an adopted pre-started
	// worker that exits now is the flat's startup failure, not a reason to
	// start the flat again in a new process.
	// The CPU so far (a pre-started worker's warm-up) is not the flat's:
	// the parent subtracts it from every usage sample and from the exit.
	log.send(specAck())
	specAcknowledged(spec)
	f := os.NewFile(listenerFD, "listener")
	ln, err := net.FileListener(f)
	f.Close()
	if err != nil {
		log.send(childMsg{T: "fatal", Msg: "worker: no listener: " + err.Error()})
		return err
	}
	w := &worker{spec: spec, log: log, data: newDataStore(spec.DataDir)}
	if prewarmed {
		waitPrewarm() // a spec that arrives during prewarm uses its result
	}
	if err := w.init(); err != nil {
		log.send(childMsg{T: "fatal", Msg: err.Error()})
		return err
	}
	srv := &http.Server{Handler: w, ReadHeaderTimeout: 30 * time.Second, ErrorLog: nil}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.send(childMsg{T: "ready", MS: time.Since(start).Milliseconds()})
	if w.js != nil {
		w.js.afterReady()
	}
	go reportUsage(log, spec.usageEvery())

	// Stdin EOF means "stop" (or the parent is gone).
	stopped := make(chan struct{})
	go func() {
		io.Copy(io.Discard, stdin)
		close(stopped)
	}()
	select {
	case <-stopped:
	case err := <-served:
		log.send(childMsg{T: "log", Level: "error", Msg: "worker: serve: " + err.Error()})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	srv.Shutdown(ctx)
	cancel()
	if w.network != nil {
		w.network.Close()
	}
	w.eng.close()
	w.data.Close()
	return nil
}

// init prepares the worker's engine. A prewarmed shell that the engine did
// not take is released afterwards.
func (w *worker) init() error {
	defer releasePrewarmedShell()
	entry := path.Clean("/" + strings.TrimPrefix(w.spec.Entry, "/"))[1:]
	if entry == "" || entry == "." {
		return errors.New("server flat has no entry file")
	}
	w.spec.Entry = entry
	switch strings.ToLower(path.Ext(entry)) {
	case ".js", ".mjs":
		// The version files are mounted read-only at "/" for the module
		// loader and std.loadFile; wazero follows symlinks out of a mount.
		// A document runtime's image key covers the version files; the same
		// walk fingerprints them.
		dir, fp, err := checkVersionDir(w.spec.Dir, w.spec.ImagePath != "" && imagesEnabled())
		if err != nil {
			return err
		}
		w.spec.Dir, w.versionFP = dir, fp
		client, err := egress.New(w.spec.NetworkOrigins)
		if err != nil {
			return err
		}
		w.network = client
		e, err := newJSEngine(w)
		if err != nil {
			client.Close()
			return err
		}
		w.js, w.eng = e, e
	case ".wasm":
		e, err := newWASIEngine(w)
		if err != nil {
			return err
		}
		w.eng = e
	default:
		return fmt.Errorf("server entry %s: want a .js, .mjs or .wasm file", entry)
	}
	return nil
}

// checkVersionDir resolves the version directory and refuses symlinks and
// special files in it: wazero's directory mount follows symlinks, so a link
// in the version files would expose the host file it points to. Bundles never
// contain symlinks; this is defence in depth. With fingerprint it also
// returns the version files' fingerprint (fingerprintFile), or "" when a
// file could not be fingerprinted: the worker then starts without an image.
func checkVersionDir(dir string, fingerprint bool) (string, string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", fmt.Errorf("version files: %w", err)
	}
	var fp hash.Hash
	if fingerprint {
		fp = sha256.New()
	}
	n := 0
	err = filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n++; n > 200000 {
			return errors.New("version files: too many files")
		}
		t := d.Type()
		if t.IsRegular() && fp != nil && fingerprintFile(fp, real, p, d) != nil {
			fp = nil
		}
		if p == real || t.IsDir() || t.IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(real, p)
		return fmt.Errorf("version files: %s is a symlink or special file; a server flat may only contain regular files", rel)
	})
	if err != nil {
		return "", "", err
	}
	if fp == nil {
		return real, "", nil
	}
	return real, hex.EncodeToString(fp.Sum(nil)), nil
}

type reqPayload struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    *string           `json:"body"`
}

type respPayload struct {
	Status  int             `json:"status"`
	Headers map[string]any  `json:"headers"`
	Body    string          `json:"body"`
	B64     bool            `json:"b64"`
	Base64  bool            `json:"body_base64"` // WASI alias
	Error   *jsError        `json:"__error"`
	Raw     json.RawMessage `json:"-"`
}

func requestURL(r *http.Request) string {
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + r.URL.RequestURI()
}

func requestHeaders(r *http.Request) map[string]string {
	h := make(map[string]string, len(r.Header)+1)
	for k, v := range r.Header {
		sep := ", "
		if k == "Cookie" {
			sep = "; "
		}
		h[strings.ToLower(k)] = strings.Join(v, sep)
	}
	if r.Host != "" {
		h["host"] = r.Host
	}
	return h
}

// Request bodies are read into memory before the handler runs. A worker holds
// at most maxBufferedBodies bytes of them at once (bodies being received,
// waiting for a runtime or being handled, but not while the response is
// written); a body that would exceed it gets
// 503. Requests without a body are never refused. A body must arrive at
// minBodyRate after a bodyGrace start, so a stalled upload does not hold
// memory or a connection indefinitely; slower uploads get 408.
const (
	maxBufferedBodies = 8 * MaxRequestBody
	minBodyRate       = 16 << 10 // bytes per second
)

var bodyGrace = 30 * time.Second // a variable for tests

var errBodiesBusy = errors.New("request body budget exhausted")

// bodyDeadline is when the next part of a body that began at start and has
// n bytes so far must arrive.
func bodyDeadline(start time.Time, n int64) time.Time {
	return start.Add(bodyGrace + time.Duration(n)*time.Second/minBodyRate)
}

// bodyBudget counts request body bytes a worker holds. It never blocks: a
// request that does not fit fails at once instead of waiting with a partial
// body, which could leave every waiter holding memory that no one releases.
type bodyBudget struct {
	mu   sync.Mutex
	used int64
}

func (b *bodyBudget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > maxBufferedBodies-b.used {
		return false
	}
	b.used += n
	return true
}

func (b *bodyBudget) give(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

// readBody reads the request body within MaxRequestBody, the worker's body
// budget and the minimum upload rate. release returns the body's bytes to
// the budget; call it once the body is no longer needed, even after an error.
// Calling release again does nothing.
func (w *worker) readBody(rw http.ResponseWriter, r *http.Request) (body []byte, release func(), err error) {
	var held int64
	release = func() { w.bodies.give(held); held = 0 }
	if r.ContentLength > MaxRequestBody {
		return nil, release, &http.MaxBytesError{Limit: MaxRequestBody}
	}
	if r.Body == nil || r.Body == http.NoBody {
		return nil, release, nil
	}
	// Body memory is reserved before it is allocated: a declared length at
	// once, a chunked body as its buffer grows.
	if r.ContentLength > 0 {
		if !w.bodies.take(r.ContentLength) {
			return nil, release, errBodiesBusy
		}
		held = r.ContentLength
		body = make([]byte, 0, r.ContentLength)
	}
	rc := http.NewResponseController(rw)
	start := time.Now()
	src := http.MaxBytesReader(rw, r.Body, MaxRequestBody)
	defer func() {
		// A deadline left on the connection would end the request's context
		// while the handler runs. net/http clears it when the body ends; clear
		// it explicitly too. A failed read closes the connection anyway.
		if err == nil {
			if derr := rc.SetReadDeadline(time.Time{}); derr != nil && !errors.Is(derr, http.ErrNotSupported) {
				body, err = nil, derr
			}
		}
	}()
	// Reads go straight into the body's spare capacity. When it is full, a
	// one-byte probe finds the end of the body (or more of a chunked one)
	// without allocating.
	var probe [1]byte
	for {
		if err := rc.SetReadDeadline(bodyDeadline(start, int64(len(body)))); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return nil, release, err
		}
		full := len(body) == cap(body)
		dst := body[len(body):cap(body)]
		if full {
			dst = probe[:]
		}
		n, rerr := src.Read(dst)
		if n > 0 {
			if !full {
				body = body[:len(body)+n]
			} else {
				// MaxBytesReader never lets a body grow past MaxRequestBody.
				size := min(max(512, 2*int64(cap(body))), MaxRequestBody)
				if !w.bodies.take(size - held) {
					return nil, release, errBodiesBusy
				}
				held = size
				body = append(make([]byte, 0, size), body...)
				body = append(body, probe[0])
			}
		}
		if rerr == io.EOF {
			return body, release, nil
		}
		if rerr != nil {
			return nil, release, rerr
		}
	}
}

func (w *worker) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if w.js != nil && w.js.hasWS && isWebSocketUpgrade(r) {
		w.js.wsHub.serve(rw, r, requestURL(r), requestHeaders(r))
		return
	}
	body, release, err := w.readBody(rw, r)
	defer release()
	if err != nil {
		var mbe *http.MaxBytesError
		var ne net.Error
		switch {
		case errors.As(err, &mbe):
			http.Error(rw, fmt.Sprintf("request body larger than %d MiB", MaxRequestBody>>20), http.StatusRequestEntityTooLarge)
		case errors.Is(err, errBodiesBusy):
			// The rest of the body is not read, so the connection cannot be reused.
			rw.Header().Set("Connection", "close")
			rw.Header().Set("Retry-After", "1")
			http.Error(rw, "Service Unavailable: server flat is busy receiving other uploads.", http.StatusServiceUnavailable)
		case errors.As(err, &ne) && ne.Timeout():
			rw.Header().Set("Connection", "close")
			http.Error(rw, "request body arrived too slowly", http.StatusRequestTimeout)
		default:
			rw.Header().Set("Connection", "close")
			http.Error(rw, "could not read request body", http.StatusBadRequest)
		}
		return
	}
	p := reqPayload{Method: r.Method, URL: requestURL(r), Headers: requestHeaders(r)}
	if len(body) > 0 {
		s := string(body)
		p.Body = &s
	}
	reqJSON, _ := json.Marshal(p)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), w.spec.timeout())
	defer cancel()
	// Visitor disconnects cancel outbound I/O without destroying the shared VM
	// or interrupting the handler's DB/FILES work. The VM keeps its deadline.
	ctx = context.WithValue(ctx, outboundRequestContextKey{}, r.Context())
	start := time.Now()
	out, err := w.eng.handle(ctx, reqJSON)
	// The body is not needed once the handler returns. Writing the response
	// can block on a visitor who stops reading; that must not hold the
	// flat's body budget.
	body, p.Body, reqJSON = nil, nil, nil
	release()
	if err == nil {
		var resp respPayload
		if jerr := json.Unmarshal(out, &resp); jerr != nil {
			err = fmt.Errorf("handler output is not a JSON response {status, headers, body}: %v", jerr)
		} else if resp.Error != nil {
			err = resp.Error
		} else {
			w.writeResponse(rw, r, &resp)
			return
		}
	}
	w.fail(rw, r, err, time.Since(start))
}

func (w *worker) fail(rw http.ResponseWriter, r *http.Request, err error, took time.Duration) {
	status, visitor := http.StatusInternalServerError, "Internal Server Error: the server flat's handler failed."
	var fe *errVMFailed
	var je *jsError
	switch {
	case errors.As(err, &fe) && fe.timedOut:
		status, visitor = http.StatusGatewayTimeout, fmt.Sprintf("Gateway Timeout: the server flat's handler did not finish within %s.", w.spec.timeout())
	case strings.Contains(err.Error(), "out of memory"):
		visitor = "Internal Server Error: the server flat's handler ran out of memory."
	case errors.As(err, &je):
	}
	w.log.Log("error", fmt.Sprintf("%s %s -> %d after %dms: %v", r.Method, r.URL.RequestURI(), status, took.Milliseconds(), err))
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.WriteHeader(status)
	io.WriteString(rw, visitor+"\n")
}

var hopHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-connection": true, "transfer-encoding": true,
	"upgrade": true, "te": true, "trailer": true, "content-length": true,
}

func (w *worker) writeResponse(rw http.ResponseWriter, r *http.Request, resp *respPayload) {
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 200 || status > 599 {
		w.fail(rw, r, fmt.Errorf("handler returned invalid status %d", resp.Status), 0)
		return
	}
	body := []byte(resp.Body)
	if resp.B64 || resp.Base64 {
		b, err := base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			w.fail(rw, r, fmt.Errorf("handler returned invalid base64 body: %v", err), 0)
			return
		}
		body = b
	}
	if len(body) > MaxResponseBody {
		w.fail(rw, r, fmt.Errorf("response body larger than %d MiB", MaxResponseBody>>20), 0)
		return
	}
	h := rw.Header()
	for k, v := range resp.Headers {
		if hopHeaders[strings.ToLower(k)] || !validHeaderName(k) {
			continue
		}
		switch x := v.(type) {
		case string:
			addHeader(h, k, x)
		case []any:
			for _, e := range x {
				if s, ok := e.(string); ok {
					addHeader(h, k, s)
				}
			}
		case float64, bool:
			addHeader(h, k, fmt.Sprint(x))
		}
	}
	rw.WriteHeader(status)
	if r.Method != http.MethodHead {
		rw.Write(body)
	}
}

func validHeaderName(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte("\"(),/:;<=>?@[\\]{}", c) >= 0 {
			return false
		}
	}
	return true
}

func addHeader(h http.Header, k, v string) {
	if strings.ContainsAny(v, "\r\n\x00") {
		return
	}
	h.Add(k, v)
}
