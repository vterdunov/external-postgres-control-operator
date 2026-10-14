//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	r4ImageFixtureECR = "public.ecr.aws/docker/library/postgres:18.6-bookworm"
	r4ImageFixtureHub = "docker.io/library/postgres:18.6-bookworm"
	r4ImageFixtureID  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r4ImageChangedID  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type r4ImageCommandFixture struct {
	cache       map[string]string
	pullIDs     map[string]string
	pullErrors  map[string]error
	inspectErr  error
	commands    [][]string
	pulls       []string
	afterLookup func(string)
}

func (f *r4ImageCommandFixture) output(ctx context.Context, args ...string) ([]byte, error) {
	f.commands = append(f.commands, slices.Clone(args))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var reference string
	for _, candidate := range []string{r4ImageFixtureECR, r4ImageFixtureHub} {
		if slices.Contains(args, candidate) {
			reference = candidate
			break
		}
	}
	if reference == "" {
		return nil, fmt.Errorf("unexpected or unpinned Docker image arguments: %q", args)
	}
	if slices.Contains(args, "inspect") {
		if f.inspectErr != nil {
			return nil, f.inspectErr
		}
		id, exists := f.cache[reference]
		if !exists {
			return nil, fmt.Errorf("Error response from daemon: No such image: %s", reference)
		}
		if f.afterLookup != nil {
			f.afterLookup(reference)
		}
		return []byte(id + "\n"), nil
	}
	if slices.Contains(args, "pull") {
		f.pulls = append(f.pulls, reference)
		if err := f.pullErrors[reference]; err != nil {
			return nil, err
		}
		if f.cache == nil {
			f.cache = make(map[string]string)
		}
		f.cache[reference] = f.pullIDs[reference]
		return []byte("Image is up to date\n"), nil
	}
	return nil, fmt.Errorf("unexpected Docker image operation: %q", args)
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

func TestR4ResolveLibPQImage(t *testing.T) {
	for _, reference := range []string{r4ImageFixtureECR, r4ImageFixtureHub} {
		t.Run("uses cached immutable image from "+reference, func(t *testing.T) {
			fixture := &r4ImageCommandFixture{cache: map[string]string{reference: r4ImageFixtureID}}
			fixture.afterLookup = func(tag string) { fixture.cache[tag] = r4ImageChangedID }
			id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
			if err != nil || id != r4ImageFixtureID {
				t.Fatalf("cached image identity lost after its tag changed: id=%q err=%v", id, err)
			}
			if len(fixture.pulls) != 0 {
				t.Fatalf("cached official client caused a registry pull: %v", fixture.pulls)
			}
		})
	}

	t.Run("pulls the pinned ECR image on a cache miss", func(t *testing.T) {
		fixture := &r4ImageCommandFixture{pullIDs: map[string]string{r4ImageFixtureECR: r4ImageFixtureID}}
		id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
		if err != nil || id != r4ImageFixtureID || !slices.Equal(fixture.pulls, []string{r4ImageFixtureECR}) {
			t.Fatalf("unexpected uncached client resolution: id=%q pulls=%v err=%v", id, fixture.pulls, err)
		}
	})

	for _, quota := range []string{"toomanyrequests: Rate exceeded", "toomanyrequests: Data limit exceeded"} {
		t.Run("falls back only for ECR quota stderr "+quota, func(t *testing.T) {
			quotaError := r4ImageRegistryError(t, "Error response from daemon: "+quota)
			fixture := &r4ImageCommandFixture{
				pullIDs:    map[string]string{r4ImageFixtureHub: r4ImageFixtureID},
				pullErrors: map[string]error{r4ImageFixtureECR: quotaError},
			}
			id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
			if err != nil || id != r4ImageFixtureID ||
				!slices.Equal(fixture.pulls, []string{r4ImageFixtureECR, r4ImageFixtureHub}) {
				t.Fatalf("quota fallback lost pinned image or registry ordering: id=%q pulls=%v err=%v",
					id, fixture.pulls, err)
			}
		})
	}

	for _, diagnostic := range []string{
		"unauthorized: authentication required", "manifest unknown", "dial tcp: connection refused",
		"toomanyrequests: unrelated registry error", "Rate exceeded without quota error code",
	} {
		t.Run("preserves unrelated pull error "+diagnostic, func(t *testing.T) {
			pullError := r4ImageRegistryError(t, diagnostic)
			fixture := &r4ImageCommandFixture{pullErrors: map[string]error{r4ImageFixtureECR: pullError}}
			id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
			if id != "" || !errors.Is(err, pullError) || !slices.Equal(fixture.pulls, []string{r4ImageFixtureECR}) {
				t.Fatalf("unrelated error was hidden or caused fallback: id=%q pulls=%v err=%v", id, fixture.pulls, err)
			}
		})
	}

	t.Run("preserves a local Docker inspection failure without pulling", func(t *testing.T) {
		inspectionError := errors.New("permission denied while trying to connect to the Docker daemon")
		fixture := &r4ImageCommandFixture{inspectErr: inspectionError}
		id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
		if id != "" || !errors.Is(err, inspectionError) || len(fixture.pulls) != 0 {
			t.Fatalf("local Docker failure was hidden by a pull: id=%q pulls=%v err=%v", id, fixture.pulls, err)
		}
	})

	t.Run("retains both registry failures when quota fallback fails", func(t *testing.T) {
		ecrError := r4ImageRegistryError(t, "toomanyrequests: Data limit exceeded")
		hubError := r4ImageRegistryError(t, "Docker Hub authentication required")
		fixture := &r4ImageCommandFixture{pullErrors: map[string]error{
			r4ImageFixtureECR: ecrError, r4ImageFixtureHub: hubError,
		}}
		id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
		if id != "" || !errors.Is(err, ecrError) || !errors.Is(err, hubError) {
			t.Fatalf("registry failures were discarded: id=%q err=%v", id, err)
		}
	})

	for _, malformed := range []string{"", "sha256:abc", r4ImageFixtureECR, "sha256:" + strings.Repeat("z", 64),
		r4ImageFixtureID + "\n" + r4ImageChangedID} {
		t.Run("rejects malformed local image ID "+malformed, func(t *testing.T) {
			fixture := &r4ImageCommandFixture{pullIDs: map[string]string{r4ImageFixtureECR: malformed}}
			id, err := r4ResolveLibPQImage(t.Context(), fixture.output)
			if err == nil || id != "" {
				t.Fatalf("invalid local image identity was accepted: id=%q err=%v", id, err)
			}
		})
	}

	r4ImageCancellationCases(t)
}

func r4ImageCancellationCases(t *testing.T) {
	t.Helper()
	t.Run("cancellation before resolution performs no Docker operations", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		fixture := &r4ImageCommandFixture{}
		id, err := r4ResolveLibPQImage(ctx, fixture.output)
		if id != "" || !errors.Is(err, context.Canceled) || len(fixture.commands) != 0 {
			t.Fatalf("canceled resolution issued Docker commands: id=%q commands=%v err=%v", id, fixture.commands, err)
		}
	})

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
