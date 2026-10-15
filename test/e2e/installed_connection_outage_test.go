//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vterdunov/external-postgres-control-operator/test/utils"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func installedConnectionServiceUnavailable(
	ctx context.Context, kube kubernetes.Interface, namespace, serviceName string,
	probe func(context.Context) (int, error),
) error {
	bounded, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	poll := time.Tick(100 * time.Millisecond)
	backends, probeCode := 0, -1
	for {
		if err := bounded.Err(); err != nil {
			return fmt.Errorf("Service %s/%s outage not confirmed: endpoints=%d pg_isready_exit=%d: %w",
				namespace, serviceName, backends, probeCode, err)
		}
		slices, err := kube.DiscoveryV1().EndpointSlices(namespace).List(bounded,
			metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + serviceName})
		if err != nil {
			return fmt.Errorf("observe outage Service EndpointSlices: %w", err)
		}
		backends = 0
		for _, slice := range slices.Items {
			backends += len(slice.Endpoints)
		}
		if backends == 0 {
			probeCode, err = probe(bounded)
			if err != nil {
				return fmt.Errorf("probe outage Service: %w", err)
			}
			switch probeCode {
			case 2:
				return bounded.Err()
			case 0, 1:
			default:
				return fmt.Errorf("pg_isready failed with exit %d", probeCode)
			}
		}
		select {
		case <-bounded.Done():
		case <-poll:
		}
	}
}

func installedConnectionProbe(ctx context.Context, host string, port int32) (int, error) {
	// Return the native status separately from kubectl transport/exec failures.
	const script = `pg_isready -q -h "$1" -p "$2" -t 1; printf '%s\n' "$?"`
	output, err := utils.Run(kubectlCommand(ctx, "-n", "epco-postgres", "exec", "deployment/postgres", "--",
		"sh", "-c", script, "sh", host, strconv.FormatInt(int64(port), 10)))
	if err != nil {
		return -1, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return -1, fmt.Errorf("pg_isready did not return a native exit status")
	}
	return code, nil
}
