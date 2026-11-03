//go:build integration

package integration

import epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"

// tablePermissionRules builds table rules from the suite's existing privilege fixtures.
func tablePermissionRules(tables []epco.SQLName, permissions epco.TablePermissions) []epco.TablePermissionRule {
	var rules []epco.TablePermissionRule
	for _, list := range []struct {
		privilege epco.TablePrivilege
		grantees  []epco.Permission
	}{
		{"select", permissions.Select}, {"insert", permissions.Insert},
		{"update", permissions.Update}, {"delete", permissions.Delete},
		{"truncate", permissions.Truncate}, {"references", permissions.References},
		{"trigger", permissions.Trigger}, {"maintain", permissions.Maintain},
	} {
		for _, grantee := range list.grantees {
			rules = append(rules, epco.TablePermissionRule{Role: grantee.Role, Tables: tables,
				Privileges: []epco.TablePrivilege{list.privilege}, Ensure: grantee.Ensure})
		}
	}
	return rules
}
