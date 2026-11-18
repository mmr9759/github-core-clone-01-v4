// The forge entrypoint: bind an ephemeral port, register with the trial
// environment, serve until the environment says the suite is done, exit 0.
//
// Lifecycle (from the trial brief): listen on port 0, POST {"port": n} to
// $MAKEFUN_ENV_URL/register, then long-poll GET $MAKEFUN_ENV_URL/done until
// it answers {"done": true}. The environment dials the address the register
// request came from, so the listener binds all interfaces, never 8080.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")

	srv := newServer(token)
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	go srv.serve(ln)

	if envURL == "" {
		// No trial environment (a local run): serve on the chosen port and
		// wait forever; the harness drives the checks itself.
		fmt.Fprintf(os.Stderr, "forge listening on %d (no MAKEFUN_ENV_URL)\n", port)
		select {}
	}

	if err := register(envURL, port); err != nil {
		fatal(err)
	}
	pollDone(envURL)
}

// register POSTs {"port": n} until the environment accepts it. The first
// register wins; a retry loop covers a slow environment start.
func register(envURL string, port int) error {
	body := []byte(`{"port":` + strconv.Itoa(port) + `}`)
	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for i := 0; i < 120; i++ {
		resp, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 409 {
				return nil
			}
			lastErr = fmt.Errorf("register answered HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return lastErr
}

// pollDone GETs /done until {"done": true}, then returns so the process
// can exit 0. A short sleep between polls keeps the loop cheap.
func pollDone(envURL string) {
	client := &http.Client{Timeout: 10 * time.Second}
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if bytes.Contains(data, []byte(`"done":true`)) || bytes.Contains(data, []byte(`"done": true`)) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "forge:", err)
	os.Exit(1)
}
