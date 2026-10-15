// Package images supplies immutable GHCR references shared by test fixtures.
package images

import (
	_ "embed"
	"encoding/json/v2"
	"fmt"
	"strings"
)

//go:embed catalog.json
var catalog []byte

// Reference resolves a catalog name to its mirrored tag and upstream digest.
func Reference(name string) (string, error) {
	var document struct {
		Images []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			Destination string `json:"destination"`
		} `json:"images"`
	}
	if err := json.Unmarshal(catalog, &document); err != nil {
		return "", fmt.Errorf("decode image catalog: %w", err)
	}
	for _, image := range document.Images {
		if image.Name == name {
			_, digest, ok := strings.Cut(image.Source, "@")
			if !ok {
				return "", fmt.Errorf("catalog image %q is not pinned", name)
			}
			return image.Destination + "@" + digest, nil
		}
	}
	return "", fmt.Errorf("unknown catalog image %q", name)
}
