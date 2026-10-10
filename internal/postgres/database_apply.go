package postgres

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	epco "github.com/vterdunov/external-postgres-control-operator/api/v1alpha1"
)

const builtinLocaleProvider = "builtin"

func (s *databaseSession) Apply(ctx context.Context, desired DatabaseDesired) (*DatabaseApplyResult, error) {
	bounded, cancel := context.WithTimeout(ctx, s.config.StatementTimeout)
	defer cancel()
	state, err := inspectDatabase(bounded, s.connection, desired.Name)
	if err != nil {
		return nil, databaseFailure(err)
	}
	if err = preflightDatabase(state, desired); err != nil {
		return nil, err
	}
	if desired.Spec.ManagementPolicy == epco.ManagementObserve {
		return s.observeDatabaseDesired(bounded, state, desired)
	}
	if desired.Spec.Ensure == epco.EnsureAbsent {
		if err = s.Drop(bounded, desired.Name); err != nil {
			return nil, err
		}
		return &DatabaseApplyResult{}, nil
	}
	if err = validateDatabaseIdentifier(string(desired.Spec.Owner), state.MaxIdentifierLength); err != nil {
		return nil, err
	}
	if len(desired.Spec.Schemas) != 0 && desired.Spec.AllowConnections != nil && !*desired.Spec.AllowConnections {
		return nil, &Failure{DependenciesNotReady, "Schema reconciliation requires the target database to allow connections"}
	}
	var recovery, readOnly bool
	if err = s.connection.QueryRow(bounded, `SELECT pg_is_in_recovery(),
		current_setting('transaction_read_only')::boolean`).Scan(&recovery, &readOnly); err != nil {
		return nil, databaseFailure(err)
	}
	if recovery || readOnly {
		return nil, &Failure{NotWritable, readOnlyMessage}
	}
	if !state.Exists {
		statement, buildErr := createDatabaseSQL(state, desired)
		if buildErr != nil {
			return nil, buildErr
		}
		// The supplied administrator retains CREATEDB. The requested owner may
		// deliberately be NOLOGIN and NOCREATEDB.
		if _, err = s.connection.Exec(bounded, statement); err != nil {
			return nil, databaseFailure(err)
		}
	} else if err = s.updateDatabase(bounded, state, desired); err != nil {
		return nil, databaseFailure(err)
	}
	state, err = inspectDatabase(bounded, s.connection, desired.Name)
	if err != nil {
		return nil, databaseFailure(err)
	}
	if !databaseMatches(state, desired.Spec) {
		return nil, &Failure{DependenciesNotReady, "The requested database properties could not be confirmed"}
	}
	result := &DatabaseApplyResult{BaseManaged: true}
	if err = s.applyDatabaseChildren(bounded, state, desired); err != nil {
		return result, err
	}
	// Child work uses separate target connections; confirm the database again
	// after those transactions have completed.
	state, err = inspectDatabase(bounded, s.connection, desired.Name)
	if err != nil {
		return result, databaseFailure(err)
	}
	if !databaseMatches(state, desired.Spec) {
		return result, &Failure{DependenciesNotReady, "The requested database properties changed during reconciliation"}
	}
	return result, nil
}

func preflightDatabase(state *DatabaseState, desired DatabaseDesired) error {
	if err := validateDatabaseIdentifier(desired.Name, state.MaxIdentifierLength); err != nil {
		return err
	}
	spec := desired.Spec
	if spec.Permissions != nil || len(spec.DefaultPrivileges) != 0 || len(spec.ConnectionSecrets) != 0 ||
		len(spec.FDWs) != 0 || len(spec.Servers) != 0 {
		return &Failure{UnsupportedFeature, "Database permissions, exports and foreign objects are not implemented"}
	}
	if err := validateExtensions(spec, state.MaxIdentifierLength); err != nil {
		return err
	}
	if spec.ManagementPolicy != epco.ManagementObserve && IsProtectedDatabase(desired.Name, state.AdminDatabase) {
		return &Failure{ProtectedDatabase, "The SQL database is protected from management"}
	}
	seen := make(map[string]bool, len(spec.Schemas))
	for _, schema := range spec.Schemas {
		name := string(schema.Name)
		if schema.Permissions != nil || len(schema.DefaultPrivileges) != 0 || len(schema.TablePermissions) != 0 {
			return &Failure{UnsupportedFeature, "Schema and table permissions and default privileges are not implemented"}
		}
		if err := validateDatabaseIdentifier(name, state.MaxIdentifierLength); err != nil {
			return err
		}
		if seen[name] {
			return &Failure{InvalidConfiguration, "Duplicate SQL schema declarations are invalid"}
		}
		seen[name] = true
		if spec.ManagementPolicy != epco.ManagementObserve && IsProtectedSchema(name) {
			return &Failure{ProtectedSchema, "The SQL schema is protected from management"}
		}
		if schema.Ensure != epco.EnsureAbsent && schema.Owner != "" {
			if err := validateDatabaseIdentifier(string(schema.Owner), state.MaxIdentifierLength); err != nil {
				return err
			}
		}
	}
	if spec.Tablespace != "" {
		if err := validateDatabaseIdentifier(string(spec.Tablespace), state.MaxIdentifierLength); err != nil {
			return err
		}
	}
	if spec.ConnectionLimit != nil && *spec.ConnectionLimit < -1 {
		return &Failure{InvalidConfiguration, "Database connection limit must be at least -1"}
	}
	return nil
}

func createDatabaseSQL(state *DatabaseState, desired DatabaseDesired) (string, error) {
	spec := desired.Spec
	if (spec.LocaleProvider != "" || spec.ICULocale != "" || spec.CollationVersion != "") && state.ServerVersion < 150000 ||
		spec.ICURules != "" && state.ServerVersion < 160000 ||
		(spec.LocaleProvider == builtinLocaleProvider || spec.BuiltinLocale != "") && state.ServerVersion < 170000 {
		return "", &Failure{UnsupportedFeature, "The PostgreSQL server version does not support the requested creation-only locale option"}
	}
	if spec.LocaleProvider != "" && spec.LocaleProvider != "libc" && spec.LocaleProvider != "icu" && spec.LocaleProvider != builtinLocaleProvider ||
		(spec.ICULocale != "" || spec.ICURules != "") && spec.LocaleProvider != "icu" ||
		spec.BuiltinLocale != "" && spec.LocaleProvider != builtinLocaleProvider {
		return "", &Failure{InvalidConfiguration, "Creation-only locale options do not match the requested provider"}
	}
	options := []string{"OWNER " + pgx.Identifier{string(spec.Owner)}.Sanitize()}
	for _, identifier := range []struct{ key, value string }{
		{"TEMPLATE", string(spec.Template)}, {"TABLESPACE", string(spec.Tablespace)},
	} {
		if identifier.value == "" {
			continue
		}
		if err := validateDatabaseIdentifier(identifier.value, state.MaxIdentifierLength); err != nil {
			return "", err
		}
		options = append(options, identifier.key+" "+pgx.Identifier{identifier.value}.Sanitize())
	}
	for _, option := range []struct{ key, value string }{
		{"ENCODING", spec.Encoding}, {"LOCALE", spec.Locale}, {"LOCALE_PROVIDER", spec.LocaleProvider},
		{"LC_COLLATE", spec.LocaleCollate}, {"LC_CTYPE", spec.LocaleCType}, {"ICU_LOCALE", spec.ICULocale},
		{"ICU_RULES", spec.ICURules}, {"BUILTIN_LOCALE", spec.BuiltinLocale}, {"COLLATION_VERSION", spec.CollationVersion},
	} {
		if option.value == "" {
			continue
		}
		if strings.ContainsRune(option.value, 0) {
			return "", &Failure{InvalidConfiguration, "A database creation option contains an invalid byte"}
		}
		options = append(options, option.key+" "+literal(option.value))
	}
	if spec.AllowConnections != nil {
		options = append(options, "ALLOW_CONNECTIONS "+strconv.FormatBool(*spec.AllowConnections))
	}
	if spec.IsTemplate != nil {
		options = append(options, "IS_TEMPLATE "+strconv.FormatBool(*spec.IsTemplate))
	}
	if spec.ConnectionLimit != nil {
		options = append(options, "CONNECTION LIMIT "+strconv.FormatInt(*spec.ConnectionLimit, 10))
	}
	return "CREATE DATABASE " + pgx.Identifier{desired.Name}.Sanitize() + " " + strings.Join(options, " "), nil
}

func (s *databaseSession) updateDatabase(ctx context.Context, state *DatabaseState, desired DatabaseDesired) error {
	spec := desired.Spec
	name := pgx.Identifier{desired.Name}.Sanitize()
	options := make([]string, 0, 3)
	if spec.AllowConnections != nil && *spec.AllowConnections != state.AllowConnections {
		options = append(options, "ALLOW_CONNECTIONS "+strconv.FormatBool(*spec.AllowConnections))
	}
	if spec.IsTemplate != nil && *spec.IsTemplate != state.IsTemplate {
		options = append(options, "IS_TEMPLATE "+strconv.FormatBool(*spec.IsTemplate))
	}
	if spec.ConnectionLimit != nil && *spec.ConnectionLimit != state.ConnectionLimit {
		options = append(options, "CONNECTION LIMIT "+strconv.FormatInt(*spec.ConnectionLimit, 10))
	}
	if string(spec.Owner) != state.Owner || len(options) != 0 {
		tx, err := s.connection.Begin(ctx)
		if err != nil {
			return err
		}
		defer rollbackDatabaseTransaction(tx)
		if string(spec.Owner) != state.Owner {
			// The supplied administrator must combine current-owner authority,
			// CREATEDB and access to the new owner. SET to a NOCREATEDB owner
			// cannot furnish that combination and must not elevate memberships.
			if _, err = tx.Exec(ctx, "ALTER DATABASE "+name+" OWNER TO "+pgx.Identifier{string(spec.Owner)}.Sanitize()); err != nil {
				return err
			}
		}
		if len(options) != 0 {
			if err = selectDatabaseRole(ctx, tx, string(spec.Owner), true); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "ALTER DATABASE "+name+" WITH "+strings.Join(options, " ")); err != nil {
				return err
			}
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	if spec.Tablespace != "" && string(spec.Tablespace) != state.Tablespace {
		return s.nontransactionalOwnerSQL(ctx, string(spec.Owner),
			"ALTER DATABASE "+name+" SET TABLESPACE "+pgx.Identifier{string(spec.Tablespace)}.Sanitize())
	}
	return nil
}

func rollbackDatabaseTransaction(tx pgx.Tx) {
	bounded, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(bounded)
}

func databaseMatches(state *DatabaseState, spec epco.DatabaseSpec) bool {
	return state.Exists && state.Owner == string(spec.Owner) &&
		(spec.IsTemplate == nil || state.IsTemplate == *spec.IsTemplate) &&
		(spec.AllowConnections == nil || state.AllowConnections == *spec.AllowConnections) &&
		(spec.ConnectionLimit == nil || state.ConnectionLimit == *spec.ConnectionLimit) &&
		(spec.Tablespace == "" || state.Tablespace == string(spec.Tablespace))
}
