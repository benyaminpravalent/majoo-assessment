package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// probeTimeout bounds the self-probe. Docker's HEALTHCHECK has its own timeout;
// this one makes sure the process exits with a useful message rather than being
// killed halfway through.
const probeTimeout = 3 * time.Second

// probeSelf performs a GET against this process's own liveness endpoint.
//
// addr is the listen address as configured (":8080", "0.0.0.0:8080"), which is
// not necessarily dialable as written, so the host part is replaced with the
// loopback address.
func probeSelf(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	url := "http://" + loopbackAddr(addr) + "/healthz"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build health-check request: %w", err)
	}

	resp, err := (&http.Client{Timeout: probeTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: %s returned %s", url, resp.Status)
	}
	return nil
}

// loopbackAddr turns a listen address into one that can be dialled from inside
// the same container.
func loopbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// A bare port such as "8080".
		return net.JoinHostPort("127.0.0.1", strings.TrimPrefix(addr, ":"))
	}
	// ":8080" and "0.0.0.0:8080" both mean "every interface"; dial loopback.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
