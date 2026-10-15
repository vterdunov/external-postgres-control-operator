//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

const (
	outageFixtureNamespace = "owned-outage-fixture"
	outageFixtureService   = "application-postgres"
	outageNoResponse       = 2
)

func outageEndpointSlice(service string, endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		Namespace: outageFixtureNamespace, Name: service + "-endpoint",
		Labels:      map[string]string{discoveryv1.LabelServiceName: service},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
	}
}

func outageBarrierContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func TestInstalledConnectionServiceUnavailable(t *testing.T) {
	t.Run("requires an actual no-response probe and ignores another Service's endpoints", func(t *testing.T) {
		client := kubefake.NewClientset(
			outageEndpointSlice(outageFixtureService),
			outageEndpointSlice("unrelated-postgres", discoveryv1.Endpoint{
				Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
			}),
		)
		probed := false
		err := installedConnectionServiceUnavailable(outageBarrierContext(t), client,
			outageFixtureNamespace, outageFixtureService, func(context.Context) (int, error) {
				probed = true
				return outageNoResponse, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if !probed {
			t.Fatal("empty endpoints alone do not prove SQL unavailability")
		}
	})

	for _, readiness := range []struct {
		name  string
		ready *bool
	}{
		{name: "ready", ready: new(true)},
		{name: "unknown readiness"},
		{name: "not ready", ready: new(false)},
	} {
		t.Run("waits for withdrawal of "+readiness.name+" endpoints", func(t *testing.T) {
			client := kubefake.NewClientset(outageEndpointSlice(outageFixtureService, discoveryv1.Endpoint{
				Addresses: []string{"10.0.0.3"}, Conditions: discoveryv1.EndpointConditions{Ready: readiness.ready},
			}))
			ctx := outageBarrierContext(t)
			err := installedConnectionServiceUnavailable(ctx, client,
				outageFixtureNamespace, outageFixtureService, func(context.Context) (int, error) {
					return outageNoResponse, nil
				})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("remaining endpoints were accepted as withdrawn: %v", err)
			}
		})
	}

	for _, response := range []struct {
		name string
		code int
	}{
		{name: "accepting", code: 0},
		{name: "rejecting", code: 1},
	} {
		t.Run("waits while SQL is "+response.name+" despite empty endpoints", func(t *testing.T) {
			client := kubefake.NewClientset(outageEndpointSlice(outageFixtureService))
			err := installedConnectionServiceUnavailable(outageBarrierContext(t), client,
				outageFixtureNamespace, outageFixtureService, func(context.Context) (int, error) {
					return response.code, nil
				})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("responding SQL endpoint was accepted as an outage: %v", err)
			}
		})
	}

	for _, failure := range []struct {
		name string
		code int
		err  error
	}{
		{name: "exec failure", code: outageNoResponse, err: errors.New("probe transport unavailable")},
		{name: "tool misuse", code: 3},
		{name: "unknown exit code", code: 126},
	} {
		t.Run("does not mistake "+failure.name+" for a SQL outage", func(t *testing.T) {
			client := kubefake.NewClientset(outageEndpointSlice(outageFixtureService))
			err := installedConnectionServiceUnavailable(outageBarrierContext(t), client,
				outageFixtureNamespace, outageFixtureService, func(context.Context) (int, error) {
					return failure.code, failure.err
				})
			if err == nil {
				t.Fatal("probe failure falsely established SQL unavailability")
			}
		})
	}

	t.Run("does not mistake an EndpointSlice API failure for withdrawal", func(t *testing.T) {
		client := kubefake.NewClientset()
		client.PrependReactor("list", "endpointslices", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("endpoint API unavailable")
		})
		err := installedConnectionServiceUnavailable(outageBarrierContext(t), client,
			outageFixtureNamespace, outageFixtureService, func(context.Context) (int, error) {
				return outageNoResponse, nil
			})
		if err == nil {
			t.Fatal("endpoint observation failure falsely established withdrawal")
		}
	})

	t.Run("honors cancellation before declaring an outage", func(t *testing.T) {
		client := kubefake.NewClientset(outageEndpointSlice(outageFixtureService))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := installedConnectionServiceUnavailable(ctx, client,
			outageFixtureNamespace, outageFixtureService, func(context.Context) (int, error) {
				return outageNoResponse, nil
			})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled fixture setup returned %v", err)
		}
	})
}
