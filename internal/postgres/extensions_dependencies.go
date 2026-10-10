package postgres

import (
	"context"
	"errors"
	"slices"

	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

// Validate dependency contradictions from the files needed by this operation.
// Installed no-ops and drops remain independent of control-file availability.
func (s *databaseSession) validateExtensionDependencies(ctx context.Context, name string, extensions []epco.ExtensionSpec) error {
	bounded, cancel := context.WithTimeout(ctx, s.config.StatementTimeout)
	defer cancel()
	connection, disposal, err := s.targetConnection(bounded, name)
	if err != nil {
		return databaseFailure(err)
	}
	defer closeDatabaseConnection(disposal)
	absent := make(map[string]bool, len(extensions))
	for _, extension := range extensions {
		absent[string(extension.Name)] = extension.Ensure == epco.EnsureAbsent
	}
	dependencies := make(map[string][]string, len(extensions))
	for _, extension := range extensions {
		if extension.Ensure == epco.EnsureAbsent {
			continue
		}
		state, inspectErr := inspectExtension(bounded, connection, string(extension.Name))
		if inspectErr != nil {
			return databaseFailure(inspectErr)
		}
		dependencies[state.Name] = nil
		if state.Exists && (extension.Version == "" || extension.Version == state.Version) {
			continue
		}
		metadata, metadataErr := availableExtensionVersion(bounded, connection, state.Name, extension.Version)
		if metadataErr != nil {
			classified := databaseFailure(metadataErr)
			if failure, ok := errors.AsType[*Failure](classified); ok {
				return &ExtensionFailure{Name: state.Name, Failure: failure}
			}
			return classified
		}
		for _, required := range metadata.Requires {
			if absent[required] {
				return &ExtensionFailure{Name: state.Name, Failure: &Failure{InvalidConfiguration, "A present extension requires an explicitly absent extension"}}
			}
			dependencies[state.Name] = append(dependencies[state.Name], required)
		}
	}
	// Repeatedly remove nodes without prerequisites in this desired graph.
	// An external missing prerequisite is pending work, not a declared cycle.
	for len(dependencies) > 0 {
		removed := false
		for extension, requires := range dependencies {
			if slices.ContainsFunc(requires, func(required string) bool {
				_, declared := dependencies[required]
				return declared
			}) {
				continue
			}
			delete(dependencies, extension)
			removed = true
		}
		if !removed {
			return &Failure{InvalidConfiguration, "The declared SQL extension prerequisites contain a cycle"}
		}
	}
	return nil
}
