package controller

import (
	"encoding/json/v2"
	"math"
	"time"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

const unverifiedPasswordReassertionInterval = time.Hour

func passwordApplicationReceipt(role *epco.DatabaseRole, cluster *epco.ExternalPostgres, source *corev1.Secret) *epco.PasswordApplicationReceipt {
	if source == nil {
		return nil
	}
	endpoint := roleEndpoint(role, cluster)
	return &epco.PasswordApplicationReceipt{SourceNamespace: source.Namespace, SourceName: source.Name,
		SourceUID: string(source.UID), SourceResourceVersion: source.ResourceVersion,
		RoleUID: string(role.UID), RoleGeneration: role.Generation, ClusterUID: string(cluster.UID), ClusterGeneration: cluster.Generation,
		Host: endpoint.Host, Port: endpoint.Port}
}

func recentPasswordWrite(role *epco.DatabaseRole, receipt *epco.PasswordApplicationReceipt, now time.Time) *epco.RolePasswordWrite {
	write := role.Status.PasswordWrite
	if receipt == nil || write == nil || !equality.Semantic.DeepEqual(role.Status.PasswordApplied, receipt) {
		return nil
	}
	if write.RoleOID <= 0 || write.RoleOID > math.MaxUint32 || write.ServerStartUnixMicro <= 0 || write.LastWriteUnixMicro <= 0 {
		return nil
	}
	age := now.Sub(time.UnixMicro(write.LastWriteUnixMicro))
	if age < 0 || age >= unverifiedPasswordReassertionInterval {
		return nil
	}
	return write
}

func boundRoleApplyStatus(role *epco.DatabaseRole, receipt *epco.PasswordApplicationReceipt) error {
	if err := boundRoleStatus(role); err != nil {
		return err
	}
	budget := role.DeepCopy()
	budget.Status.PasswordApplied = receipt
	budget.Status.PasswordWrite = nil
	budget.Status.SecretResourceVersion = ""
	if receipt != nil {
		budget.Status.SecretResourceVersion = receipt.SourceResourceVersion
		// Reserve room before SQL for the largest possible committed-write record.
		budget.Status.PasswordWrite = &epco.RolePasswordWrite{
			RoleOID: math.MaxUint32, ServerStartUnixMicro: math.MaxInt64, LastWriteUnixMicro: math.MaxInt64,
		}
	}
	return boundRoleStatus(budget)
}

func boundRoleStatus(role *epco.DatabaseRole) error {
	serialized, err := json.Marshal(role)
	if err != nil {
		return err
	}
	if len(serialized) > DatabaseACLStatusByteLimit-databaseACLStatusHeadroom {
		return &postgres.Failure{Reason: postgres.StatusSizeExceeded, Message: "Role management history exceeds the resource status size budget"}
	}
	return nil
}
