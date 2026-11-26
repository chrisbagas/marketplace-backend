// Command healthcheck is the container health probe for the distroless API image
// (no shell, curl or wget there): it GETs /healthz on localhost and exits 0 only
// on HTTP 200. The compose file runs it as `["CMD", "/healthcheck"]`.
package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	client := http.Client{Timeout: 4 * time.Second}
	res, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		os.Exit(1)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
