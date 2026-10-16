package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	roleCapabilityMember = "MEMBER"
	roleCapabilitySet    = "SET"
	roleCapabilityUsage  = "USAGE"
)

type roleCapabilityKey struct {
	name    string
	purpose RoleCapabilityPurpose
}
type roleCapabilityEligibility struct {
	allowed bool
	err     error
}
type serviceRoleIdentity struct {
	login, owner       string
	loginOID, ownerOID uint32
	version            int
	inherit            bool
	allowed, super     bool
}
type serviceGrantor struct {
	name       string
	oid        uint32
	switchRole bool
}

func readServiceRoleIdentity(ctx context.Context, q roleQuerier, owner string) (serviceRoleIdentity, error) {
	return readServiceRoleCapability(ctx, q, owner, false)
}

func readServiceRoleCapability(ctx context.Context, q roleQuerier, owner string, inherit bool) (serviceRoleIdentity, error) {
	identity := serviceRoleIdentity{owner: owner, inherit: inherit}
	err := q.QueryRow(ctx, `SELECT session_user,a.oid,r.oid,r.rolsuper,pg_catalog.current_setting('server_version_num')::pg_catalog.int4
 FROM pg_catalog.pg_roles a JOIN pg_catalog.pg_roles r ON r.rolname OPERATOR(pg_catalog.=) $1 WHERE a.rolname OPERATOR(pg_catalog.=) session_user`, owner).
		Scan(&identity.login, &identity.loginOID, &identity.ownerOID, &identity.super, &identity.version)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity, &Failure{DependenciesNotReady, "A required SQL owner or creator role does not exist"}
	}
	if err != nil {
		return identity, err
	}
	capability := roleCapabilityMember
	if identity.version >= 160000 {
		capability = roleCapabilitySet
	}
	if inherit {
		capability = roleCapabilityUsage
	}
	err = q.QueryRow(ctx, `SELECT pg_catalog.pg_has_role($1::pg_catalog.oid,$2::pg_catalog.oid,$3)`, identity.loginOID, identity.ownerOID, capability).Scan(&identity.allowed)
	return identity, err
}

// ensureServiceRole acquires only a missing SET prerequisite. The retained grant
// belongs to the administrative connection, not to one Database's ACL history.
func (s *databaseSession) ensureServiceRole(ctx context.Context, owner string, purpose RoleCapabilityPurpose) error {
	return s.ensureServiceRoleCapability(ctx, owner, purpose, false)
}

func (s *databaseSession) ensureServiceInheritance(ctx context.Context, owner string) error {
	return s.ensureServiceRoleCapability(ctx, owner, RoleCapabilityProvision, true)
}

func (s *databaseSession) ensureServiceRoleCapability(ctx context.Context, owner string, purpose RoleCapabilityPurpose, inherit bool) error {
	bounded, cancel := context.WithTimeout(ctx, s.config.StatementTimeout)
	defer cancel()
	identity, err := readServiceRoleCapability(bounded, s.connection, owner, inherit)
	if err != nil {
		return err
	}
	if identity.allowed {
		return nil
	}
	if IsProtectedRole(owner, identity.login) || identity.super || inherit && identity.version < 160000 {
		return &Failure{InsufficientPrivileges, DatabaseOwnerSetRoleRequiredMessage}
	}
	key := roleCapabilityKey{owner, purpose}
	eligibility, known := s.eligibility[key]
	if !known {
		if s.options.ResolveRoleCapability != nil {
			eligibility.allowed, eligibility.err = s.options.ResolveRoleCapability(bounded, owner, purpose)
		}
		if s.eligibility == nil {
			s.eligibility = make(map[roleCapabilityKey]roleCapabilityEligibility)
		}
		s.eligibility[key] = eligibility
	}
	if eligibility.err != nil {
		return eligibility.err
	}
	if !eligibility.allowed {
		return &Failure{InsufficientPrivileges, DatabaseOwnerSetRoleRequiredMessage}
	}
	grantors, err := s.serviceGrantors(bounded, identity)
	if err != nil {
		return err
	}
	for _, grantor := range grantors {
		err = s.grantServiceRole(bounded, identity, grantor)
		if err == nil {
			return nil
		}
		if server, ok := errors.AsType[*pgconn.PgError](err); ok && (server.Code == "42501" || server.Code == "0LP01") {
			continue
		}
		return err
	}
	return &Failure{InsufficientPrivileges, DatabaseOwnerSetRoleRequiredMessage}
}

func (s *databaseSession) serviceGrantors(ctx context.Context, identity serviceRoleIdentity) ([]serviceGrantor, error) {
	candidates, err := reachableServiceGrantors(ctx, s.connection, identity)
	result := make([]serviceGrantor, 1, 1+len(candidates))
	result[0] = serviceGrantor{name: identity.login, oid: identity.loginOID}
	return append(result, candidates...), err
}

func reachableServiceGrantors(ctx context.Context, q roleQuerier, identity serviceRoleIdentity) ([]serviceGrantor, error) {
	var result []serviceGrantor
	capability := roleCapabilityMember
	if identity.version >= 160000 {
		capability = roleCapabilitySet
	}
	rows, err := q.Query(ctx, `SELECT r.rolname,r.oid,NOT pg_catalog.pg_has_role($1::pg_catalog.oid,r.oid,'USAGE')
 FROM pg_catalog.pg_auth_members m JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) m.member
 WHERE m.roleid OPERATOR(pg_catalog.=) $2 AND m.admin_option AND r.oid OPERATOR(pg_catalog.<>) $1
 AND (pg_catalog.pg_has_role($1::pg_catalog.oid,r.oid,'USAGE') OR pg_catalog.pg_has_role($1::pg_catalog.oid,r.oid,$3))
 GROUP BY r.rolname,r.oid ORDER BY NOT pg_catalog.pg_has_role($1::pg_catalog.oid,r.oid,'USAGE'),r.oid LIMIT $4`, identity.loginOID, identity.ownerOID, capability, maximumMembershipGrantors-1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var grantor serviceGrantor
		if err = rows.Scan(&grantor.name, &grantor.oid, &grantor.switchRole); err != nil {
			return nil, err
		}
		result = append(result, grantor)
	}
	return result, rows.Err()
}

func sameServiceIdentity(a, b serviceRoleIdentity) bool {
	return a.login == b.login && a.loginOID == b.loginOID && a.owner == b.owner && a.ownerOID == b.ownerOID && a.version == b.version && a.inherit == b.inherit
}

func (s *databaseSession) grantServiceRole(ctx context.Context, identity serviceRoleIdentity, grantor serviceGrantor) (err error) {
	tx, err := s.connection.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), membershipRollbackTimeout)
			defer cancel()
			if rollbackErr := tx.Rollback(cleanup); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				err = &Failure{ConnectionUnavailable, "Unable to confirm administrative membership rollback"}
			}
		}
	}()
	observed, err := readServiceRoleCapability(ctx, tx, identity.owner, identity.inherit)
	if err != nil {
		return err
	}
	if !sameServiceIdentity(identity, observed) {
		return &Failure{DependenciesNotReady, "SQL role identities changed while preparing administrative membership"}
	}
	if observed.allowed {
		return tx.Commit(ctx)
	}
	var grantorName string
	if err = tx.QueryRow(ctx, `SELECT rolname FROM pg_catalog.pg_roles WHERE oid OPERATOR(pg_catalog.=) $1`, grantor.oid).Scan(&grantorName); err != nil {
		return err
	}
	if grantorName != grantor.name {
		return &Failure{DependenciesNotReady, "SQL grantor identity changed while preparing administrative membership"}
	}
	if grantor.switchRole {
		if _, err = tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{grantor.name}.Sanitize()); err != nil {
			return err
		}
	}
	statement := "GRANT " + pgx.Identifier{identity.owner}.Sanitize() + " TO " + pgx.Identifier{identity.login}.Sanitize()
	var existing, admin, inherit, set bool
	if identity.version >= 160000 {
		err = tx.QueryRow(ctx, `SELECT admin_option,inherit_option,set_option FROM pg_catalog.pg_auth_members WHERE roleid OPERATOR(pg_catalog.=) $1 AND member OPERATOR(pg_catalog.=) $2 AND grantor OPERATOR(pg_catalog.=) $3`, identity.ownerOID, identity.loginOID, grantor.oid).Scan(&admin, &inherit, &set)
		existing = err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if identity.inherit {
			if existing {
				statement += " WITH INHERIT TRUE"
			} else {
				statement += " WITH ADMIN FALSE, INHERIT TRUE, SET TRUE"
			}
		} else if existing {
			statement += " WITH SET TRUE"
		} else {
			statement += " WITH INHERIT FALSE, SET TRUE"
		}
		statement += " GRANTED BY " + pgx.Identifier{grantor.name}.Sanitize()
	}
	if _, err = tx.Exec(ctx, statement); err != nil {
		return err
	}
	if err = confirmServiceMembership(ctx, tx, identity, grantor, existing, admin, inherit, set); err != nil {
		return err
	}

	confirmed, err := readServiceRoleCapability(ctx, tx, identity.owner, identity.inherit)
	if err != nil {
		return err
	}
	if !sameServiceIdentity(identity, confirmed) || !confirmed.allowed {
		return &Failure{DependenciesNotReady, "Administrative role capability could not be confirmed"}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	confirmed, err = readServiceRoleCapability(ctx, s.connection, identity.owner, identity.inherit)
	if err != nil {
		return err
	}
	if !sameServiceIdentity(identity, confirmed) || !confirmed.allowed {
		return &Failure{DependenciesNotReady, "Committed administrative role capability could not be confirmed"}
	}
	return nil
}

// Existing owner authority may be inherited without SET; do not widen it merely
// because a role participates in a declaration.
func (s *databaseSession) ensureOwnerExecution(ctx context.Context, owner string, purpose RoleCapabilityPurpose) error {
	var inherited bool
	if err := s.connection.QueryRow(ctx, `SELECT pg_catalog.pg_has_role(session_user,$1,'USAGE')`, owner).Scan(&inherited); err != nil {
		return err
	}
	if inherited {
		return nil
	}
	return s.ensureServiceRole(ctx, owner, purpose)
}

func confirmServiceMembership(ctx context.Context, tx pgx.Tx, identity serviceRoleIdentity, grantor serviceGrantor, existing, admin, inherit, set bool) error {
	if identity.version >= 160000 {
		var actualAdmin, actualInherit, actualSet bool
		if err := tx.QueryRow(ctx, `SELECT admin_option,inherit_option,set_option FROM pg_catalog.pg_auth_members WHERE roleid OPERATOR(pg_catalog.=) $1 AND member OPERATOR(pg_catalog.=) $2 AND grantor OPERATOR(pg_catalog.=) $3`, identity.ownerOID, identity.loginOID, grantor.oid).Scan(&actualAdmin, &actualInherit, &actualSet); err != nil {
			return err
		}
		expectedAdmin, expectedInherit, expectedSet := admin, inherit, true
		if !existing {
			expectedAdmin, expectedInherit = false, false
		}
		if identity.inherit {
			expectedInherit = true
			if existing {
				expectedSet = set
			}
		}
		if actualAdmin != expectedAdmin || actualInherit != expectedInherit || actualSet != expectedSet {
			return &Failure{DependenciesNotReady, "Administrative membership options could not be confirmed"}
		}
	} else {
		var actualGrantor uint32
		if err := tx.QueryRow(ctx, `SELECT grantor FROM pg_catalog.pg_auth_members WHERE roleid OPERATOR(pg_catalog.=) $1 AND member OPERATOR(pg_catalog.=) $2`, identity.ownerOID, identity.loginOID).Scan(&actualGrantor); err != nil {
			return err
		}
		if actualGrantor == 0 {
			return &Failure{DependenciesNotReady, "Administrative membership grantor could not be confirmed"}
		}
	}
	return nil
}
