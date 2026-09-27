package health

import (
	"encoding/json"
	"net/http"
)

// Handler answers GET /healthz with the service name and version.
func Handler(service, version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{ // #nosec G104 -- best-effort response write
			"service": service,
			"status":  "ok",
			"version": version,
		})
	})
	return mux
}
