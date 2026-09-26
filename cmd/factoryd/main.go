package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/health"
)

var version = "dev"

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		log.Println(version)
		return
	}

	dsn := os.Getenv("FACTORYD_DATABASE_URL")
	if dsn == "" {
		log.Fatal("FACTORYD_DATABASE_URL is required")
	}
	store, err := factoryd.OpenStore(context.Background(), dsn)
	if err != nil {
		log.Fatalf("open task store (boot migrations): %v", err)
	}
	defer store.Close()

	mux := http.NewServeMux()
	mux.Handle("/", health.Handler("factoryd", version))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			http.Error(w, "store unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("factoryd %s listening on %s", version, *addr)
	log.Fatal(server.ListenAndServe())
}
