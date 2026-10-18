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

// Version answers GET /versionz with the build in the body and the
// X-Build header.
func Version(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Build", version)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(version)) // #nosec G104 -- best-effort response write, client disconnect is the only failure mode
	})
}
