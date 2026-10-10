package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

type aclEngine struct {
	session       *databaseSession
	desired       ACLDesired
	targets       []epco.ManagedACLTarget
	index         int
	catalog       *aclCatalog
	target        *pgx.Conn
	targetFailure error
	trusted       bool
	outcomes      []epco.PermissionStatus
	blocked       map[string]bool
}
type aclCheckpointFailure struct{ error }

func (e *aclCheckpointFailure) Unwrap() error { return e.error }

func cloneACLTargets(targets []epco.ManagedACLTarget) []epco.ManagedACLTarget {
	result := make([]epco.ManagedACLTarget, len(targets))
	for i := range targets {
		targets[i].DeepCopyInto(&result[i])
	}
	return result
}
func aclTargetKey(t epco.ManagedACLTarget) string {
	return fmt.Sprintf("%q/%d/%q", t.Host, t.Port, t.Database)
}
func (s *databaseSession) newACLEngine(ctx context.Context, d ACLDesired, needTarget bool) (*aclEngine, func(), error) {
	state, err := s.Inspect(ctx, d.Name)
	if err != nil {
		return nil, nil, err
	}
	if !state.Exists {
		return nil, nil, &Failure{DatabaseNotFound, databaseNotFoundMessage}
	}
	e := &aclEngine{session: s, desired: d, targets: cloneACLTargets(d.Targets), index: -1}
	for i := range e.targets {
		if aclTargetKey(e.targets[i]) == aclTargetKey(d.Target) {
			e.index = i
			break
		}
	}
	if e.index < 0 {
		e.targets = append(e.targets, d.Target)
		e.index = len(e.targets) - 1
	}
	e.trusted = e.targets[e.index].BindingRevision == d.BindingRevision
	cleanup := func() {}
	if needTarget {
		if !state.AllowConnections {
			e.targetFailure = &Failure{DependenciesNotReady, "Target ACL reconciliation requires connection admission"}
		} else {
			var disposal *pgxProbeConnection
			e.target, disposal, err = s.targetConnection(ctx, d.Name)
			if err != nil {
				e.targetFailure = databaseFailure(err)
			} else {
				cleanup = func() { closeDatabaseConnection(disposal) }
			}
		}
	}
	var targetQueries roleQuerier
	if e.target != nil {
		targetQueries = s.aclQueries(e.target)
	}
	e.catalog, err = readACLCatalog(ctx, s.aclQueries(s.connection), targetQueries, d.Name, state.ServerVersion)
	if err != nil {
		cleanup()
		return nil, nil, databaseFailure(err)
	}
	return e, cleanup, nil
}
func aclNeedsTarget(spec epco.DatabaseSpec, targets []epco.ManagedACLTarget, target epco.ManagedACLTarget) bool {
	for _, d := range aclDeclarations(spec) {
		if d.binding.Class != aclClassDatabase {
			return true
		}
	}
	for _, t := range targets {
		if aclTargetKey(t) != aclTargetKey(target) {
			continue
		}
		for _, b := range t.Bindings {
			if b.Class != aclClassDatabase {
				return true
			}
		}
	}
	return false
}
func (e *aclEngine) connection(b epco.ACLBinding) *pgx.Conn {
	if b.Class == aclClassDatabase {
		return e.session.connection
	}
	return e.target
}
func (e *aclEngine) checkpoint(ctx context.Context) error {
	if e.desired.Observe {
		return nil
	}
	if e.desired.Checkpoint == nil {
		return &aclCheckpointFailure{&Failure{InvalidConfiguration, "ACL mutation requires a durable checkpoint callback"}}
	}
	e.targets[e.index].BindingRevision = e.desired.BindingRevision
	slices.SortFunc(e.targets[e.index].Bindings, func(a, b epco.ACLBinding) int { return strings.Compare(bindingKey(a), bindingKey(b)) })
	if err := e.desired.Checkpoint(ctx, cloneACLTargets(e.targets)); err != nil {
		return &aclCheckpointFailure{err}
	}
	return nil
}
func bindingKey(b epco.ACLBinding) string {
	return fmt.Sprintf("%s/%d/%d/%d/%d", b.Declaration, b.ObjectOID, b.SchemaOID, b.CreatorOID, b.GrantorOID)
}
func (e *aclEngine) result() *ACLResult {
	return &ACLResult{Targets: e.targets, Permissions: e.outcomes}
}
func (e *aclEngine) put(b epco.ACLBinding) {
	bindings := e.targets[e.index].Bindings
	i := slices.IndexFunc(bindings, func(old epco.ACLBinding) bool {
		return old.Declaration == b.Declaration && old.ObjectOID == b.ObjectOID && old.SchemaOID == b.SchemaOID && old.CreatorOID == b.CreatorOID && old.GrantorOID == b.GrantorOID && old.GranteeOID == b.GranteeOID
	})
	if i < 0 {
		bindings = append(bindings, b)
	} else {
		bindings[i] = b
	}
	e.targets[e.index].Bindings = bindings
}
func (e *aclEngine) remove(b epco.ACLBinding) {
	e.targets[e.index].Bindings = slices.DeleteFunc(e.targets[e.index].Bindings, func(old epco.ACLBinding) bool {
		return bindingKey(old) == bindingKey(b) && old.GranteeOID == b.GranteeOID
	})
}
func (e *aclEngine) outcome(ctx context.Context, b epco.ACLBinding, applied bool, message string) {
	e.outcomes = append(e.outcomes, epco.PermissionStatus{Scope: b.Scope, Creator: b.Creator, Class: b.Class, Schema: b.Schema, Table: b.Table, Grantee: b.Grantee, Privilege: b.Privilege, Applied: applied, Effective: e.effective(ctx, b), Message: message})
}

func (s *databaseSession) ReconcileACL(ctx context.Context, d ACLDesired) (*ACLResult, error) {
	state, err := s.Inspect(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	if err = preflightACL(state, d.Spec); err != nil {
		return nil, err
	}
	e, cleanup, err := s.newACLEngine(ctx, d, !d.ConnectionOnly && aclNeedsTarget(d.Spec, d.Targets, d.Target))
	if err != nil {
		return nil, err
	}
	defer cleanup()
	declarations := aclDeclarations(d.Spec)
	if d.ConnectionOnly {
		declarations = slices.DeleteFunc(declarations, func(d aclDeclaration) bool {
			return d.binding.Class != aclClassDatabase || d.binding.Privilege != aclConnect
		})
	}
	var first error
	if !d.Observe {
		if err = e.prepareIntent(ctx, declarations); err != nil {
			return e.result(), err
		}
	}
	if !d.Observe {
		if err = e.cleanupHistory(ctx, declarations); err != nil {
			if _, ok := errors.AsType[*aclCheckpointFailure](err); ok {
				return e.result(), err
			}
			first = err
		}
	}
	for _, declaration := range declarations {
		if err = ctx.Err(); err != nil {
			return e.result(), err
		}
		bindings, expandErr := e.expand(ctx, declaration)
		if expandErr != nil {
			e.outcome(ctx, declaration.binding, false, expandErr.Error())
			if first == nil {
				first = expandErr
			}
			continue
		}
		for _, b := range bindings {
			if !declaration.absent && e.blocked[concreteACLKey(b)] {
				e.outcome(ctx, b, false, "ACLBindingUnresolved: historical provenance must be resolved before managing this tuple")
				continue
			}
			err = e.reconcileBinding(ctx, b, declaration.absent)
			if err != nil {
				if _, ok := errors.AsType[*aclCheckpointFailure](err); ok {
					return e.result(), err
				}
				e.outcome(ctx, b, false, err.Error())
				if first == nil {
					first = err
				}
			} else {
				e.outcome(ctx, b, true, "The declared direct ACL layer was confirmed")
			}
		}
	}
	if !d.Observe {
		if err = e.checkpoint(ctx); err != nil {
			return e.result(), err
		}
	}
	return e.result(), first
}

func concreteACLKey(b epco.ACLBinding) string {
	return fmt.Sprintf("%s/%d/%d/%d/%d/%s", b.Class, b.ObjectOID, b.SchemaOID, b.CreatorOID, b.GranteeOID, b.Privilege)
}

func (e *aclEngine) expand(ctx context.Context, d aclDeclaration) ([]epco.ACLBinding, error) {
	b := d.binding
	if b.Class != aclClassDatabase && e.targetFailure != nil {
		return nil, e.targetFailure
	}
	if b.Scope == aclScopeTables || b.Scope == aclScopeSequences {
		var result []epco.ACLBinding
		for _, object := range e.catalog.relations {
			if object.schema != b.Schema || object.class != b.Class {
				continue
			}
			concrete := b
			concrete.Table = object.name
			concrete.ObjectOID = object.oid
			bound, exists, err := e.catalog.bind(concrete, true)
			if err != nil {
				return nil, err
			}
			if exists {
				bound.BindingRevision = e.desired.BindingRevision
				result = append(result, bound)
			}
		}
		slices.SortFunc(result, func(a, b epco.ACLBinding) int { return strings.Compare(a.Table, b.Table) })
		if _, ok := e.catalog.schemas[b.Schema]; !ok {
			return nil, &Failure{DependenciesNotReady, "A declared ACL schema does not exist"}
		}
		if b.Grantee != aclPublic {
			if _, ok := e.catalog.roles[b.Grantee]; !ok && !d.absent {
				return nil, &Failure{DependenciesNotReady, "A required ACL grantee role does not exist"}
			}
		}
		return result, nil
	}
	bound, exists, err := e.catalog.bind(b, false)
	if err != nil {
		return nil, err
	}
	if !exists {
		if d.absent {
			e.outcome(ctx, b, true, "The direct ACL target is absent")
			return nil, nil
		}
		message := "A required ACL role or schema does not exist"
		if b.Scope == aclClassTable {
			message = "TableNotFound: the named table or its grantee does not exist"
		}
		return nil, &Failure{DependenciesNotReady, message}
	}
	bound.BindingRevision = e.desired.BindingRevision
	return []epco.ACLBinding{bound}, nil
}

func (e *aclEngine) reconcileBinding(ctx context.Context, b epco.ACLBinding, absent bool) error {
	b.BindingRevision = e.desired.BindingRevision
	rows, err := readACLRows(ctx, e.queries(b), b)
	if err != nil {
		return databaseFailure(err)
	}
	matching := matchingACLRows(rows, b)
	if e.desired.Observe {
		if absent && len(matching) > 0 || !absent && len(matching) == 0 {
			return &Failure{DependenciesNotReady, "The observed direct ACL differs from its declaration"}
		}
		return nil
	}
	if absent {
		for _, row := range matching {
			selected := b
			selected.GrantorOID = row.grantor
			selected.Grantor = e.catalog.rolesByOID[row.grantor].Name
			selected.Before = e.catalog.before(matching)
			if err = e.revoke(ctx, selected); err != nil {
				return err
			}
		}
		return e.confirmAbsence(ctx, b)
	}
	// Retain the previously selected grantor rather than adopting another row.
	history := e.targets[e.index].Bindings
	prior := slices.IndexFunc(history, func(old epco.ACLBinding) bool {
		return old.Declaration == b.Declaration && old.ObjectOID == b.ObjectOID && old.SchemaOID == b.SchemaOID && old.CreatorOID == b.CreatorOID && old.GranteeOID == b.GranteeOID
	})
	if prior >= 0 {
		b = history[prior]
	}
	if b.GrantorOID == 0 && len(matching) > 0 {
		b.GrantorOID = matching[0].grantor
		b.Grantor = e.catalog.rolesByOID[b.GrantorOID].Name
	}
	if b.GrantorOID != 0 && aclRowPresent(rows, b) {
		previous := b
		b.Before = e.catalog.before(matching)
		b.Pending = ""
		b.GrantorCandidates = nil
		b.Generation = e.desired.Generation
		e.put(b)
		if prior >= 0 && reflect.DeepEqual(previous, b) {
			return nil
		}
		return e.checkpoint(ctx)
	}
	b.Pending = aclPendingGrant
	b.Generation = e.desired.Generation
	b.Before = e.catalog.before(matching)
	b.GrantorCandidates = e.candidates(b, rows)
	e.put(b)
	if err = e.checkpoint(ctx); err != nil {
		return err
	}
	after, err := e.mutate(ctx, b, true)
	if err != nil {
		return err
	}
	granted := matchingACLRows(after, b)
	if len(granted) == 0 {
		return &Failure{InsufficientPrivileges, "PostgreSQL did not install the requested direct privilege"}
	}
	selected := installedACLGrant(granted, b)
	if selected < 0 {
		return &Failure{InsufficientPrivileges, "PostgreSQL did not install a privilege with confirmed grantor provenance"}
	}
	old := b
	b.GrantorOID = granted[selected].grantor
	b.Grantor = e.catalog.rolesByOID[b.GrantorOID].Name
	b.Pending = ""
	b.GrantorCandidates = nil
	b.Before = e.catalog.before(granted)
	e.remove(old)
	e.put(b)
	return e.checkpoint(ctx)
}
func (e *aclEngine) revoke(ctx context.Context, b epco.ACLBinding) error {
	if b.Class == aclClassDatabase && b.Privilege == aclConnect {
		if err := e.preserveConnect(ctx, b); err != nil {
			return err
		}
	}
	b.Pending = aclPendingRevoke
	b.Generation = e.desired.Generation
	e.put(b)
	if err := e.checkpoint(ctx); err != nil {
		return err
	}
	after, err := e.mutate(ctx, b, false)
	if err != nil {
		return err
	}
	if aclRowPresent(after, b) {
		return &Failure{InsufficientPrivileges, "PostgreSQL did not remove the recorded direct privilege"}
	}
	if b.Class == aclClassDatabase && b.Privilege == aclConnect {
		if err = e.verifyConnect(ctx); err != nil {
			return err
		}
	}
	e.remove(b)
	return e.checkpoint(ctx)
}
