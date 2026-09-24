package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/sebastianrueckerai/factory/internal/health"
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
	log.Fatal(http.ListenAndServe(*addr, health.Handler("factoryd", version)))
}
