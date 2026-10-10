package controller

import (
	"encoding/json/v2"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
	"github.com/vterdunov/external-postgres-control-operator/internal/postgres"
	corev1 "k8s.io/api/core/v1"
)

func passwordApplicationReceipt(role *epco.DatabaseRole, cluster *epco.PostgresCluster, source *corev1.Secret) *epco.PasswordApplicationReceipt {
	if source == nil {
		return nil
	}
	endpoint := roleEndpoint(role, cluster)
	return &epco.PasswordApplicationReceipt{SourceNamespace: source.Namespace, SourceName: source.Name,
		SourceUID: string(source.UID), SourceResourceVersion: source.ResourceVersion,
		RoleUID: string(role.UID), RoleGeneration: role.Generation, ClusterUID: string(cluster.UID), ClusterGeneration: cluster.Generation,
		Host: endpoint.Host, Port: endpoint.Port}
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
