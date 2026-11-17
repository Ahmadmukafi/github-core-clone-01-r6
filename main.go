// Entry point: binds the forge, registers with the trial environment when
// present, and polls /done until the suite is over.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	store := createStore()
	server := newForgeServer(store)
	port, err := server.listen("127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		// Standalone mode: serve on $PORT when set, otherwise the ephemeral port.
		if standalonePort, err := strconv.Atoi(os.Getenv("PORT")); err == nil && standalonePort > 0 {
			server.close()
			standalone := newForgeServer(store)
			if _, err := standalone.listen("127.0.0.1:" + strconv.Itoa(standalonePort)); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Printf("github-core-clone listening on http://127.0.0.1:%d\n", standalonePort)
		} else {
			fmt.Printf("github-core-clone listening on http://127.0.0.1:%d\n", port)
		}
		select {}
	}

	// Trial mode: register, then poll /done until the suite is over.
	base := strings.TrimRight(envURL, "/")
	registerURL := base + "/register"
	doneURL := base + "/done"
	client := &http.Client{Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]int{"port": port})
	resp, err := client.Post(registerURL, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "register failed: HTTP %d\n", resp.StatusCode)
		os.Exit(1)
	}
	for {
		time.Sleep(500 * time.Millisecond)
		resp, err := client.Get(doneURL)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var parsed struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(data, &parsed) == nil && parsed.Done {
				break
			}
		}
	}
	server.close()
	os.Exit(0)
}
