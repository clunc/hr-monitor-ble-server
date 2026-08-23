// Package httpapi exposes the gateway's connect/disconnect control surface.
// Shape follows the sibling gateways: obp-real's /api/device/{connect,disconnect}
// and walkingpad-gateway's /status + /control.
package httpapi

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"

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
	ctrl Controller
	mux  *http.ServeMux
}

func New(ctrl Controller) *Server {
	s := &Server{ctrl: ctrl, mux: http.NewServeMux()}
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
