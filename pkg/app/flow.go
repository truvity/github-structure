package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"time"
)

const (
	// ListenAddr is where the helper serves the one-click page. Loopback
	// only: the page carries the manifest and receives the credential
	// code, and neither should ever be reachable off the machine.
	ListenAddr = "127.0.0.1:9797"
)

type (
	// Flow runs the browser half of an App creation: it serves a page
	// whose only job is to POST the manifest to GitHub, then catches the
	// redirect carrying the one-time code.
	Flow struct {
		Org      string
		AppName  string
		Manifest *Manifest

		state  string
		result chan flowResult
		server *http.Server
	}

	flowResult struct {
		code string
		err  error
	}
)

// NewFlow prepares a creation flow. The state parameter is a CSRF nonce:
// GitHub echoes it back, and a callback that does not match it is not our
// redirect and must not be exchanged.
func NewFlow(org, appName string, manifest *Manifest) (*Flow, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generating state: %w", err)
	}

	return &Flow{
		Org:      org,
		AppName:  appName,
		Manifest: manifest,
		state:    base64.RawURLEncoding.EncodeToString(buf),
		result:   make(chan flowResult, 1),
	}, nil
}

// StartURL is the single link the operator clicks.
func (f *Flow) StartURL() string {
	return "http://" + ListenAddr + "/"
}

// Run serves the flow until GitHub redirects back with a code, the
// context is canceled, or the timeout expires.
func (f *Flow) Run(ctx context.Context, timeout time.Duration) (string, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handleStart)
	mux.HandleFunc("/callback", f.handleCallback)

	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", ListenAddr)
	if err != nil {
		return "", fmt.Errorf("listening on %s (another creation flow still running?): %w", ListenAddr, err)
	}

	f.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = f.server.Serve(listener) }()

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = f.server.Shutdown(shutdownCtx)
	}()

	select {
	case res := <-f.result:
		// Give the browser a moment to render the "done" page before the
		// server goes away under it.
		time.Sleep(300 * time.Millisecond)

		return res.code, res.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for the GitHub redirect")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// handleStart renders the auto-submitting form. GitHub's manifest flow
// requires a POST with the manifest as a form field, which a plain link
// cannot do — hence a local page instead of just printing a URL.
func (f *Flow) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)

		return
	}

	body, err := json.Marshal(f.Manifest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := startPage.Execute(w, map[string]any{
		"Action":   CreateURL(f.Org, f.state),
		"Manifest": string(body),
		"AppName":  f.AppName,
		"Org":      f.Org,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (f *Flow) handleCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	code := query.Get("code")
	state := query.Get("state")

	if subtle.ConstantTimeCompare([]byte(state), []byte(f.state)) != 1 {
		http.Error(w, "state mismatch — this callback did not come from the flow this helper started", http.StatusBadRequest)

		f.finish(flowResult{err: fmt.Errorf("state mismatch on callback")})

		return
	}

	if code == "" {
		http.Error(w, "no code in callback", http.StatusBadRequest)

		f.finish(flowResult{err: fmt.Errorf("GitHub redirected without a code")})

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = donePage.Execute(w, map[string]any{"AppName": f.AppName})

	f.finish(flowResult{code: code})
}

func (f *Flow) finish(res flowResult) {
	select {
	case f.result <- res:
	default:
	}
}

var startPage = template.Must(template.New("start").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Create {{.AppName}}</title>
<style>
 body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem}
 code{background:#f3f3f3;padding:.1rem .3rem;border-radius:3px}
 button{font:inherit;padding:.6rem 1.2rem;cursor:pointer}
</style></head>
<body>
<h1>Create <code>{{.AppName}}</code></h1>
<p>This posts the manifest generated from <code>cfg/github.yaml</code> to the
<strong>{{.Org}}</strong> organization. GitHub will show you the App's
permissions before anything is created — review them, then confirm there.</p>
<form id="f" action="{{.Action}}" method="post">
  <input type="hidden" name="manifest" value='{{.Manifest}}'>
  <button type="submit">Continue to GitHub</button>
</form>
<script>document.getElementById('f').submit()</script>
</body></html>
`))

var donePage = template.Must(template.New("done").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>{{.AppName}} created</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem}</style>
</head>
<body>
<h1>{{.AppName}} created</h1>
<p>Credentials are being written to 1Password. Return to the terminal —
it will print the install link next.</p>
</body></html>
`))
