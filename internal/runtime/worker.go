package runtime

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/egress"
)

// engine handles one request: request JSON in, response JSON out.
type engine interface {
	handle(ctx context.Context, req []byte) ([]byte, error)
	close()
}

type worker struct {
	spec    workerSpec
	log     *childLog
	data    *dataStore
	eng     engine
	js      *jsEngine      // nil for WASI
	network *egress.Client // shared by all JS request and WebSocket VMs
}

// listenerFD is the inherited listening Unix socket (cmd.ExtraFiles[0]).
const listenerFD = 3

// WorkerMain is the entry point of "flats worker": it reads its spec from
// stdin, serves HTTP on the inherited socket and exits when stdin closes
// (the parent stopped it or died).
func WorkerMain(args []string) error {
	_ = args
	log := stderrLog
	start := time.Now()
	stdin := bufio.NewReaderSize(os.Stdin, 64<<10)
	line, err := stdin.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("worker: read spec: %w", err)
	}
	var spec workerSpec
	if err := json.Unmarshal(line, &spec); err != nil {
		return fmt.Errorf("worker: bad spec: %w", err)
	}
	f := os.NewFile(listenerFD, "listener")
	ln, err := net.FileListener(f)
	f.Close()
	if err != nil {
		log.send(childMsg{T: "fatal", Msg: "worker: no listener: " + err.Error()})
		return err
	}
	w := &worker{spec: spec, log: log, data: newDataStore(spec.DataDir)}
	if err := w.init(); err != nil {
		log.send(childMsg{T: "fatal", Msg: err.Error()})
		return err
	}
	srv := &http.Server{Handler: w, ReadHeaderTimeout: 30 * time.Second, ErrorLog: nil}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.send(childMsg{T: "ready", MS: time.Since(start).Milliseconds()})

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

func (w *worker) init() error {
	entry := path.Clean("/" + strings.TrimPrefix(w.spec.Entry, "/"))[1:]
	if entry == "" || entry == "." {
		return errors.New("server flat has no entry file")
	}
	w.spec.Entry = entry
	switch strings.ToLower(path.Ext(entry)) {
	case ".js", ".mjs":
		// The version files are mounted read-only at "/" for the module
		// loader and std.loadFile; wazero follows symlinks out of a mount.
		dir, err := checkVersionDir(w.spec.Dir)
		if err != nil {
			return err
		}
		w.spec.Dir = dir
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
// contain symlinks; this is defence in depth.
func checkVersionDir(dir string) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("version files: %w", err)
	}
	n := 0
	err = filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n++; n > 200000 {
			return errors.New("version files: too many files")
		}
		if t := d.Type(); p == real || t.IsDir() || t.IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(real, p)
		return fmt.Errorf("version files: %s is a symlink or special file; a server flat may only contain regular files", rel)
	})
	if err != nil {
		return "", err
	}
	return real, nil
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

func (w *worker) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if w.js != nil && w.js.hasWS && isWebSocketUpgrade(r) {
		w.js.wsHub.serve(rw, r, requestURL(r), requestHeaders(r))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, MaxRequestBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(rw, fmt.Sprintf("request body larger than %d MiB", MaxRequestBody>>20), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(rw, "could not read request body", http.StatusBadRequest)
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
