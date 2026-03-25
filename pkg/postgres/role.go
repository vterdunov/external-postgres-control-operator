package postgres

import (
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

const (
	CREATE_GROUP_ROLE   = `CREATE ROLE "%s"`
	RENAME_GROUP_ROLE   = `ALTER ROLE "%s" RENAME TO "%s"`
	CREATE_USER_ROLE    = `CREATE ROLE "%s" WITH LOGIN PASSWORD '%s'`
	GRANT_ROLE          = `GRANT "%s" TO "%s"`
	ALTER_USER_SET_ROLE = `ALTER USER "%s" SET ROLE "%s"`
	REVOKE_ROLE         = `REVOKE "%s" FROM "%s"`
	UPDATE_PASSWORD     = `ALTER ROLE "%s" WITH PASSWORD '%s'`
	DROP_ROLE           = `DROP ROLE "%s"`
	DROP_OWNED_BY       = `DROP OWNED BY "%s"`
	REASIGN_OBJECTS     = `REASSIGN OWNED BY "%s" TO "%s"`
)

func (c *pg) CreateGroupRole(role string) error {
	// Error code 42710 is duplicate_object (role already exists)
	_, err := c.db.Exec(fmt.Sprintf(CREATE_GROUP_ROLE, role))
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "42710" {
			return nil
		}
		return err
	}
	return nil
}

func (c *pg) RenameGroupRole(currentRole, newRole string) error {
	_, err := c.db.Exec(fmt.Sprintf(RENAME_GROUP_ROLE, currentRole, newRole))
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok {
			// 42704 => role does not exist; treat as success so caller can recreate
			if pqErr.Code == "42704" {
				return nil
			}
		}
		return err
	}
	return nil
}

func (c *pg) CreateUserRole(role, password string) (string, error) {
	_, err := c.db.Exec(fmt.Sprintf(CREATE_USER_ROLE, role, password))
	if err != nil {
		return "", err
	}
	return role, nil
}

func (c *pg) GrantRole(role, grantee string) error {
	_, err := c.db.Exec(fmt.Sprintf(GRANT_ROLE, role, grantee))
	if err != nil {
		return err
	}
	return nil
}

func (c *pg) AlterDefaultLoginRole(role, setRole string) error {
	_, err := c.db.Exec(fmt.Sprintf(ALTER_USER_SET_ROLE, role, setRole))
	if err != nil {
		return err
	}
	return nil
}

func (c *pg) RevokeRole(role, revoked string) error {
	_, err := c.db.Exec(fmt.Sprintf(REVOKE_ROLE, role, revoked))
	if err != nil {
		return err
	}
	return nil
}

func (c *pg) DropRole(role, newOwner, database string) error {
	tmpDb, err := GetConnection(c.user, c.pass, c.host, database, c.args)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "3D000" {
		} else {
			return err
		}
	} else {
		defer tmpDb.Close()

		if err := c.reassignAndDrop(tmpDb, role, newOwner); err != nil {
			return err
		}
	}

	// Also clean up privileges on the default database (e.g. CONNECT grants)
	if err := c.reassignAndDrop(c.db, role, newOwner); err != nil {
		return err
	}

	_, err = c.db.Exec(fmt.Sprintf(DROP_ROLE, role))
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "42704" {
			return nil
		}
		return err
	}
	return nil
}

func (c *pg) reassignAndDrop(db *sql.DB, role, newOwner string) error {
	_, err := db.Exec(fmt.Sprintf(REASIGN_OBJECTS, role, newOwner))
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "42704" {
			// role not found, continue
		} else {
			return err
		}
	}

	_, err = db.Exec(fmt.Sprintf(DROP_OWNED_BY, role))
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "42704" {
			// role not found, continue
		} else {
			return err
		}
	}
	return nil
}

func (c *pg) UpdatePassword(role, password string) error {
	_, err := c.db.Exec(fmt.Sprintf(UPDATE_PASSWORD, role, password))
	if err != nil {
		return err
	}

	return nil
}
