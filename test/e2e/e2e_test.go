//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/test/utils"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const operatorNamespace = "epco-system"

var _ = Describe("Installed operator", func() {
	It("has one available manager and no restarting containers", func() {
		ctx, cancel := boundedContext()
		defer cancel()
		Eventually(func(g Gomega) {
			deployment, err := kubeClient.AppsV1().Deployments(operatorNamespace).Get(
				ctx, "epco-controller-manager", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ObservedGeneration).To(Equal(deployment.Generation))
			g.Expect(deployment.Status.AvailableReplicas).To(Equal(int32(1)))
			g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)))
			pods, err := kubeClient.CoreV1().Pods(operatorNamespace).List(
				ctx, metav1.ListOptions{LabelSelector: "control-plane=controller-manager"})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).To(HaveLen(1))
			g.Expect(pods.Items[0].DeletionTimestamp).To(BeNil())
			g.Expect(pods.Items[0].Status.Phase).To(Equal(corev1.PodRunning))
			g.Expect(pods.Items[0].Status.ContainerStatuses).NotTo(BeEmpty())
			for _, status := range pods.Items[0].Status.ContainerStatuses {
				g.Expect(status.State.Running).NotTo(BeNil())
				g.Expect(status.Ready).To(BeTrue())
				g.Expect(status.RestartCount).To(BeZero())
			}
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
	})

	It("serves health probes and permits only authorized HTTPS metrics requests", func() {
		ctx, cancel := boundedContext()
		defer cancel()
		authorizedToken := metricsToken(ctx, "metrics-reader", true)
		forbiddenToken := metricsToken(ctx, "metrics-forbidden", false)
		ports := forwardManagerPorts()
		// The manager generates an ephemeral self-signed certificate in this fixture.
		transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
		defer transport.CloseIdleConnections()
		httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		for _, path := range []string{"healthz", "readyz"} {
			status, body := request(httpClient, fmt.Sprintf("http://127.0.0.1:%d/%s", ports[8081], path), "")
			Expect(status).To(Equal(http.StatusOK))
			Expect(body).To(Equal("ok"))
		}
		endpoint := fmt.Sprintf("https://127.0.0.1:%d/metrics", ports[8443])
		Eventually(func(g Gomega) {
			status, body := request(httpClient, endpoint, authorizedToken)
			g.Expect(status).To(Equal(http.StatusOK))
			g.Expect(body).To(ContainSubstring("go_goroutines"))
		}, 30*time.Second, time.Second).Should(Succeed())
		status, _ := request(httpClient, endpoint, "")
		Expect(status).To(Equal(http.StatusUnauthorized))
		status, _ = request(httpClient, endpoint, forbiddenToken)
		Expect(status).To(Equal(http.StatusForbidden))
	})

	It("verifies the selected PostgreSQL server over authenticated TCP "+
		"and the independently pinned client", func(ctx SpecContext) {
		namespace := os.Getenv("EPCO_E2E_POSTGRES_NAMESPACE")
		Expect(namespace).To(Equal("epco-postgres"))
		deployment, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, "postgres", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal(os.Getenv("EPCO_E2E_POSTGRES_IMAGE")))
		cmd := kubectlCommand(ctx, "-n", namespace, "exec", "deployment/postgres", "--",
			"psql", "-h", "127.0.0.1", "-U", "postgres", "-Atc",
			"SELECT current_user, current_database(), current_setting('server_version_num'), version()")
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		fields := strings.Split(strings.TrimSpace(output), "|")
		Expect(fields).To(HaveLen(4))
		Expect(fields[:2]).To(Equal([]string{installedDatabasePostgresName, installedDatabasePostgresName}))
		serverNumber, err := strconv.Atoi(fields[2])
		Expect(err).NotTo(HaveOccurred())
		Expect(strconv.Itoa(serverNumber / 10000)).To(Equal(os.Getenv("EPCO_PG_VERSION")))
		job := &batchv1.Job{Namespace: namespace, Name: "client-version",
			Spec: batchv1.JobSpec{BackoffLimit: new(int32(0)), ActiveDeadlineSeconds: new(int64(60)),
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: new(false),
					Containers: []corev1.Container{{Name: r6MigrationContainer, Image: installedClientImage(),
						Command: []string{"psql", "--version"}}},
				}}}}
		_, err = kubeClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanup, stop := boundedContext()
			defer stop()
			Expect(kubeClient.BatchV1().Jobs(namespace).Delete(cleanup, job.Name,
				metav1.DeleteOptions{PropagationPolicy: new(metav1.DeletePropagationForeground)})).To(Succeed())
		})
		var clientVersion string
		Eventually(func(g Gomega) {
			latest, getErr := kubeClient.BatchV1().Jobs(namespace).Get(ctx, job.Name, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(latest.Status.Succeeded).To(Equal(int32(1)))
			clientVersion = strings.TrimSpace(installedConnectionJobLogs(ctx, job))
			g.Expect(clientVersion).To(HavePrefix("psql (PostgreSQL) 16.15"))
		}).WithContext(ctx).WithTimeout(65 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		observed := fmt.Sprintf("server authenticated identity: %s|%s\nserver_version_num: %d\n"+
			"server actual version: %s\nclient actual version: %s\n",
			fields[0], fields[1], serverNumber, fields[3], clientVersion)
		Expect(os.WriteFile(filepath.Join(os.Getenv("EPCO_E2E_ARTIFACTS"), "fixture-observed.txt"),
			[]byte(observed), 0600)).To(Succeed())
	}, NodeTimeout(90*time.Second))

	It("supports create, read, update and delete for each existing CRD", func() {
		ctx, cancel := boundedContext()
		defer cancel()
		secret := &corev1.Secret{Name: "crud-admin", Namespace: operatorNamespace, Type: corev1.SecretTypeBasicAuth,
			StringData: map[string]string{corev1.BasicAuthUsernameKey: "postgres",
				corev1.BasicAuthPasswordKey: "fixture-only-no-controller"}}
		Expect(apiClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { deleteObject(secret) })
		cluster := &epco.ExternalPostgres{Name: "crud-cluster", Namespace: operatorNamespace,
			Spec: epco.ExternalPostgresSpec{
				Connection:        epco.PostgresConnection{Host: "postgres.epco-postgres.svc.cluster.local"},
				CredentialsSecret: epco.LocalReference{Name: secret.Name}, TLS: &epco.PostgresTLS{Mode: "disable"},
			}}
		role := &epco.DatabaseRole{Name: "crud-role", Namespace: operatorNamespace,
			Spec: epco.DatabaseRoleSpec{Cluster: epco.ClusterReference{Name: cluster.Name}}}
		database := &epco.Database{Name: "crud-database", Namespace: operatorNamespace,
			Spec: epco.DatabaseSpec{Cluster: epco.ClusterReference{Name: cluster.Name}, Owner: "crud-role"}}
		for _, object := range []client.Object{cluster, role, database} {
			Expect(apiClient.Create(ctx, object)).To(Succeed())
			DeferCleanup(func() { deleteObject(object) })
			read := object.DeepCopyObject().(client.Object)
			Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(object), read)).To(Succeed())
			Expect(read.GetUID()).To(Equal(object.GetUID()))
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				fresh := object.DeepCopyObject().(client.Object)
				if err := apiClient.Get(ctx, client.ObjectKeyFromObject(object), fresh); err != nil {
					return err
				}
				annotations := fresh.GetAnnotations()
				if annotations == nil {
					annotations = map[string]string{}
				}
				annotations["epco.io/e2e"] = "updated"
				fresh.SetAnnotations(annotations)
				return apiClient.Update(ctx, fresh)
			})).To(Succeed())
			Expect(apiClient.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
			Expect(object.GetAnnotations()).To(HaveKeyWithValue("epco.io/e2e", "updated"))
			deleteObject(object)
		}
	})
})

func metricsToken(ctx context.Context, name string, authorized bool) string {
	sa := &corev1.ServiceAccount{Name: name, Namespace: operatorNamespace}
	Expect(apiClient.Create(ctx, sa)).To(Succeed())
	DeferCleanup(func() { deleteObject(sa) })
	if authorized {
		binding := &rbacv1.ClusterRoleBinding{Name: "epco-e2e-metrics-reader",
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "epco-metrics-reader"},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: operatorNamespace}}}
		Expect(apiClient.Create(ctx, binding)).To(Succeed())
		DeferCleanup(func() { deleteObject(binding) })
	}
	expiration := int64(600)
	token, err := kubeClient.CoreV1().ServiceAccounts(operatorNamespace).CreateToken(ctx, name,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiration}},
		metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	return token.Status.Token
}

func request(httpClient *http.Client, endpoint, token string) (int, string) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := httpClient.Do(request)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return response.StatusCode, string(body)
}

func deleteObject(object client.Object) {
	ctx, cancel := boundedContext()
	defer cancel()
	err := apiClient.Delete(ctx, object)
	ExpectWithOffset(1, client.IgnoreNotFound(err)).To(Succeed())
	Eventually(func(g Gomega) {
		copy := object.DeepCopyObject().(client.Object)
		err := apiClient.Get(ctx, client.ObjectKeyFromObject(object), copy)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"Waiting for deletion of %T %s (finalizers %v): %v",
			object, client.ObjectKeyFromObject(object), copy.GetFinalizers(), err)
	}).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func kubectlCommand(ctx context.Context, args ...string) *exec.Cmd {
	commandArgs := make([]string, 0, 5+len(args))
	commandArgs = append(commandArgs, "--kubeconfig", kubeconfigPath, "--context", kubeContext, "--request-timeout=20s")
	commandArgs = append(commandArgs, args...)
	return exec.CommandContext(ctx, "kubectl", commandArgs...)
}

// Parse only localhost forwarding announcements; an OS-selected port avoids races.
func parseForwardedPorts(output string) map[int]int {
	ports := map[int]int{}
	for line := range strings.SplitSeq(output, "\n") {
		var local, remote int
		if _, err := fmt.Sscanf(line, "Forwarding from 127.0.0.1:%d -> %d", &local, &remote); err == nil {
			ports[remote] = local
		}
	}
	return ports
}

func forwardManagerPorts() map[int]int {
	ctx, cancel := context.WithCancel(context.Background())
	DeferCleanup(cancel)
	log, err := os.CreateTemp(filepath.Dir(kubeconfigPath), "epco-e2e-forward-*.log")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		Expect(log.Close()).To(Succeed())
		Expect(os.Remove(log.Name())).To(Succeed())
	})
	cmd := kubectlCommand(ctx, "-n", operatorNamespace, "port-forward", "--address=127.0.0.1",
		"deployment/epco-controller-manager", "0:8081", "0:8443")
	cmd.Stdout, cmd.Stderr = log, log
	Expect(cmd.Start()).To(Succeed())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	DeferCleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			Fail("port-forward did not stop")
		}
	})
	var ports map[int]int
	Eventually(func(g Gomega) {
		output, readErr := os.ReadFile(log.Name())
		g.Expect(readErr).NotTo(HaveOccurred())
		ports = parseForwardedPorts(string(output))
		g.Expect(ports).To(HaveLen(2), "Port-forward output: %s", utils.Redact(string(output)))
	}, 20*time.Second, 100*time.Millisecond).Should(Succeed())
	return ports
}
