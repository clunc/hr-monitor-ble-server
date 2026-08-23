// Package httpapi exposes the gateway's connect/disconnect control surface.
// Shape follows the sibling gateways: obp-real's /api/device/{connect,disconnect}
// and walkingpad-gateway's /status + /control.
package httpapi

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/clunc/hr-monitor-ble-server/pkg/heartrate"
	"github.com/sirupsen/logrus"
)

// The control page ships inside the binary: no second image, no nginx, no
// static-file volume to keep in sync with the API it drives.
//
//go:embed index.html
var indexHTML []byte

// Controller is the slice of the monitor the API drives.
type Controller interface {
	Connect(name, mac string) error
	Disconnect()
	Status() heartrate.Status
	Devices() []heartrate.Device
}

type Server struct {
	ctrl  Controller
	mux   *http.ServeMux
	peers []string
}

// peer is another gateway's view of its own strap. One instance owns one strap,
// so a two-strap setup is two processes — without this the page can only ever
// show half of what is running.
type peer struct {
	URL    string `json:"url"`
	Source string `json:"source,omitempty"`
	Link   string `json:"link"`
	Device string `json:"device,omitempty"`
	LastHR uint32 `json:"last_hr,omitempty"`
	Err    string `json:"error,omitempty"`
}

// peersFor reads HR_PEERS: comma-separated base URLs of sibling gateways.
func peersFor() []string {
	var out []string
	for _, p := range strings.Split(os.Getenv("HR_PEERS"), ",") {
		if p = strings.TrimSpace(strings.TrimSuffix(p, "/")); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) knownPeer(url string) bool {
	for _, p := range s.peers {
		if p == url {
			return true
		}
	}
	return false
}

// fetchPeers queries siblings server-side. Doing it here rather than from the
// page avoids cross-origin requests between gateways on different ports.
func (s *Server) fetchPeers() []peer {
	out := make([]peer, len(s.peers))
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	for i, url := range s.peers {
		wg.Add(1)
		go func(i int, url string) {
			defer wg.Done()
			out[i] = peer{URL: url, Link: "unreachable"}
			resp, err := client.Get(url + "/status")
			if err != nil {
				out[i].Err = err.Error()
				return
			}
			defer resp.Body.Close()
			var st struct {
				Link   string `json:"link"`
				Source string `json:"source"`
				Device string `json:"device"`
				Target string `json:"target"`
				LastHR uint32 `json:"last_hr"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
				out[i].Err = err.Error()
				return
			}
			label := st.Source
			if label == "" {
				label = st.Device
			}
			if label == "" {
				label = st.Target
			}
			out[i] = peer{URL: url, Source: label, Link: st.Link, Device: st.Device, LastHR: st.LastHR}
		}(i, url)
	}
	wg.Wait()
	return out
}

func New(ctrl Controller) *Server {
	s := &Server{ctrl: ctrl, mux: http.NewServeMux(), peers: peersFor()}
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.ctrl.Status())
	})
	s.mux.HandleFunc("GET /peers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.fetchPeers())
	})
	// Control a sibling from this page. Proxied here rather than called from the
	// browser: the peer is a different origin, so a direct fetch would be blocked.
	// Only URLs from HR_PEERS are accepted — this must not become a way to make
	// the gateway POST to arbitrary hosts.
	s.mux.HandleFunc("POST /peer/{action}", func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		if action != "connect" && action != "disconnect" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad action"})
			return
		}
		url := strings.TrimSuffix(r.URL.Query().Get("url"), "/")
		if !s.knownPeer(url) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "unknown peer"})
			return
		}
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Post(url+"/"+action, "application/json", nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		writeJSON(w, resp.StatusCode, map[string]string{"peer": url, "action": action})
	})
	s.mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.ctrl.Devices())
	})
	// Optional name/mac select which strap to hold; omitted means the configured
	// target. 409 when a link or attempt is already in flight (obp precedent).
	s.mux.HandleFunc("POST /connect", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if err := s.ctrl.Connect(q.Get("name"), q.Get("mac")); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, heartrate.ErrBusy) {
				code = http.StatusConflict
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		logrus.Info("control: connect requested")
		writeJSON(w, http.StatusAccepted, s.ctrl.Status())
	})
	s.mux.HandleFunc("POST /disconnect", func(w http.ResponseWriter, r *http.Request) {
		s.ctrl.Disconnect()
		logrus.Info("control: disconnect requested")
		writeJSON(w, http.StatusOK, s.ctrl.Status())
	})
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
