//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

const (
	r4LibPQImageTag = "18.6-bookworm"
	r4LibPQImage    = "public.ecr.aws/docker/library/postgres:" + r4LibPQImageTag
	r4LibPQMirror   = "docker.io/library/postgres:" + r4LibPQImageTag
)

var r4LibPQImageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func r4ResolveLibPQImage(
	ctx context.Context, output func(context.Context, ...string) ([]byte, error),
) (string, error) {
	invoke := func(args ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := output(ctx, args...)
		if canceled := ctx.Err(); canceled != nil {
			return nil, canceled
		}
		return data, err
	}
	for _, reference := range []string{r4LibPQImage, r4LibPQMirror} {
		data, err := invoke("image", "inspect", "--format", "{{.Id}}", reference)
		if err == nil {
			return r4LibPQIdentity(data)
		}
		if !strings.Contains(r4ImageDiagnostic(data, err), "no such image: "+reference) {
			return "", fmt.Errorf("inspect pinned application client image: %w", err)
		}
	}
	reference := r4LibPQImage
	data, err := invoke("pull", reference)
	if err != nil {
		diagnostic := r4ImageDiagnostic(data, err)
		if !strings.Contains(diagnostic, "toomanyrequests") ||
			!strings.Contains(diagnostic, "rate exceeded") && !strings.Contains(diagnostic, "data limit exceeded") {
			return "", fmt.Errorf("pull pinned application client image: %w", err)
		}
		reference = r4LibPQMirror
		if _, fallbackErr := invoke("pull", reference); fallbackErr != nil {
			return "", fmt.Errorf("pull mirrored application client image: %w", errors.Join(err, fallbackErr))
		}
	}
	data, err = invoke("image", "inspect", "--format", "{{.Id}}", reference)
	if err != nil {
		return "", fmt.Errorf("inspect pulled application client image: %w", err)
	}
	return r4LibPQIdentity(data)
}

func r4LibPQIdentity(data []byte) (string, error) {
	id := strings.TrimSpace(string(data))
	if !r4LibPQImageID.MatchString(id) {
		return "", fmt.Errorf("pinned application client image has an invalid immutable ID")
	}
	return id, nil
}

func r4ImageDiagnostic(data []byte, err error) string {
	diagnostic := string(data)
	if failure, ok := errors.AsType[*exec.ExitError](err); ok {
		diagnostic += "\n" + string(failure.Stderr)
	}
	if err != nil {
		diagnostic += "\n" + err.Error()
	}
	return strings.ToLower(diagnostic)
}
