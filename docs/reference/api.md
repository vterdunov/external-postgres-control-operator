<!-- Generated from Go API sources by crd-ref-docs. DO NOT EDIT. -->
# API reference

Types, presence, default values, enum values and CEL rules come from the Go API source.

## postgresql.epco.io/v1alpha1

Package v1alpha1 contains API Schema definitions for the postgresql v1alpha1 API group.



### ACLBinding

ACLBinding is one concrete object/privilege/grantee/grantor responsibility.
Pending operations are possibly issued work, never commands to replay blindly.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `bindingRevision` | string | optional |  |  | BindingRevision records the target epoch in which catalog identities were resolved. |
| `declaration` | string | required |  |  |  |
| `scope` | string | required |  |  |  |
| `class` | string | required |  |  |  |
| `privilege` | string | required |  |  |  |
| `grantee` | string | required |  |  |  |
| `schema` | string | optional |  |  |  |
| `table` | string | optional |  |  |  |
| `creator` | string | optional |  |  |  |
| `grantor` | string | optional |  |  |  |
| `owner` | string | optional |  |  |  |
| `databaseOID` | integer | optional |  |  |  |
| `schemaOID` | integer | optional |  |  |  |
| `objectOID` | integer | optional |  |  |  |
| `creatorOID` | integer | optional |  |  |  |
| `granteeOID` | integer | optional |  |  |  |
| `grantorOID` | integer | optional |  |  |  |
| `ownerOID` | integer | optional |  |  |  |
| `pending` | string | optional |  |  |  |
| `generation` | integer | optional |  |  |  |
| `administrative` | boolean | optional |  |  |  |
| `grantorCandidates` | [ACLRoleIdentity](#aclroleidentity) array | optional |  |  |  |
| `before` | [ACLGrantIdentity](#aclgrantidentity) array | optional |  |  | Before contains the matching direct rows before a possibly issued operation. |
| `previousOwner` | [ACLRoleIdentity](#aclroleidentity) | optional |  |  |  |
| `desiredOwner` | [ACLRoleIdentity](#aclroleidentity) | optional |  |  |  |


### ACLGrantIdentity

ACLGrantIdentity preserves the narrow pre-operation grantor provenance.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `grantor` | [ACLRoleIdentity](#aclroleidentity) | required |  |  |  |
| `grantee` | [ACLRoleIdentity](#aclroleidentity) | optional |  |  | Grantee is recorded when owner transfer can also rewrite the grantee. |
| `grantable` | boolean | optional |  |  |  |


### ACLRoleIdentity

ACLRoleIdentity is the catalog identity and last confirmed name of a SQL role.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | string | required |  |  |  |
| `oid` | integer | optional |  |  |  |


### ClusterAccess

ClusterAccess authorizes references by namespace labels.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `namespaceSelector` | [LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#labelselector-v1-meta) | optional |  |  | NamespaceSelector selects both local and remote namespaces. Nil selects<br />none; an explicitly empty selector selects all namespaces. |


### ClusterReference

ClusterReference identifies a ExternalPostgres connection descriptor.
CEL: `self == oldSelf` - cluster reference is immutable

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | string | required |  | MaxLength: 253<br />MinLength: 1<br /> | Name is the ExternalPostgres resource name. |
| `namespace` | string | optional |  | MaxLength: 63<br />MinLength: 1<br /> | Namespace defaults to the referring resource's namespace. |


### ConnectionSecret

ConnectionSecret declares one derived application basic-auth Secret.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | string | required |  | MaxLength: 253<br />MinLength: 1<br /> | Name is the output Secret name in the Database namespace. |
| `roleRef` | [LocalReference](#localreference) | required |  |  | RoleRef identifies a local DatabaseRole with a password source. |
| `sslRootCertPath` | string | optional |  | MinLength: 1<br /> | SSLRootCertPath is the application's mounted CA path. When omitted and a<br />cluster CA is configured, the reconciler uses /etc/postgresql/ca.crt. |


### ConnectionSecretState

ConnectionSecretState preserves credential-free output cleanup responsibility.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | string | required |  |  | Name is the namespace-local output name whose publication was attempted. |
| `uid` | string | optional |  |  | UID identifies the confirmed output incarnation. Omission preserves an uncertain CREATE. |
| `adoptionFromDatabaseUID` | string | optional |  |  | AdoptionFromDatabaseUID records a validated retained owner while adoption is pending.<br />The exact Secret incarnation is claimed before its ownership annotation changes. |


### Database

Database is the API for a PostgreSQL database, SQL objects, and permissions.
CEL: `(has(self.spec.name) ? self.spec.name : self.metadata.name) == (has(oldSelf.spec.name) ? oldSelf.spec.name : oldSelf.metadata.name)` - effective SQL name is immutable

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `metadata` | [ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#objectmeta-v1-meta) | required |  |  |  |
| `spec` | [DatabaseSpec](#databasespec) | required |  | CEL: `self.managementPolicy != 'manage' \|\| has(self.owner)` - owner is required in manage mode<br />CEL: `self.managementPolicy != 'observe' \|\| (self.ensure == 'present' && self.databaseReclaimPolicy == 'retain')` - observe requires ensure=present and retain reclaim policy<br />CEL: `!has(self.icuLocale) \|\| (has(self.localeProvider) && self.localeProvider == 'icu')` - icuLocale requires localeProvider=icu<br />CEL: `!has(self.icuRules) \|\| (has(self.localeProvider) && self.localeProvider == 'icu')` - icuRules requires localeProvider=icu<br />CEL: `!has(self.builtinLocale) \|\| (has(self.localeProvider) && self.localeProvider == 'builtin')` - builtinLocale requires localeProvider=builtin<br />CEL: `has(self.template) == has(oldSelf.template) && (!has(self.template) \|\| self.template == oldSelf.template)` - template is immutable<br />CEL: `has(self.encoding) == has(oldSelf.encoding) && (!has(self.encoding) \|\| self.encoding == oldSelf.encoding)` - encoding is immutable<br />CEL: `has(self.locale) == has(oldSelf.locale) && (!has(self.locale) \|\| self.locale == oldSelf.locale)` - locale is immutable<br />CEL: `has(self.localeProvider) == has(oldSelf.localeProvider) && (!has(self.localeProvider) \|\| self.localeProvider == oldSelf.localeProvider)` - localeProvider is immutable<br />CEL: `has(self.localeCollate) == has(oldSelf.localeCollate) && (!has(self.localeCollate) \|\| self.localeCollate == oldSelf.localeCollate)` - localeCollate is immutable<br />CEL: `has(self.localeCType) == has(oldSelf.localeCType) && (!has(self.localeCType) \|\| self.localeCType == oldSelf.localeCType)` - localeCType is immutable<br />CEL: `has(self.icuLocale) == has(oldSelf.icuLocale) && (!has(self.icuLocale) \|\| self.icuLocale == oldSelf.icuLocale)` - icuLocale is immutable<br />CEL: `has(self.icuRules) == has(oldSelf.icuRules) && (!has(self.icuRules) \|\| self.icuRules == oldSelf.icuRules)` - icuRules is immutable<br />CEL: `has(self.builtinLocale) == has(oldSelf.builtinLocale) && (!has(self.builtinLocale) \|\| self.builtinLocale == oldSelf.builtinLocale)` - builtinLocale is immutable<br />CEL: `has(self.collationVersion) == has(oldSelf.collationVersion) && (!has(self.collationVersion) \|\| self.collationVersion == oldSelf.collationVersion)` - collationVersion is immutable<br /> |  |
| `status` | [DatabaseStatus](#databasestatus) | optional |  |  |  |


### DatabaseObjectSpec

DatabaseObjectSpec contains the lifecycle fields shared by SQL objects.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL object name. |
| `ensure` | [EnsureOption](#ensureoption) | optional | present | Enum: [present absent]<br /> | Ensure requests presence or DROP with RESTRICT; omission means present. |


### DatabasePermissions

DatabasePermissions declares database privileges.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `connect` | [Permission](#permission) array | optional |  |  | Connect declares CONNECT grants. |
| `create` | [Permission](#permission) array | optional |  |  | Create declares CREATE grants. |
| `temporary` | [Permission](#permission) array | optional |  |  | Temporary declares TEMPORARY grants. |


### DatabaseRole

DatabaseRole is the API for a PostgreSQL role on an external server.
CEL: `(has(self.spec.name) ? self.spec.name : self.metadata.name) == (has(oldSelf.spec.name) ? oldSelf.spec.name : oldSelf.metadata.name)` - effective SQL name is immutable

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `metadata` | [ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#objectmeta-v1-meta) | required |  |  |  |
| `spec` | [DatabaseRoleSpec](#databaserolespec) | required |  | CEL: `!(has(self.passwordSecret) && has(self.passwordGeneration))` - passwordSecret and passwordGeneration are mutually exclusive<br />CEL: `!self.disablePassword \|\| (!has(self.passwordSecret) && !has(self.passwordGeneration))` - disablePassword cannot be combined with a password source<br />CEL: `self.managementPolicy != 'observe' \|\| self.databaseRoleReclaimPolicy == 'retain'` - observe requires retain reclaim policy<br />CEL: `!has(oldSelf.passwordGeneration) \|\| (has(self.passwordGeneration) && self.passwordGeneration.secretName == oldSelf.passwordGeneration.secretName)` - generated password source is immutable once configured<br /> |  |
| `status` | [DatabaseRoleStatus](#databaserolestatus) | optional |  |  |  |


### DatabaseRoleSpec

DatabaseRoleSpec defines PostgreSQL role attributes and password sources.
CEL: `!(has(self.passwordSecret) && has(self.passwordGeneration))` - passwordSecret and passwordGeneration are mutually exclusive
CEL: `!self.disablePassword \|\| (!has(self.passwordSecret) && !has(self.passwordGeneration))` - disablePassword cannot be combined with a password source
CEL: `self.managementPolicy != 'observe' \|\| self.databaseRoleReclaimPolicy == 'retain'` - observe requires retain reclaim policy
CEL: `!has(oldSelf.passwordGeneration) \|\| (has(self.passwordGeneration) && self.passwordGeneration.secretName == oldSelf.passwordGeneration.secretName)` - generated password source is immutable once configured

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `cluster` | [ClusterReference](#clusterreference) | required |  | CEL: `self == oldSelf` - cluster reference is immutable<br /> | Cluster identifies the PostgreSQL connection descriptor. |
| `name` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL role name; omission resolves to metadata.name. |
| `comment` | string | optional |  |  | Comment is the SQL role description. |
| `login` | boolean | optional | false |  | Login allows the role to log in. It is never enabled implicitly. |
| `inherit` | boolean | optional | true |  | Inherit controls the role attribute, not existing PG16+ membership options. |
| `superuser` | boolean | optional | false |  | Superuser grants PostgreSQL superuser status when the server permits it. |
| `createdb` | boolean | optional | false |  | CreateDB allows database creation. |
| `createrole` | boolean | optional | false |  | CreateRole allows role administration subject to server authorization. |
| `replication` | boolean | optional | false |  | Replication allows replication connections. |
| `bypassrls` | boolean | optional | false |  | BypassRLS bypasses row-level security. |
| `connectionLimit` | integer | optional | -1 | Minimum: -1<br /> | ConnectionLimit is the role's connection limit; -1 means unlimited. |
| `validUntil` | [Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#time-v1-meta) | optional |  |  | ValidUntil is the password expiration timestamp. Omission means no expiration. |
| `disablePassword` | boolean | optional | false |  | DisablePassword sets PASSWORD NULL. |
| `passwordSecret` | [LocalReference](#localreference) | optional |  |  | PasswordSecret selects a supplied local basic-auth Secret. |
| `passwordGeneration` | [PasswordGeneration](#passwordgeneration) | optional |  |  | PasswordGeneration requests a persisted, reusable generated password. |
| `inRoles` | [SQLName](#sqlname) array | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | InRoles contains SQL parent role names, using CNPG membership semantics. |
| `parameters` | object (keys:string, values:string) | optional |  |  | Parameters contains the ALTER ROLE SET parameters managed by this resource. |
| `membershipPolicy` | [MembershipPolicy](#membershippolicy) | optional | authoritative | Enum: [authoritative managed]<br /> | MembershipPolicy controls authoritative or operator-managed memberships. |
| `managementPolicy` | [ManagementPolicy](#managementpolicy) | optional | manage | Enum: [manage observe]<br /> | ManagementPolicy chooses reconciliation or observation. |
| `databaseRoleReclaimPolicy` | [ReclaimPolicy](#reclaimpolicy) | optional | retain | Enum: [retain delete]<br /> | ReclaimPolicy controls role deletion when this CR is deleted. |


### DatabaseRoleStatus

DatabaseRoleStatus reports role reconciliation and the applied password version.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `observedGeneration` | integer | optional |  |  | ObservedGeneration is the latest generation inspected by the reconciler. |
| `applied` | boolean | optional |  |  | Applied reports whether reconciliation succeeded. |
| `message` | string | optional |  |  | Message describes the most recent reconciliation result. |
| `effectiveName` | string | optional |  |  | EffectiveName is the resolved SQL name, including metadata.name fallback. |
| `appliedGeneration` | integer | optional |  |  | AppliedGeneration is the latest successfully reconciled generation. |
| `lastSuccessfulReconcileTime` | [Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#time-v1-meta) | optional |  |  | LastSuccessfulReconcileTime records the most recent successful reconciliation. |
| `conditions` | [Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#condition-v1-meta) array | optional |  |  | Conditions report readiness and failures, keyed by condition type. |
| `hasManagedSQL` | boolean | optional |  |  | HasManagedSQL records successful SQL management and remains true during observation or temporary failure. |
| `generatedPasswordInitialized` | boolean | optional |  |  | GeneratedPasswordInitialized records that a persisted generated source was accepted. Source loss must not regenerate it. |
| `managedEndpoints` | [RoleEndpointState](#roleendpointstate) array | optional |  |  | ManagedEndpoints preserves cleanup intent for each configured endpoint within this resource UID. |
| `secretResourceVersion` | string | optional |  |  | SecretResourceVersion identifies the password source version applied to SQL. |
| `passwordApplied` | [PasswordApplicationReceipt](#passwordapplicationreceipt) | optional |  |  | PasswordApplied records the exact credential-free input snapshot accepted by SQL. |


### DatabaseSpec

DatabaseSpec defines a PostgreSQL database and its managed SQL objects and ACLs.
CEL: `self.managementPolicy != 'manage' \|\| has(self.owner)` - owner is required in manage mode
CEL: `self.managementPolicy != 'observe' \|\| (self.ensure == 'present' && self.databaseReclaimPolicy == 'retain')` - observe requires ensure=present and retain reclaim policy
CEL: `!has(self.icuLocale) \|\| (has(self.localeProvider) && self.localeProvider == 'icu')` - icuLocale requires localeProvider=icu
CEL: `!has(self.icuRules) \|\| (has(self.localeProvider) && self.localeProvider == 'icu')` - icuRules requires localeProvider=icu
CEL: `!has(self.builtinLocale) \|\| (has(self.localeProvider) && self.localeProvider == 'builtin')` - builtinLocale requires localeProvider=builtin
CEL: `has(self.template) == has(oldSelf.template) && (!has(self.template) \|\| self.template == oldSelf.template)` - template is immutable
CEL: `has(self.encoding) == has(oldSelf.encoding) && (!has(self.encoding) \|\| self.encoding == oldSelf.encoding)` - encoding is immutable
CEL: `has(self.locale) == has(oldSelf.locale) && (!has(self.locale) \|\| self.locale == oldSelf.locale)` - locale is immutable
CEL: `has(self.localeProvider) == has(oldSelf.localeProvider) && (!has(self.localeProvider) \|\| self.localeProvider == oldSelf.localeProvider)` - localeProvider is immutable
CEL: `has(self.localeCollate) == has(oldSelf.localeCollate) && (!has(self.localeCollate) \|\| self.localeCollate == oldSelf.localeCollate)` - localeCollate is immutable
CEL: `has(self.localeCType) == has(oldSelf.localeCType) && (!has(self.localeCType) \|\| self.localeCType == oldSelf.localeCType)` - localeCType is immutable
CEL: `has(self.icuLocale) == has(oldSelf.icuLocale) && (!has(self.icuLocale) \|\| self.icuLocale == oldSelf.icuLocale)` - icuLocale is immutable
CEL: `has(self.icuRules) == has(oldSelf.icuRules) && (!has(self.icuRules) \|\| self.icuRules == oldSelf.icuRules)` - icuRules is immutable
CEL: `has(self.builtinLocale) == has(oldSelf.builtinLocale) && (!has(self.builtinLocale) \|\| self.builtinLocale == oldSelf.builtinLocale)` - builtinLocale is immutable
CEL: `has(self.collationVersion) == has(oldSelf.collationVersion) && (!has(self.collationVersion) \|\| self.collationVersion == oldSelf.collationVersion)` - collationVersion is immutable

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `cluster` | [ClusterReference](#clusterreference) | required |  | CEL: `self == oldSelf` - cluster reference is immutable<br /> | Cluster identifies the connection descriptor. |
| `name` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL database name; omission resolves to metadata.name. |
| `owner` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Owner is the SQL owner role, required in manage mode. |
| `ensure` | [EnsureOption](#ensureoption) | optional | present | Enum: [present absent]<br /> | Ensure requests database presence or explicit removal regardless of reclaim policy. |
| `template` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Template is a creation-only PostgreSQL database option. |
| `encoding` | string | optional |  | MinLength: 1<br /> | Encoding is a creation-only PostgreSQL database option. |
| `locale` | string | optional |  | MinLength: 1<br /> | Locale is a creation-only PostgreSQL database option. |
| `localeProvider` | string | optional |  | Enum: [libc icu builtin]<br />MinLength: 1<br /> | LocaleProvider is a creation-only PostgreSQL database option. |
| `localeCollate` | string | optional |  | MinLength: 1<br /> | LocaleCollate is a creation-only PostgreSQL database option. |
| `localeCType` | string | optional |  | MinLength: 1<br /> | LocaleCType is a creation-only PostgreSQL database option. |
| `icuLocale` | string | optional |  | MinLength: 1<br /> | ICULocale is a creation-only PostgreSQL database option. |
| `icuRules` | string | optional |  | MinLength: 1<br /> | ICURules is a creation-only PostgreSQL database option. |
| `builtinLocale` | string | optional |  | MinLength: 1<br /> | BuiltinLocale is a creation-only PostgreSQL database option. |
| `collationVersion` | string | optional |  | MinLength: 1<br /> | CollationVersion is a creation-only PostgreSQL database option. |
| `isTemplate` | boolean | optional |  |  | IsTemplate changes template status only when explicitly specified. |
| `allowConnections` | boolean | optional |  |  | AllowConnections changes connection admission only when explicitly specified. |
| `connectionLimit` | integer | optional |  | Minimum: -1<br /> | ConnectionLimit changes the database limit only when explicitly specified. |
| `tablespace` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Tablespace is the SQL tablespace used by the database. |
| `permissions` | [DatabasePermissions](#databasepermissions) | optional |  |  | Permissions declares database CONNECT, CREATE, and TEMPORARY grants. |
| `schemas` | [SchemaSpec](#schemaspec) array | optional |  |  | Schemas contains schemas and their permissions. |
| `extensions` | [ExtensionSpec](#extensionspec) array | optional |  |  | Extensions contains managed extensions. |
| `fdws` | [FDWSpec](#fdwspec) array | optional |  |  | FDWs contains foreign data wrappers in CNPG format. |
| `servers` | [ServerSpec](#serverspec) array | optional |  |  | Servers contains foreign servers in CNPG format. |
| `defaultPrivileges` | [DefaultPrivileges](#defaultprivileges) array | optional |  |  | DefaultPrivileges contains global defaults for actual object creator roles. |
| `connectionSecrets` | [ConnectionSecret](#connectionsecret) array | optional |  |  | ConnectionSecrets declares application connection exports in this namespace. |
| `managementPolicy` | [ManagementPolicy](#managementpolicy) | optional | manage | Enum: [manage observe]<br /> | ManagementPolicy selects reconciliation or read-only observation. |
| `databaseReclaimPolicy` | [ReclaimPolicy](#reclaimpolicy) | optional | retain | Enum: [retain delete]<br /> | ReclaimPolicy controls SQL cleanup when this CR is deleted. |


### DatabaseStatus

DatabaseStatus reports database reconciliation and per-grant results.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `hasManagedSQL` | boolean | optional |  |  | HasManagedSQL records confirmed database and schema lifecycle management.<br />It remains true through observation and subsequent failures. |
| `observedGeneration` | integer | optional |  |  | ObservedGeneration is the latest generation inspected by the reconciler. |
| `applied` | boolean | optional |  |  | Applied reports whether reconciliation succeeded. |
| `message` | string | optional |  |  | Message describes the most recent reconciliation result. |
| `effectiveName` | string | optional |  |  | EffectiveName is the resolved SQL name, including metadata.name fallback. |
| `appliedGeneration` | integer | optional |  |  | AppliedGeneration is the latest successfully reconciled generation. |
| `lastSuccessfulReconcileTime` | [Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#time-v1-meta) | optional |  |  | LastSuccessfulReconcileTime records the most recent successful reconciliation. |
| `conditions` | [Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#condition-v1-meta) array | optional |  |  | Conditions report readiness and failures, keyed by condition type. |
| `permissions` | [PermissionStatus](#permissionstatus) array | optional |  |  | Permissions identifies observed outcomes for schema/table/grantee tuples. |
| `permissionsOmitted` | integer | optional |  | Minimum: 0<br /> | PermissionsOmitted counts optional outcome reports omitted to preserve the<br />resource size budget. Durable ACL cleanup history is never omitted. |
| `managedACLTargets` | [ManagedACLTarget](#managedacltarget) array | optional |  |  | ManagedACLTargets retains cleanup responsibility within this resource UID. |
| `activeACLTarget` | string | optional |  |  | ActiveACLTarget identifies the last selected configured target, without credentials. |
| `connectionSecretHistory` | [ConnectionSecretState](#connectionsecretstate) array | optional |  |  | ConnectionSecretHistory records attempted publication before creating outputs.<br />Missing recorded outputs require SQL confirmation before recreation. |


### DefaultPrivileges

DefaultPrivileges declares future-object privileges for the actual SQL creator.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `forRole` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | ForRole is the SQL role creating the future objects. |
| `tables` | [TablePermissions](#tablepermissions) | optional |  |  | Tables declares defaults for future tables. |
| `sequences` | [SequencePermissions](#sequencepermissions) | optional |  |  | Sequences declares defaults for future sequences. |
| `functions` | [FunctionPermissions](#functionpermissions) | optional |  |  | Functions declares defaults for future functions. |
| `types` | [TypePermissions](#typepermissions) | optional |  |  | Types declares defaults for future types. |


### EnsureOption

EnsureOption declares the desired presence of a SQL object or privilege.
Validation: Enum: [present absent]


### ExtensionSpec

ExtensionSpec declares a PostgreSQL extension.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL object name. |
| `ensure` | [EnsureOption](#ensureoption) | optional | present | Enum: [present absent]<br /> | Ensure requests presence or DROP with RESTRICT; omission means present. |
| `version` | string | optional |  |  | Version selects the extension version; omission uses the server default. |
| `schema` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Schema selects the extension object schema when relocation is supported. |


### ExternalPostgres

ExternalPostgres is the API for an external PostgreSQL connection descriptor.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `metadata` | [ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#objectmeta-v1-meta) | required |  |  |  |
| `spec` | [ExternalPostgresSpec](#externalpostgresspec) | required |  |  |  |
| `status` | [ExternalPostgresStatus](#externalpostgresstatus) | optional |  |  |  |


### ExternalPostgresSpec

ExternalPostgresSpec defines a connection to an external writable PostgreSQL server.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `connection` | [PostgresConnection](#postgresconnection) | required |  |  | Connection identifies the administrative endpoint and database. |
| `credentialsSecret` | [LocalReference](#localreference) | required |  |  | CredentialsSecret supplies a local basic-auth Secret with username/password. |
| `tls` | [PostgresTLS](#postgrestls) | optional |  |  | TLS configures transport security. Omission uses verify-full and system trust. |
| `access` | [ClusterAccess](#clusteraccess) | optional |  |  | Access optionally selects authorized Namespace objects. Without it, only<br />references from the ExternalPostgres namespace are authorized. |
| `reconcileInterval` | [PositiveDuration](#positiveduration) | optional | 5m | MaxLength: 64<br />Pattern: `^([0-9]+([.][0-9]+)?(h\|m\|s\|ms\|us\|ns))+$`<br />CEL: `duration(self) > duration('0s')` - duration must be positive<br /> | ReconcileInterval is the interval between periodic reconciliations. |
| `connectTimeout` | [PositiveDuration](#positiveduration) | optional | 10s | MaxLength: 64<br />Pattern: `^([0-9]+([.][0-9]+)?(h\|m\|s\|ms\|us\|ns))+$`<br />CEL: `duration(self) > duration('0s')` - duration must be positive<br /> | ConnectTimeout limits connection establishment. |
| `statementTimeout` | [PositiveDuration](#positiveduration) | optional | 30s | MaxLength: 64<br />Pattern: `^([0-9]+([.][0-9]+)?(h\|m\|s\|ms\|us\|ns))+$`<br />CEL: `duration(self) > duration('0s')` - duration must be positive<br /> | StatementTimeout limits each SQL statement. |


### ExternalPostgresStatus

ExternalPostgresStatus defines the observed connection state.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `observedGeneration` | integer | optional |  |  | ObservedGeneration is the latest generation inspected by the reconciler. |
| `applied` | boolean | optional |  |  | Applied reports whether reconciliation succeeded. |
| `message` | string | optional |  |  | Message describes the most recent reconciliation result. |
| `effectiveName` | string | optional |  |  | EffectiveName is the resolved SQL name, including metadata.name fallback. |
| `appliedGeneration` | integer | optional |  |  | AppliedGeneration is the latest successfully reconciled generation. |
| `lastSuccessfulReconcileTime` | [Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#time-v1-meta) | optional |  |  | LastSuccessfulReconcileTime records the most recent successful reconciliation. |
| `conditions` | [Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#condition-v1-meta) array | optional |  |  | Conditions report readiness and failures, keyed by condition type. |


### FDWSpec

FDWSpec declares a foreign data wrapper in CNPG format.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL object name. |
| `ensure` | [EnsureOption](#ensureoption) | optional | present | Enum: [present absent]<br /> | Ensure requests presence or DROP with RESTRICT; omission means present. |
| `handler` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Handler is the SQL handler function name. |
| `validator` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Validator is the SQL validator function name. |
| `owner` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Owner is the SQL owner role. |
| `options` | [OptionSpec](#optionspec) array | optional |  |  | Options declares managed FDW options. |
| `usage` | [UsageSpec](#usagespec) array | optional |  |  | Usage uses CNPG's grant/revoke format rather than permission ensure entries. |


### FunctionPermissions

FunctionPermissions declares function privileges.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `execute` | [Permission](#permission) array | optional |  |  | Execute declares EXECUTE grants. |


### LocalReference

LocalReference names a resource in the referring resource's namespace.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | string | required |  | MaxLength: 253<br />MinLength: 1<br /> | Name is the Kubernetes resource name. |


### ManagedACLTarget

ManagedACLTarget records concrete ACL responsibility on one configured target.
Inactive targets are retained but never contacted for cleanup.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `host` | string | required |  |  |  |
| `port` | integer | required |  |  |  |
| `database` | string | required |  |  |  |
| `bindingRevision` | string | optional |  |  | BindingRevision invalidates catalog identity after routing/descriptor changes. |
| `bindings` | [ACLBinding](#aclbinding) array | optional |  |  |  |


### ManagementPolicy

ManagementPolicy selects reconciliation or read-only observation.
Validation: Enum: [manage observe]


### MembershipPolicy

MembershipPolicy controls reconciliation of memberships not in inRoles.
Validation: Enum: [authoritative managed]


### NamedTablePermissions

NamedTablePermissions declares privileges on one existing table.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the existing SQL table name, without schema qualification. |
| `permissions` | [TablePermissions](#tablepermissions) | required |  |  | Permissions declares grants without creating or dropping the table. |


### OptionSpec

OptionSpec declares a foreign wrapper/server option.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL option name. |
| `value` | string | required |  |  | Value is the option value, including an explicitly empty value. |
| `ensure` | [EnsureOption](#ensureoption) | optional | present | Enum: [present absent]<br /> | Ensure selects presence or removal. |


### PasswordApplicationReceipt

PasswordApplicationReceipt identifies configured inputs, never credential bytes.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `sourceNamespace` | string | required |  |  |  |
| `sourceName` | string | required |  |  |  |
| `sourceUID` | string | required |  |  |  |
| `sourceResourceVersion` | string | required |  |  |  |
| `roleUID` | string | required |  |  |  |
| `roleGeneration` | integer | required |  |  |  |
| `clusterUID` | string | required |  |  |  |
| `clusterGeneration` | integer | required |  |  |  |
| `host` | string | required |  |  |  |
| `port` | integer | required |  |  |  |


### PasswordGeneration

PasswordGeneration requests a generated basic-auth Secret.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `secretName` | string | required |  | MaxLength: 253<br />MinLength: 1<br /> | SecretName is the immutable local name of the generated password source. |


### Permission

Permission declares a SQL grantee's desired privilege state.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL grantee; public represents PostgreSQL PUBLIC. |
| `ensure` | [EnsureOption](#ensureoption) | optional | present | Enum: [present absent]<br /> | Ensure selects presence or explicit removal of this grant only. |


### PermissionStatus

PermissionStatus identifies the result of one permission reconciliation.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `scope` | string | optional |  |  | Scope distinguishes database, schema, relation and default ACL outcomes. |
| `creator` | string | optional |  |  | Creator identifies the actual object creator for default privileges. |
| `class` | string | optional |  |  | Class identifies tables, sequences, functions or types for default privileges. |
| `schema` | string | optional |  |  | Schema identifies the SQL schema when applicable. |
| `table` | string | optional |  |  | Table identifies an explicitly named SQL table when applicable. |
| `grantee` | string | required |  |  | Grantee identifies the SQL role or PUBLIC grantee. |
| `privilege` | string | required |  |  | Privilege identifies the reconciled SQL privilege. |
| `applied` | boolean | optional |  |  | Applied indicates whether the desired permission state was confirmed. |
| `effective` | boolean | optional |  |  | Effective reports access through all ACL grantors, PUBLIC and inherited roles.<br />For defaults it reports the combined global and schema-local future layer. |
| `message` | string | optional |  |  | Message describes the observed outcome without credentials. |


### PositiveDuration

PositiveDuration is a positive Go duration expressed with h, m, s, ms, us, or ns.
CEL: `duration(self) > duration('0s')` - duration must be positive
Validation: MaxLength: 64; Pattern: `^([0-9]+([.][0-9]+)?(h\|m\|s\|ms\|us\|ns))+$`


### PostgresConnection

PostgresConnection identifies the administrative connection target.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `host` | string | required |  | MinLength: 1<br /> | Host is the PostgreSQL hostname or IP address. |
| `port` | integer | optional | 5432 | Maximum: 65535<br />Minimum: 1<br /> | Port is the PostgreSQL TCP port. |
| `database` | [SQLName](#sqlname) | optional | postgres | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Database is the administrative database used for cluster-wide operations. |


### PostgresTLS

PostgresTLS controls the operator's administrative TLS connection.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `mode` | string | optional | verify-full | Enum: [disable allow prefer require verify-ca verify-full]<br /> | Mode is the PostgreSQL sslmode. Defaults to verify-full. |
| `caSecret` | [SecretKeyReference](#secretkeyreference) | optional |  |  | CASecret identifies the local Secret key containing trusted CA certificates. |
| `clientCertificateSecret` | [LocalReference](#localreference) | optional |  |  | ClientCertificateSecret contains tls.crt/tls.key for the operator only. |


### ReclaimPolicy

ReclaimPolicy controls SQL cleanup when the Kubernetes resource is deleted.
Validation: Enum: [retain delete]


### ResourceStatus

ResourceStatus reports reconciliation without including credentials or DSNs.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `observedGeneration` | integer | optional |  |  | ObservedGeneration is the latest generation inspected by the reconciler. |
| `applied` | boolean | optional |  |  | Applied reports whether reconciliation succeeded. |
| `message` | string | optional |  |  | Message describes the most recent reconciliation result. |
| `effectiveName` | string | optional |  |  | EffectiveName is the resolved SQL name, including metadata.name fallback. |
| `appliedGeneration` | integer | optional |  |  | AppliedGeneration is the latest successfully reconciled generation. |
| `lastSuccessfulReconcileTime` | [Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#time-v1-meta) | optional |  |  | LastSuccessfulReconcileTime records the most recent successful reconciliation. |
| `conditions` | [Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.37/#condition-v1-meta) array | optional |  |  | Conditions report readiness and failures, keyed by condition type. |


### RoleEndpointState

RoleEndpointState stores managed or possibly applied changes for one configured endpoint, not a physical server identity.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `host` | string | required |  |  | Host is the normalized configured hostname. |
| `port` | integer | required |  |  | Port is the effective configured TCP port. |
| `memberships` | [RoleMembershipBinding](#rolemembershipbinding) array | optional |  |  | Memberships records grantor-specific application and administrative bindings for retry cleanup. |
| `parameters` | string array | optional |  |  | Parameters records role-wide setting names managed or possibly applied by this resource. |


### RoleMembershipBinding

RoleMembershipBinding identifies one SQL membership without storing transient role OIDs.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `role` | string | required |  |  | Role is the parent SQL role. |
| `member` | string | required |  |  | Member is the SQL grantee. |
| `grantor` | string | required |  |  | Grantor is the SQL role that granted this binding. |
| `adminOption` | boolean | optional |  |  | AdminOption records administrative authority on this binding. |
| `administrative` | boolean | optional |  |  | Administrative distinguishes operator administration from application membership ownership. |


### SQLName

SQLName is a nonempty PostgreSQL identifier. Server-specific byte limits are
checked by the reconciler; identifiers are never truncated.
Validation: MinLength: 1; Pattern: `^[^\x00]+$`


### SchemaPermissions

SchemaPermissions declares schema and schema-wide relation privileges.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `usage` | [Permission](#permission) array | optional |  |  | Usage declares schema USAGE grants. |
| `create` | [Permission](#permission) array | optional |  |  | Create declares schema CREATE grants. |
| `tables` | [TablePermissions](#tablepermissions) | optional |  |  | Tables manages grants on all existing tables in this schema. |
| `sequences` | [SequencePermissions](#sequencepermissions) | optional |  |  | Sequences manages grants on all existing sequences in this schema. |


### SchemaSpec

SchemaSpec declares a schema and its managed ACL scopes.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL object name. |
| `ensure` | [EnsureOption](#ensureoption) | optional | present | Enum: [present absent]<br /> | Ensure requests presence or DROP with RESTRICT; omission means present. |
| `owner` | [SQLName](#sqlname) | optional |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Owner is applied explicitly; new schemas otherwise use the database owner. |
| `permissions` | [SchemaPermissions](#schemapermissions) | optional |  |  | Permissions manages schema, all-table, and all-sequence grants. |
| `defaultPrivileges` | [DefaultPrivileges](#defaultprivileges) array | optional |  |  | DefaultPrivileges contains schema-local defaults for actual creator roles. |
| `tablePermissions` | [NamedTablePermissions](#namedtablepermissions) array | optional |  |  | TablePermissions targets existing named tables without managing their DDL. |


### SecretKeyReference

SecretKeyReference selects a key in a namespace-local Secret.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | string | required |  | MaxLength: 253<br />MinLength: 1<br /> | Name is the Secret name. |
| `key` | string | required |  | MinLength: 1<br /> | Key is the Secret data key containing CA certificates. |


### SequencePermissions

SequencePermissions declares sequence privileges.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `usage` | [Permission](#permission) array | optional |  |  | Usage declares USAGE grants. |
| `select` | [Permission](#permission) array | optional |  |  | Select declares SELECT grants. |
| `update` | [Permission](#permission) array | optional |  |  | Update declares UPDATE grants. |


### ServerSpec

ServerSpec declares a foreign server in CNPG format.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL object name. |
| `ensure` | [EnsureOption](#ensureoption) | optional | present | Enum: [present absent]<br /> | Ensure requests presence or DROP with RESTRICT; omission means present. |
| `fdw` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | FDW is the SQL foreign data wrapper name. |
| `options` | [OptionSpec](#optionspec) array | optional |  |  | Options declares managed server options. |
| `usage` | [UsageSpec](#usagespec) array | optional |  |  | Usage uses CNPG's grant/revoke format. |


### TablePermissions

TablePermissions declares table privileges.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `select` | [Permission](#permission) array | optional |  |  | Select declares SELECT grants. |
| `insert` | [Permission](#permission) array | optional |  |  | Insert declares INSERT grants. |
| `update` | [Permission](#permission) array | optional |  |  | Update declares UPDATE grants. |
| `delete` | [Permission](#permission) array | optional |  |  | Delete declares DELETE grants. |
| `truncate` | [Permission](#permission) array | optional |  |  | Truncate declares TRUNCATE grants. |
| `references` | [Permission](#permission) array | optional |  |  | References declares REFERENCES grants. |
| `trigger` | [Permission](#permission) array | optional |  |  | Trigger declares TRIGGER grants. |
| `maintain` | [Permission](#permission) array | optional |  |  | Maintain declares MAINTAIN grants. |


### TypePermissions

TypePermissions declares type privileges.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `usage` | [Permission](#permission) array | optional |  |  | Usage declares USAGE grants. |


### UsageSpec

UsageSpec declares a CNPG-compatible FDW/server usage operation.

| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
| `name` | [SQLName](#sqlname) | required |  | MinLength: 1<br />Pattern: `^[^\x00]+$`<br /> | Name is the SQL grantee name. |
| `type` | string | optional | grant | Enum: [grant revoke]<br /> | Type selects the CNPG grant/revoke operation. |
