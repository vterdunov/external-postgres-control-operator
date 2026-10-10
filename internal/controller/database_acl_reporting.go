package controller

import (
	"encoding/json/v2"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

// boundDatabasePermissionReports retains complete durable history and bounds
// only optional reports. Named and non-expanded declarations take precedence
// over individual objects discovered by broad table and sequence declarations.
func boundDatabasePermissionReports(database *epco.Database) error {
	encoded, err := json.Marshal(database)
	if err != nil || len(encoded) <= DatabaseACLStatusByteLimit-databaseACLStatusHeadroom {
		return err
	}
	outcomes := database.Status.Permissions
	omitted := database.Status.PermissionsOmitted + int32(len(outcomes))
	database.Status.Permissions = nil
	database.Status.PermissionsOmitted = omitted
	encoded, err = json.Marshal(database)
	if err != nil {
		return err
	}
	// The maximal omission counter was serialized above. Keeping outcomes only
	// reduces its encoded length, so this accounting is conservative and linear.
	remaining := DatabaseACLStatusByteLimit - databaseACLStatusHeadroom - len(encoded) - len(`,"permissions":[]`)
	retained := make([]epco.PermissionStatus, 0, len(outcomes))
	for _, broad := range []bool{false, true} {
		for _, outcome := range outcomes {
			if (outcome.Scope == "tables" || outcome.Scope == "sequences") != broad {
				continue
			}
			entry, marshalErr := json.Marshal(outcome)
			if marshalErr != nil {
				return marshalErr
			}
			size := len(entry)
			if len(retained) > 0 {
				size++ // JSON array separator.
			}
			if size > remaining {
				continue
			}
			remaining -= size
			retained = append(retained, outcome)
		}
	}
	database.Status.Permissions = retained
	database.Status.PermissionsOmitted = omitted - int32(len(retained))
	return nil
}
