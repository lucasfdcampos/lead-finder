// cmd/test_instagram is a standalone CLI that exercises GenerateHandleVariations
// for a set of sample businesses and writes the results to /tmp/handles_test.json.
//
// Usage:
//
//	go run ./cmd/test_instagram/
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/lucasfdcampos/lead-finder/internal/provider/instagram"
)

type businessCase struct {
	Name    string
	City    string
	Handles []string
}

func main() {
	cases := []struct {
		name string
		city string
	}{
		{"La Femme", "arapongas"},
		{"Dr Pizza", "holambra"},
		{"Hamburgueria do Zé", "londrina"},
		{"Barbearia Kings", "maringa"},
		{"Loja das Flores", ""},
		{"Auto Peças Silva & Irmãos", "curitiba"},
	}

	results := make([]businessCase, 0, len(cases))

	for _, c := range cases {
		handles := instagram.GenerateHandleVariations(c.name, c.city)
		results = append(results, businessCase{
			Name:    c.name,
			City:    c.city,
			Handles: handles,
		})

		fmt.Printf("\n=== %s (city: %q) — %d variants ===\n", c.name, c.city, len(handles))
		for i, h := range handles {
			fmt.Printf("  [%02d] %s\n", i+1, h)
		}
	}

	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		log.Fatalf("json marshal: %v", err)
	}

	dest := "/tmp/handles_test.json"
	if err := os.WriteFile(dest, out, 0o644); err != nil {
		log.Fatalf("write file: %v", err)
	}

	fmt.Printf("\n✓ Results written to %s\n", dest)
}
