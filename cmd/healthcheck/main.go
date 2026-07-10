package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

const readinessURL = "http://127.0.0.1:8080/api/v1/health/ready"

func main() {
	if err := check(readinessURL, &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}); err != nil {
		fmt.Fprintln(os.Stderr, "not ready")
		os.Exit(1)
	}
}

type httpGetter interface {
	Get(string) (*http.Response, error)
}

func check(url string, client httpGetter) error {
	response, err := client.Get(url)
	if err != nil {
		return errors.New("readiness request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("readiness response was not successful")
	}
	return nil
}
