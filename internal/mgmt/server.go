package mgmt

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed ui.html
var uiFS embed.FS

// Server is the HTTP surface. It only ever edits the K8sWorkerPool object, so
// the page can do nothing an operator could not do with kubectl - and the
// controller stays the only thing that talks to vCD.
type Server struct {
	Client    client.Client
	Collector *Collector
	// Auth is the account list read from the Secret. Nil means no
	// authentication at all, which only a local run should do.
	Auth *Authenticator
	// PathPrefix is where the ingress mounts the page, /nodescale-ui by
	// default. Everything the server answers lives under it, so the ingress
	// needs no rewrite.
	PathPrefix string
}

// prefix is the path prefix in the one form the mux accepts.
func (s *Server) prefix() string {
	p := strings.TrimSuffix(strings.TrimSpace(s.PathPrefix), "/")
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// Handler is the whole surface, prefix included.
func (s *Server) Handler() http.Handler {
	p := s.prefix()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+p+"/{$}", s.index)
	mux.HandleFunc("GET "+p+"/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET "+p+"/api/me", s.me)
	mux.HandleFunc("GET "+p+"/api/pools", s.listPools)
	mux.HandleFunc("GET "+p+"/api/pools/{ns}/{name}", s.getPool)
	mux.HandleFunc("POST "+p+"/api/pools/{ns}/{name}/replicas", s.postReplicas)
	mux.HandleFunc("POST "+p+"/api/pools/{ns}/{name}/flavor", s.postFlavor)
	mux.HandleFunc("POST "+p+"/api/pools/{ns}/{name}/delete-workers", s.postDeleteWorkers)
	if p != "" {
		mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, p+"/", http.StatusFound)
		})
	}
	return s.withAuth(mux)
}

// me tells the page who it is talking to: a viewer gets a read-only page, and
// the header shows the name. When authentication is off the role is admin and
// the page says so, because everything really is allowed.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	auth := s.Auth != nil && !s.Auth.Disabled
	role := string(actor.Role)
	if !auth {
		role = string(RoleAdmin)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": actor.Username,
		"role":     role,
		"auth":     auth,
	})
}

// index serves the page with its own prefix baked in.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	page, err := uiFS.ReadFile("ui.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := strings.Replace(string(page), "__PREFIX__", s.prefix(), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, out)
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	pools, at, err := s.Collector.Snapshot()
	if err != nil && len(pools) == 0 {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pools":       pools,
		"collectedAt": at.UTC().Format(time.RFC3339),
		"stale":       err != nil,
	})
}

func (s *Server) getPool(w http.ResponseWriter, r *http.Request) {
	pools, _, _ := s.Collector.Snapshot()
	ns, name := r.PathValue("ns"), r.PathValue("name")
	for i := range pools {
		if pools[i].Namespace == ns && pools[i].Name == name {
			writeJSON(w, http.StatusOK, pools[i])
			return
		}
	}
	writeError(w, http.StatusNotFound, errors.New("no such pool"))
}

func (s *Server) postReplicas(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Replicas int32 `json:"replicas"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.SetReplicas(r.Context(), r.PathValue("ns"), r.PathValue("name"), body.Replicas); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replicas": body.Replicas})
}

func (s *Server) postFlavor(w http.ResponseWriter, r *http.Request) {
	var body Flavor
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.SetFlavor(r.Context(), r.PathValue("ns"), r.PathValue("name"), body); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) postDeleteWorkers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workers []string `json:"workers"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.DeleteWorkers(r.Context(), r.PathValue("ns"), r.PathValue("name"), body.Workers); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// statusFor maps an API error onto the status the page shows.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case apierrors.IsNotFound(err):
		return http.StatusNotFound
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsForbidden(err):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func decode(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
