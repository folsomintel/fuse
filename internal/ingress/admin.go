package ingress

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
)

// the admin api. only the orchestrator calls it, with one shared bearer token.
//
//	PUT    /v1/owners/{owner}   publish (create, update, rotate, adopt)
//	GET    /v1/owners/{owner}   look up
//	DELETE /v1/owners/{owner}   remove
//	GET    /healthz

// publishBody is the body of PUT /v1/owners/{owner}.
type publishBody struct {
	Token     string     `json:"token"`
	Ports     []PortSpec `json:"ports"`
	AdoptFrom string     `json:"adopt_from,omitempty"`
}

// routeView is a route as the orchestrator sees it, with the address clients use.
type routeView struct {
	Name       string `json:"name"`
	GuestPort  int    `json:"guest_port"`
	PublicPort int    `json:"public_port"`
	URL        string `json:"url"`
}

// ownerView is the answer to a publish or a look up.
type ownerView struct {
	// ProxyAddr and ServerCertPEM go into the guest's tunnel.json.
	ProxyAddr     string      `json:"proxy_addr"`
	ServerCertPEM string      `json:"server_cert_pem"`
	Routes        []routeView `json:"routes"`
}

// AdminConfig is what the admin api needs beyond the proxy itself.
type AdminConfig struct {
	Token string
	// PublicHost is the name or address clients and guests reach this machine
	// by. TunnelPort is the udp port guests dial.
	PublicHost    string
	TunnelPort    int
	ServerCertPEM string
}

// AdminHandler serves the admin api.
func AdminHandler(p *Proxy, cfg AdminConfig) http.Handler {
	view := func(routes []Route) ownerView {
		out := ownerView{
			ProxyAddr:     net.JoinHostPort(cfg.PublicHost, strconv.Itoa(cfg.TunnelPort)),
			ServerCertPEM: cfg.ServerCertPEM,
			Routes:        make([]routeView, 0, len(routes)),
		}
		for _, r := range routes {
			out.Routes = append(out.Routes, routeView{
				Name: r.Name, GuestPort: r.GuestPort, PublicPort: r.PublicPort,
				URL: net.JoinHostPort(cfg.PublicHost, strconv.Itoa(r.PublicPort)),
			})
		}
		return out
	}
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("PUT /v1/owners/{owner}", func(w http.ResponseWriter, r *http.Request) {
		var body publishBody
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		routes, err := p.Publish(r.PathValue("owner"), body.Token, body.Ports, body.AdoptFrom)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, http.StatusOK, view(routes))
	})
	mux.HandleFunc("GET /v1/owners/{owner}", func(w http.ResponseWriter, r *http.Request) {
		routes, ok := p.Routes(r.PathValue("owner"))
		if !ok {
			http.Error(w, "owner not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, view(routes))
	})
	mux.HandleFunc("DELETE /v1/owners/{owner}", func(w http.ResponseWriter, r *http.Request) {
		found, err := p.Remove(r.PathValue("owner"))
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !found:
			http.Error(w, "owner not found", http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			got := []byte(r.Header.Get("Authorization"))
			if subtle.ConstantTimeCompare(got, []byte("Bearer "+cfg.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
