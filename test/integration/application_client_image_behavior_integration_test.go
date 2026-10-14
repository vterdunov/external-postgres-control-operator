//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
)

const (
	r4DockerPull         = "pull"
	r4DockerImage        = "image"
	r4DockerInspect      = "inspect"
	r4ImageFixturePrefix = "ghcr.io/vterdunov/external-postgres-control-operator:mirror-postgres-18.6-bookworm@sha256:"
	r4ImageFixtureID     = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r4ImageChangedID     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var r4ImageFixtureReference = regexp.MustCompile("^" + regexp.QuoteMeta(r4ImageFixturePrefix) + "[a-f0-9]{64}$")

type r4ImageCommandFixture struct {
	pullData, inspectData []byte
	pullErr, inspectErr   error
	commands              [][]string
}

func (f *r4ImageCommandFixture) output(_ context.Context, args ...string) ([]byte, error) {
	f.commands = append(f.commands, slices.Clone(args))
	if len(args) == 0 || !r4ImageFixtureReference.MatchString(args[len(args)-1]) {
		return nil, fmt.Errorf("unexpected or unpinned Docker image arguments: %q", args)
	}
	switch args[0] {
	case r4DockerPull:
		return f.pullData, f.pullErr
	case r4DockerImage:
		return f.inspectData, f.inspectErr
	default:
		return nil, fmt.Errorf("unexpected Docker image operation: %q", args)
	}
}

func r4ImageRegistryError(t *testing.T, message string) error {
	t.Helper()
	command := exec.CommandContext(t.Context(), "sh", "-c", `printf '%s' "$1" >&2; exit 125`,
		"registry-fixture", message)
	_, err := command.Output()
	failure, ok := errors.AsType[*exec.ExitError](err)
	if !ok || !strings.Contains(string(failure.Stderr), message) {
		t.Fatalf("registry error fixture did not capture actual command stderr: %v", err)
	}
	return err
}

func r4RequireImageCommands(t *testing.T, commands [][]string, inspect bool) {
	t.Helper()
	wantCount := 1
	if inspect {
		wantCount++
	}
	if len(commands) != wantCount || len(commands[0]) != 2 || commands[0][0] != r4DockerPull ||
		!r4ImageFixtureReference.MatchString(commands[0][1]) {
		t.Fatalf("must pull only the pinned GHCR client before inspection: %v", commands)
	}
	if inspect {
		want := []string{r4DockerImage, r4DockerInspect, "--format", "{{.Id}}", commands[0][1]}
		if !slices.Equal(commands[1], want) {
			t.Fatalf("inspection changed the pulled reference or immutable ID format: %v", commands)
		}
	}
}

func TestR4ResolveLibPQImage(t *testing.T) {
	fixture := &r4ImageCommandFixture{inspectData: []byte(r4ImageFixtureID + "\n")}
	id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
	if err != nil || id != r4ImageFixtureID {
		t.Fatalf("pinned client resolution lost immutable identity: id=%q err=%v", id, err)
	}
	r4RequireImageCommands(t, fixture.commands, true)
}

func TestR4ResolveLibPQImageDiagnostics(t *testing.T) {
	for _, stage := range []string{r4DockerPull, r4DockerInspect} {
		for _, diagnostic := range []string{
			"Cannot connect to the Docker daemon", "dial tcp: connection refused", "unauthorized: authentication required",
		} {
			for _, transport := range []string{"combined output", "exit stderr"} {
				t.Run(stage+"/"+transport+"/"+diagnostic, func(t *testing.T) {
					failure := errors.New("fixture command failed")
					data := []byte(diagnostic)
					if transport == "exit stderr" {
						failure = r4ImageRegistryError(t, diagnostic)
						data = nil
					}
					fixture := &r4ImageCommandFixture{inspectData: []byte(r4ImageFixtureID)}
					if stage == r4DockerPull {
						fixture.pullData, fixture.pullErr = data, failure
					} else {
						fixture.inspectData, fixture.inspectErr = data, failure
					}
					id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
					if id != "" || !errors.Is(err, failure) || !strings.Contains(err.Error(), diagnostic) {
						t.Fatalf("command cause or actionable diagnostic was lost: id=%q err=%v", id, err)
					}
					r4RequireImageCommands(t, fixture.commands, stage == r4DockerInspect)
				})
			}
		}
	}
}

func TestR4ResolveLibPQImageSanitizesAndBoundsDiagnostics(t *testing.T) {
	const credential = "fixture-private-password"
	for _, stage := range []string{r4DockerPull, r4DockerInspect} {
		t.Run(stage, func(t *testing.T) {
			diagnostic := "unauthorized https://fixture-user:" + credential + "@ghcr.io/v2/\x1b[31m\x00\n" +
				strings.Repeat("registry detail ", 1000)
			fixture := &r4ImageCommandFixture{inspectData: []byte(r4ImageFixtureID)}
			failure := errors.New("fixture command failed")
			if stage == r4DockerPull {
				fixture.pullData, fixture.pullErr = []byte(diagnostic), failure
			} else {
				fixture.inspectData, fixture.inspectErr = []byte(diagnostic), failure
			}
			id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
			if err == nil || id != "" {
				t.Fatalf("failed command produced a usable image: id=%q err=%v", id, err)
			}
			message := err.Error()
			if !strings.Contains(message, "unauthorized") || strings.Contains(message, credential) || len(message) > 4096 ||
				strings.ContainsFunc(message, unicode.IsControl) {
				t.Fatalf("diagnostic is not actionable, sanitized, and bounded: %.200q", message)
			}
		})
	}
}

func TestR4ResolveLibPQImageRejectsMalformedIdentity(t *testing.T) {
	for _, malformed := range []string{"", "sha256:abc", r4ImageFixturePrefix,
		"sha256:" + strings.Repeat("z", 64), "sha256:" + strings.Repeat("A", 64),
		r4ImageFixtureID + "\n" + r4ImageChangedID} {
		t.Run(malformed, func(t *testing.T) {
			fixture := &r4ImageCommandFixture{inspectData: []byte(malformed)}
			id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
			if err == nil || id != "" {
				t.Fatalf("invalid local image identity was accepted: id=%q err=%v", id, err)
			}
			r4RequireImageCommands(t, fixture.commands, true)
		})
	}
}

func TestR4ResolveLibPQImageCancellation(t *testing.T) {
	t.Run("before resolution performs no Docker operations", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		fixture := &r4ImageCommandFixture{}
		id, err := r4ResolveLibPQImage(ctx, fixture.output)
		if id != "" || !errors.Is(err, context.Canceled) || len(fixture.commands) != 0 {
			t.Fatalf("canceled resolution issued Docker commands: id=%q commands=%v err=%v", id, fixture.commands, err)
		}
	})
	for _, stage := range []string{r4DockerPull, r4DockerInspect} {
		t.Run("after "+stage+" discards successful command output", func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture := &r4ImageCommandFixture{inspectData: []byte(r4ImageFixtureID)}
			id, err := r4ResolveLibPQImage(ctx, func(commandContext context.Context, args ...string) ([]byte, error) {
				data, commandErr := fixture.output(commandContext, args...)
				if args[0] == stage || stage == r4DockerInspect && args[0] == r4DockerImage {
					cancel()
				}
				return data, commandErr
			})
			if id != "" || !errors.Is(err, context.Canceled) {
				t.Fatalf("command result escaped cancellation: id=%q err=%v", id, err)
			}
			r4RequireImageCommands(t, fixture.commands, stage == r4DockerInspect)
		})
	}
	t.Run("in-flight Docker work observes the supplied deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		called := false
		id, err := r4ResolveLibPQImage(ctx, func(commandContext context.Context, _ ...string) ([]byte, error) {
			called = true
			deadline, ok := commandContext.Deadline()
			expected, _ := ctx.Deadline()
			if !ok || deadline.After(expected) {
				t.Error("Docker command escaped the caller's deadline")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		})
		if !called || id != "" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline was lost: called=%t id=%q err=%v", called, id, err)
		}
	})
}
