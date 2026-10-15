//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"unicode"
)

var (
	r4LibPQImageID       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	r4ImageCredentialURL = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s]+@`)
)

type r4ImageCommandError struct {
	message string
	cause   error
}

func (e *r4ImageCommandError) Error() string { return e.message }
func (e *r4ImageCommandError) Unwrap() error { return e.cause }

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
	reference, err := ImageForVersion("18")
	if err != nil {
		return "", err
	}
	data, err := invoke("pull", reference)
	if err != nil {
		return "", r4ImageFailure("pull pinned application client image", data, err)
	}
	data, err = invoke("image", "inspect", "--format", "{{.Id}}", reference)
	if err != nil {
		return "", r4ImageFailure("inspect pinned application client image", data, err)
	}
	id := strings.TrimSpace(string(data))
	if !r4LibPQImageID.MatchString(id) {
		return "", fmt.Errorf("pinned application client image has an invalid immutable ID")
	}
	return id, nil
}

func r4ImageFailure(operation string, data []byte, err error) error {
	diagnostic := string(data)
	if failure, ok := errors.AsType[*exec.ExitError](err); ok {
		diagnostic += " " + string(failure.Stderr)
	}
	diagnostic += " " + err.Error()
	diagnostic = r4ImageCredentialURL.ReplaceAllString(diagnostic, "[REDACTED_URL]@")
	diagnostic = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, diagnostic)
	message := operation + ": " + strings.Join(strings.Fields(diagnostic), " ")
	const limit = 4096
	if len(message) > limit {
		message = strings.ToValidUTF8(message[:limit], "")
	}
	return &r4ImageCommandError{message: message, cause: err}
}
