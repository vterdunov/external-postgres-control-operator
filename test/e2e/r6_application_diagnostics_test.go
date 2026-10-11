//go:build e2e

package e2e

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/test/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (app *r6Application) quiescent(ctx context.Context, objectOID, granteeOID string) {
	app.ready(ctx)
	for _, name := range []string{r6Migrator, r6Runtime, r6Reader, r6Named} {
		role := app.roles[name]
		ExpectWithOffset(1, role.Status.PasswordApplied).NotTo(BeNil())
		ExpectWithOffset(1, role.Status.PasswordApplied.ClusterGeneration).To(Equal(app.cluster.Generation))
		ExpectWithOffset(1, role.Status.PasswordApplied.ClusterUID).To(Equal(string(app.cluster.UID)))
	}
	ExpectWithOffset(1, app.namedBindingConfirmed(objectOID, granteeOID)).To(BeTrue(),
		"exact named grant must be confirmed before creating its cleanup obligation")
	Consistently(func() string {
		return installedDatabaseSQL(ctx, "postgres",
			"SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' "+
				"AND pid<>pg_backend_pid() AND (datname='"+string(app.database.Spec.Name)+
				"' OR (datname='postgres' AND usename='postgres' AND application_name=''))")
	}).WithContext(ctx).WithTimeout(3*time.Second).WithPolling(100*time.Millisecond).Should(Equal("0"),
		"the owned target and current empty-application-name administrative sessions must be quiescent before locking")
}

func (app *r6Application) namedBindingConfirmed(objectOID, granteeOID string) bool {
	for _, target := range app.database.Status.ManagedACLTargets {
		if target.Database != string(app.database.Spec.Name) {
			continue
		}
		for _, binding := range target.Bindings {
			if binding.Scope == r6ACLTable && binding.Class == r6ACLTable &&
				binding.Schema == installedDatabaseApplicationSchema &&
				binding.Table == r6Orders && binding.Grantee == string(app.roles[r6Named].Spec.Name) &&
				binding.Privilege == "SELECT" && binding.Pending == "" &&
				binding.Generation == app.database.Generation && r6BindingOIDsMatch(binding, objectOID, granteeOID) {
				return true
			}
		}
	}
	return false
}

// Defer this on the spec goroutine. Capture before Ginkgo invokes any cleanup,
// then rethrow its original failure so diagnostics cannot change the verdict.
func r6CaptureFailedRevoke(app *r6Application) {
	if failure := recover(); failure != nil {
		r6WriteRevokeSnapshot(app)
		panic(failure)
	}
}

type r6RevokeSnapshot struct {
	Namespace       string                  `json:"namespace"`
	Name            string                  `json:"name"`
	UID             string                  `json:"uid"`
	Generation      int64                   `json:"generation"`
	ActiveACLTarget string                  `json:"activeACLTarget"`
	Targets         []epco.ManagedACLTarget `json:"managedACLTargets"`
	Conditions      []metav1.Condition      `json:"conditions"`
	DatabaseRead    bool                    `json:"databaseRead"`
	BackendRead     bool                    `json:"backendRead"`
	BackendRows     string                  `json:"backendRows"`
}

func r6WriteRevokeSnapshot(app *r6Application) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	current := &epco.Database{}
	snapshot := r6RevokeSnapshot{Namespace: app.database.Namespace, Name: app.database.Name}
	if apiClient.Get(ctx, client.ObjectKeyFromObject(app.database), current) == nil {
		snapshot.DatabaseRead = true
		snapshot.UID, snapshot.Generation = string(current.UID), current.Generation
		snapshot.ActiveACLTarget, snapshot.Targets = current.Status.ActiveACLTarget, current.Status.ManagedACLTargets
		snapshot.Conditions = current.Status.Conditions
	}
	// Never export pg_stat_activity.query. Even unrelated operator statements may
	// contain passwords; this server-side category exposes only the work class.
	sql := "SELECT pid,datname,usename,application_name,state,COALESCE(wait_event_type,''), " +
		"COALESCE(wait_event,''),pg_blocking_pids(pid), CASE " +
		"WHEN query ILIKE '%ALTER ROLE%' THEN 'role-change' " +
		"WHEN query ILIKE '%REVOKE%' THEN 'revoke' WHEN query ILIKE '%GRANT%' THEN 'grant' " +
		"WHEN query ILIKE '%pg_class%' OR query ILIKE '%pg_namespace%' THEN 'catalog-read' " +
		"WHEN query ILIKE '%pg_sleep%' THEN 'owned-blocker' ELSE 'other' END " +
		"FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid() " +
		"AND datname IN ('" + string(app.database.Spec.Name) + "','postgres') ORDER BY pid"
	cmd := kubectlCommand(ctx, "-n", "epco-postgres", "exec", "deployment/postgres", "--",
		"psql", "-X", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1", "-U", "postgres", "-d", "postgres", "-Atc", sql)
	rows, err := utils.Run(cmd)
	snapshot.BackendRead = installedConnectionSucceeded(err)
	if snapshot.BackendRead {
		snapshot.BackendRows = strings.TrimSpace(rows)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		GinkgoWriter.Println("R6 credential-free snapshot encoding failed")
		return
	}
	AddReportEntry("R6 failure-only revoke snapshot before blocker cleanup",
		string(data), ReportEntryVisibilityFailureOrVerbose)
	if directory := os.Getenv("EPCO_E2E_ARTIFACTS"); directory != "" {
		if os.WriteFile(filepath.Join(directory, "r6-revoke-failure-"+app.namespace.Name+".json"), data, 0600) != nil {
			GinkgoWriter.Println("R6 credential-free snapshot file could not be written")
		}
	}
}
