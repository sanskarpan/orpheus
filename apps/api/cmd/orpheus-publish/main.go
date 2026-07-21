// Command orpheus-publish is the processor marketplace publisher CLI (Phase 7).
//
// It submits a community processor to the marketplace moderation queue via
// POST /v1/marketplace/submissions. On approval the processor is promoted into
// the public catalog as a community-trust processor.
//
//	orpheus-publish \
//	  -name community.my-proc -display "My Processor" \
//	  -publisher acme -description "does a thing"
//
// Env: ORPHEUS_API_URL (default http://localhost:8080), ORPHEUS_API_KEY.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "orpheus-publish:", err)
		os.Exit(1)
	}
}

func run() error {
	name := flag.String("name", "", "processor name (e.g. community.my-proc)")
	display := flag.String("display", "", "human display name")
	description := flag.String("description", "", "short description")
	publisher := flag.String("publisher", "", "publisher handle")
	flag.Parse()

	if *name == "" || *display == "" || *publisher == "" {
		return fmt.Errorf("-name, -display and -publisher are required")
	}
	base := envOr("ORPHEUS_API_URL", "http://localhost:8080")
	key := os.Getenv("ORPHEUS_API_KEY")
	if key == "" {
		return fmt.Errorf("ORPHEUS_API_KEY is required")
	}

	body, _ := json.Marshal(map[string]string{
		"name": *name, "display_name": *display,
		"description": *description, "publisher": *publisher,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/marketplace/submissions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("submission failed (%d): %s", resp.StatusCode, out)
	}
	fmt.Printf("submitted %s — %s\n", *name, out)
	return nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
