// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/executor/internal/exec"
	"github.com/pingcap/tidb/pkg/extension"
	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/planner/core/resolve"
	"github.com/pingcap/tidb/pkg/privilege"
	"github.com/pingcap/tidb/pkg/privilege/privileges"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/sessiontxn"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/dbterror/exeerrors"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/sqlescape"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
	"go.uber.org/zap"
)

/***
 * Grant Statement
 * See https://dev.mysql.com/doc/refman/5.7/en/grant.html
 ************************************************************************************/
var (
	_ exec.Executor = (*GrantExec)(nil)
)

// GrantExec executes GrantStmt.
type GrantExec struct {
	exec.BaseExecutor

	Privs                 []*ast.PrivElem
	ObjectType            ast.ObjectTypeType
	Level                 *ast.GrantLevel
	Users                 []*ast.UserSpec
	AuthTokenOrTLSOptions []*ast.AuthTokenOrTLSOption

	is        infoschema.InfoSchema
	WithGrant bool
	done      bool
}

// Next implements the Executor Next interface.
func (e *GrantExec) Next(ctx context.Context, _ *chunk.Chunk) error {
	if e.done {
		return nil
	}
	e.done = true
	internalCtx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)

	dbName := e.Level.DBName
	if len(dbName) == 0 {
		dbName = e.Ctx().GetSessionVars().CurrentDB
	}

	// For table & column level, check whether table exists and privilege is valid
	if e.Level.Level == ast.GrantLevelTable {
		// Return if privilege is invalid, to fail before not existing table, see issue #29302
		for _, p := range e.Privs {
			if len(p.Cols) == 0 {
				if !mysql.AllTablePrivs.Has(p.Priv) && p.Priv != mysql.AllPriv && p.Priv != mysql.UsagePriv && p.Priv != mysql.GrantPriv && p.Priv != mysql.ExtendedPriv {
					return exeerrors.ErrIllegalGrantForTable
				}
			} else {
				if !mysql.AllColumnPrivs.Has(p.Priv) && p.Priv != mysql.AllPriv && p.Priv != mysql.UsagePriv {
					return exeerrors.ErrWrongUsage.GenWithStackByArgs("COLUMN GRANT", "NON-COLUMN PRIVILEGES")
				}
			}
		}
		dbNameStr := ast.NewCIStr(dbName)
		schema := e.Ctx().GetDomain().(*domain.Domain).InfoSchema()
		tbl, err := schema.TableByName(ctx, dbNameStr, ast.NewCIStr(e.Level.TableName))
		// Allow GRANT on non-existent table with at least create privilege, see issue #28533 #29268
		if err != nil {
			allowed := false
			if terror.ErrorEqual(err, infoschema.ErrTableNotExists) {
				for _, p := range e.Privs {
					if p.Priv == mysql.AllPriv || p.Priv&mysql.CreatePriv > 0 {
						allowed = true
						break
					}
				}
			}
			if !allowed {
				return err
			}
		}
		// Note the table name compare is not case sensitive here.
		// In TiDB, system variable lower_case_table_names = 2 which means name comparisons are not case-sensitive.
		if tbl != nil && tbl.Meta().Name.L != strings.ToLower(e.Level.TableName) {
			return infoschema.ErrTableNotExists.GenWithStackByArgs(dbName, e.Level.TableName)
		}
		if len(e.Level.DBName) > 0 {
			// The database name should also match.
			db, succ := schema.SchemaByName(dbNameStr)
			if !succ || db.Name.L != dbNameStr.L {
				return infoschema.ErrTableNotExists.GenWithStackByArgs(dbName, e.Level.TableName)
			}
		}
	}

	// Commit the old transaction, like DDL.
	if err := sessiontxn.NewTxnInStmt(ctx, e.Ctx()); err != nil {
		return err
	}
	defer func() { e.Ctx().GetSessionVars().SetInTxn(false) }()

	// Create internal session to start internal transaction.
	isCommit := false
	internalSession, err := e.GetSysSession()
	internalSession.GetSessionVars().User = e.Ctx().GetSessionVars().User
	if err != nil {
		return err
	}
	defer func() {
		if !isCommit {
			_, err := internalSession.GetSQLExecutor().ExecuteInternal(internalCtx, "rollback")
			if err != nil {
				logutil.BgLogger().Error("rollback error occur at grant privilege", zap.Error(err))
			}
		}
		e.ReleaseSysSession(internalCtx, internalSession)
	}()

	_, err = internalSession.GetSQLExecutor().ExecuteInternal(internalCtx, "begin")
	if err != nil {
		return err
	}

	defaultAuthPlugin, err := e.Ctx().GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.DefaultAuthPlugin)
	if err != nil {
		return err
	}
	// Check which user is not exist.
	for _, user := range e.Users {
		if user.User.CurrentUser {
			user.User.Username = e.Ctx().GetSessionVars().User.AuthUsername
			user.User.Hostname = e.Ctx().GetSessionVars().User.AuthHostname
		}
		exists, err := userExists(ctx, e.Ctx(), user.User.Username, user.User.Hostname)
		if err != nil {
			return err
		}
		if !exists {
			if e.Ctx().GetSessionVars().SQLMode.HasNoAutoCreateUserMode() {
				return exeerrors.ErrCantCreateUserWithGrant
			}
			// This code path only applies if mode NO_AUTO_CREATE_USER is unset.
			// It is required for compatibility with 5.7 but removed from 8.0
			// since it results in a massive security issue:
			// spelling errors will create users with no passwords.
			authPlugin := defaultAuthPlugin
			if user.AuthOpt != nil && user.AuthOpt.AuthPlugin != "" {
				authPlugin = user.AuthOpt.AuthPlugin
			}
			extensions, extErr := extension.GetExtensions()
			if extErr != nil {
				return exeerrors.ErrPluginIsNotLoaded.GenWithStackByArgs(extErr.Error())
			}
			authPluginImpl := extensions.GetAuthPlugins()[authPlugin]
			pwd, ok := encodePasswordWithPlugin(*user, authPluginImpl, defaultAuthPlugin)
			if !ok {
				return errors.Trace(exeerrors.ErrPasswordFormat)
			}
			_, err = internalSession.GetSQLExecutor().ExecuteInternal(internalCtx,
				`INSERT INTO %n.%n (Host, User, authentication_string, plugin) VALUES (%?, %?, %?, %?);`,
				mysql.SystemDB, mysql.UserTable, user.User.Hostname, user.User.Username, pwd, authPlugin)
			if err != nil {
				return err
			}
		}
	}

	// Grant for each user
	for _, user := range e.Users {
		// If there is no privilege entry in corresponding table, insert a new one.
		// Global scope:		mysql.global_priv
		// DB scope:			mysql.DB
		// Table scope:			mysql.Tables_priv
		// Column scope:		mysql.Columns_priv
		if e.AuthTokenOrTLSOptions != nil {
			err = checkAndInitGlobalPriv(internalSession, user.User.Username, user.User.Hostname)
			if err != nil {
				return err
			}
		}
		switch e.Level.Level {
		case ast.GrantLevelDB:
			err := checkAndInitDBPriv(internalSession, dbName, user.User.Username, user.User.Hostname)
			if err != nil {
				return err
			}
		case ast.GrantLevelTable:
			err := checkAndInitTablePriv(internalSession, dbName, e.Level.TableName, e.is, user.User.Username, user.User.Hostname)
			if err != nil {
				return err
			}
		}

		// Previously "WITH GRANT OPTION" implied setting the Grant_Priv in mysql.user.
		// However, with DYNAMIC privileges the GRANT OPTION is individually grantable, and not a global
		// property of the user. The logic observed in MySQL 8.0 is as follows:
		// - The GRANT OPTION applies to all PrivElems in e.Privs.
		// - Thus, if PrivElems contains any non-DYNAMIC privileges, the user GRANT option needs to be set.
		// - If it contains ONLY dynamic privileges, don't set the GRANT option, as it is individually set in the handling of dynamic options.
		privs := e.Privs
		if e.WithGrant && containsNonDynamicPriv(privs) {
			privs = append(privs, &ast.PrivElem{Priv: mysql.GrantPriv})
		}

		// Grant TLS privs to use in global table
		err = e.grantGlobalPriv(internalSession, user)
		if err != nil {
			return err
		}
		// Grant each priv to the user.
		for _, priv := range privs {
			if len(priv.Cols) > 0 {
				// Check column scope privilege entry.
				// TODO: Check validity before insert new entry.
				err := e.checkAndInitColumnPriv(ctx, user.User.Username, user.User.Hostname, priv.Cols, internalSession)
				if err != nil {
					return err
				}
			}
			err := e.grantLevelPriv(ctx, priv, user, internalSession)
			if err != nil {
				return err
			}
		}
	}

	_, err = internalSession.GetSQLExecutor().ExecuteInternal(internalCtx, "commit")
	if err != nil {
		return err
	}
	isCommit = true
	users := userSpecToUserList(e.Users)
	return domain.GetDomain(e.Ctx()).NotifyUpdatePrivilege(users)
}

func containsNonDynamicPriv(privList []*ast.PrivElem) bool {
	for _, priv := range privList {
		if priv.Priv != mysql.ExtendedPriv {
			return true
		}
	}
	return false
}

// checkAndInitGlobalPriv checks if global scope privilege entry exists in mysql.global_priv.
// If not exists, insert a new one.
func checkAndInitGlobalPriv(ctx sessionctx.Context, user string, host string) error {
	ok, err := globalPrivEntryExists(ctx, user, host)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	// Entry does not exist for user-host-db. Insert a new entry.
	return initGlobalPrivEntry(ctx, user, host)
}

// checkAndInitDBPriv checks if DB scope privilege entry exists in mysql.DB.
// If unexists, insert a new one.
func checkAndInitDBPriv(ctx sessionctx.Context, dbName string, user string, host string) error {
	ok, err := dbUserExists(ctx, user, host, dbName)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	// Entry does not exist for user-host-db. Insert a new entry.
	return initDBPrivEntry(ctx, user, host, dbName)
}

// checkAndInitTablePriv checks if table scope privilege entry exists in mysql.Tables_priv.
// If unexists, insert a new one.
func checkAndInitTablePriv(ctx sessionctx.Context, dbName, tblName string, _ infoschema.InfoSchema, user string, host string) error {
	ok, err := tableUserExists(ctx, user, host, dbName, tblName)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	// Entry does not exist for user-host-db-tbl. Insert a new entry.
	return initTablePrivEntry(ctx, user, host, dbName, tblName)
}

// checkAndInitColumnPriv checks if column scope privilege entry exists in mysql.Columns_priv.
// If unexists, insert a new one.
func (e *GrantExec) checkAndInitColumnPriv(ctx context.Context, user string, host string, cols []*ast.ColumnName, internalSession sessionctx.Context) error {
	dbName, tbl, err := getTargetSchemaAndTable(ctx, e.Ctx(), e.Level.DBName, e.Level.TableName, e.is)
	if err != nil {
		return err
	}
	for _, c := range cols {
		col := table.FindCol(tbl.Cols(), c.Name.L)
		if col == nil {
			return errors.Errorf("Unknown column: %s", c.Name.O)
		}
		ok, err := columnPrivEntryExists(internalSession, user, host, dbName, tbl.Meta().Name.O, col.Name.O)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		// Entry does not exist for user-host-db-tbl-col. Insert a new entry.
		err = initColumnPrivEntry(internalSession, user, host, dbName, tbl.Meta().Name.O, col.Name.O)
		if err != nil {
			return err
		}
	}
	return nil
}

// initGlobalPrivEntry inserts a new row into mysql.DB with empty privilege.
func initGlobalPrivEntry(sctx sessionctx.Context, user string, host string) error {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	_, err := sctx.GetSQLExecutor().ExecuteInternal(ctx, `INSERT INTO %n.%n (Host, User, PRIV) VALUES (%?, %?, %?)`, mysql.SystemDB, mysql.GlobalPrivTable, host, user, "{}")
	return err
}

// initDBPrivEntry inserts a new row into mysql.DB with empty privilege.
func initDBPrivEntry(sctx sessionctx.Context, user string, host string, db string) error {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	_, err := sctx.GetSQLExecutor().ExecuteInternal(ctx, `INSERT INTO %n.%n (Host, User, DB) VALUES (%?, %?, %?)`, mysql.SystemDB, mysql.DBTable, host, user, db)
	return err
}

// initTablePrivEntry inserts a new row into mysql.Tables_priv with empty privilege.
func initTablePrivEntry(sctx sessionctx.Context, user string, host string, db string, tbl string) error {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	_, err := sctx.GetSQLExecutor().ExecuteInternal(ctx, `INSERT INTO %n.%n (Host, User, DB, Table_name, Table_priv, Column_priv) VALUES (%?, %?, %?, %?, '', '')`, mysql.SystemDB, mysql.TablePrivTable, host, user, db, tbl)
	return err
}

// initColumnPrivEntry inserts a new row into mysql.Columns_priv with empty privilege.
func initColumnPrivEntry(sctx sessionctx.Context, user string, host string, db string, tbl string, col string) error {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	_, err := sctx.GetSQLExecutor().ExecuteInternal(ctx, `INSERT INTO %n.%n (Host, User, DB, Table_name, Column_name, Column_priv) VALUES (%?, %?, %?, %?, %?, '')`, mysql.SystemDB, mysql.ColumnPrivTable, host, user, db, tbl, col)
	return err
}

// grantGlobalPriv grants priv to user in global scope.
func (e *GrantExec) grantGlobalPriv(sctx sessionctx.Context, user *ast.UserSpec) error {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	if len(e.AuthTokenOrTLSOptions) == 0 {
		return nil
	}
	priv, err := tlsOption2GlobalPriv(e.AuthTokenOrTLSOptions)
	if err != nil {
		return errors.Trace(err)
	}
	_, err = sctx.GetSQLExecutor().ExecuteInternal(ctx, `UPDATE %n.%n SET PRIV=%? WHERE User=%? AND Host=%?`, mysql.SystemDB, mysql.GlobalPrivTable, priv, user.User.Username, user.User.Hostname)
	return err
}

func tlsOption2GlobalPriv(authTokenOrTLSOptions []*ast.AuthTokenOrTLSOption) (priv []byte, err error) {
	if len(authTokenOrTLSOptions) == 0 {
		priv = []byte("{}")
		return
	}
	dupSet := make(map[ast.AuthTokenOrTLSOptionType]struct{})
	for _, opt := range authTokenOrTLSOptions {
		if _, dup := dupSet[opt.Type]; dup {
			var typeName string
			switch opt.Type {
			case ast.Cipher:
				typeName = "CIPHER"
			case ast.Issuer:
				typeName = "ISSUER"
			case ast.Subject:
				typeName = "SUBJECT"
			case ast.SAN:
				typeName = "SAN"
			case ast.TokenIssuer:
			}
			err = errors.Errorf("Duplicate require %s clause", typeName)
			return
		}
		dupSet[opt.Type] = struct{}{}
	}
	gp := privileges.GlobalPrivValue{SSLType: privileges.SslTypeNotSpecified}
	for _, opt := range authTokenOrTLSOptions {
		switch opt.Type {
		case ast.TlsNone:
			gp.SSLType = privileges.SslTypeNone
		case ast.Ssl:
			gp.SSLType = privileges.SslTypeAny
		case ast.X509:
			gp.SSLType = privileges.SslTypeX509
		case ast.Cipher:
			gp.SSLType = privileges.SslTypeSpecified
			if len(opt.Value) > 0 {
				if _, ok := util.SupportCipher[opt.Value]; !ok {
					err = errors.Errorf("Unsupported cipher suit: %s", opt.Value)
					return
				}
				gp.SSLCipher = opt.Value
			}
		case ast.Issuer:
			err = util.CheckSupportX509NameOneline(opt.Value)
			if err != nil {
				return
			}
			gp.SSLType = privileges.SslTypeSpecified
			gp.X509Issuer = opt.Value
		case ast.Subject:
			err = util.CheckSupportX509NameOneline(opt.Value)
			if err != nil {
				return
			}
			gp.SSLType = privileges.SslTypeSpecified
			gp.X509Subject = opt.Value
		case ast.SAN:
			gp.SSLType = privileges.SslTypeSpecified
			_, err = util.ParseAndCheckSAN(opt.Value)
			if err != nil {
				return
			}
			gp.SAN = opt.Value
		case ast.TokenIssuer:
		default:
			err = errors.Errorf("Unknown ssl type: %#v", opt.Type)
			return
		}
	}
	if gp.SSLType == privileges.SslTypeNotSpecified && len(gp.SSLCipher) == 0 &&
		len(gp.X509Issuer) == 0 && len(gp.X509Subject) == 0 && len(gp.SAN) == 0 {
		return
	}
	priv, err = json.Marshal(&gp)
	if err != nil {
		return
	}
	return
}

// grantLevelPriv grants priv to user in s.Level scope.
func (e *GrantExec) grantLevelPriv(ctx context.Context, priv *ast.PrivElem, user *ast.UserSpec, internalSession sessionctx.Context) error {
	if priv.Priv == mysql.ExtendedPriv {
		return e.grantDynamicPriv(priv.Name, user, internalSession)
	}
	if priv.Priv == mysql.UsagePriv {
		return nil
	}
	switch e.Level.Level {
	case ast.GrantLevelGlobal:
		return e.grantGlobalLevel(priv, user, internalSession)
	case ast.GrantLevelDB:
		return e.grantDBLevel(priv, user, internalSession)
	case ast.GrantLevelTable:
		if len(priv.Cols) == 0 {
			return e.grantTableLevel(priv, user, internalSession)
		}
		return e.grantColumnLevel(ctx, priv, user, internalSession)
	default:
		return errors.Errorf("Unknown grant level: %#v", e.Level)
	}
}

func (e *GrantExec) grantDynamicPriv(privName string, user *ast.UserSpec, internalSession sessionctx.Context) error {
	privName = strings.ToUpper(privName)
	if e.Level.Level != ast.GrantLevelGlobal { // DYNAMIC can only be *.*
		return exeerrors.ErrIllegalPrivilegeLevel.GenWithStackByArgs(privName)
	}
	if !privilege.GetPrivilegeManager(e.Ctx()).IsDynamicPrivilege(privName) {
		// In GRANT context, MySQL returns a syntax error if the privilege has not been registered with the server:
		// ERROR 1149 (42000): You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use
		// But in REVOKE context, it returns a warning ErrDynamicPrivilegeNotRegistered. It is not strictly compatible,
		// but TiDB returns the more useful ErrDynamicPrivilegeNotRegistered instead of a parse error.
		return exeerrors.ErrDynamicPrivilegeNotRegistered.GenWithStackByArgs(privName)
	}
	grantOption := "N"
	if e.WithGrant {
		grantOption = "Y"
	}
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	_, err := internalSession.GetSQLExecutor().ExecuteInternal(ctx, `REPLACE INTO %n.global_grants (user,host,priv,with_grant_option) VALUES (%?, %?, %?, %?)`, mysql.SystemDB, user.User.Username, user.User.Hostname, privName, grantOption)
	return err
}

// grantGlobalLevel manipulates mysql.user table.
func (*GrantExec) grantGlobalLevel(priv *ast.PrivElem, user *ast.UserSpec, internalSession sessionctx.Context) error {
	sql := new(strings.Builder)
	sqlescape.MustFormatSQL(sql, `UPDATE %n.%n SET `, mysql.SystemDB, mysql.UserTable)
	err := composeGlobalPrivUpdate(sql, priv.Priv, "Y")
	if err != nil {
		return err
	}
	sqlescape.MustFormatSQL(sql, ` WHERE User=%? AND Host=%?`, user.User.Username, user.User.Hostname)

	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	_, err = internalSession.GetSQLExecutor().ExecuteInternal(ctx, sql.String())
	return err
}

// grantDBLevel manipulates mysql.db table.
func (e *GrantExec) grantDBLevel(priv *ast.PrivElem, user *ast.UserSpec, internalSession sessionctx.Context) error {
	if slices.Contains(mysql.StaticGlobalOnlyPrivs, priv.Priv) {
		return exeerrors.ErrWrongUsage.GenWithStackByArgs("DB GRANT", "GLOBAL PRIVILEGES")
	}

	dbName := e.Level.DBName
	if len(dbName) == 0 {
		dbName = e.Ctx().GetSessionVars().CurrentDB
	}

	sql := new(strings.Builder)
	sqlescape.MustFormatSQL(sql, "UPDATE %n.%n SET ", mysql.SystemDB, mysql.DBTable)
	err := composeDBPrivUpdate(sql, priv.Priv, "Y")
	if err != nil {
		return err
	}
	sqlescape.MustFormatSQL(sql, " WHERE User=%? AND Host=%? AND DB=%?", user.User.Username, user.User.Hostname, dbName)

	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	_, err = internalSession.GetSQLExecutor().ExecuteInternal(ctx, sql.String())
	return err
}

// grantTableLevel manipulates mysql.tables_priv table.
func (e *GrantExec) grantTableLevel(priv *ast.PrivElem, user *ast.UserSpec, internalSession sessionctx.Context) error {
	dbName := e.Level.DBName
	if len(dbName) == 0 {
		dbName = e.Ctx().GetSessionVars().CurrentDB
	}
	tblName := e.Level.TableName

	sql := new(strings.Builder)
	sqlescape.MustFormatSQL(sql, "UPDATE %n.%n SET ", mysql.SystemDB, mysql.TablePrivTable)
	err := composeTablePrivUpdateForGrant(internalSession, sql, priv.Priv, user.User.Username, user.User.Hostname, dbName, tblName)
	if err != nil {
		return err
	}
	sqlescape.MustFormatSQL(sql, " WHERE User=%? AND Host=%? AND DB=%? AND Table_name=%?", user.User.Username, user.User.Hostname, dbName, tblName)

	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	_, err = internalSession.GetSQLExecutor().ExecuteInternal(ctx, sql.String())
	return err
}

// grantColumnLevel manipulates mysql.tables_priv table.
func (e *GrantExec) grantColumnLevel(ctx context.Context, priv *ast.PrivElem, user *ast.UserSpec, internalSession sessionctx.Context) error {
	dbName, tbl, err := getTargetSchemaAndTable(ctx, e.Ctx(), e.Level.DBName, e.Level.TableName, e.is)
	if err != nil {
		return err
	}

	for _, c := range priv.Cols {
		col := table.FindCol(tbl.Cols(), c.Name.L)
		if col == nil {
			return errors.Errorf("Unknown column: %s", c)
		}

		sql := new(strings.Builder)
		sqlescape.MustFormatSQL(sql, "UPDATE %n.%n SET ", mysql.SystemDB, mysql.ColumnPrivTable)
		err := composeColumnPrivUpdateForGrant(internalSession, sql, priv.Priv, user.User.Username, user.User.Hostname, dbName, tbl.Meta().Name.O, col.Name.O)
		if err != nil {
			return err
		}
		sqlescape.MustFormatSQL(sql, " WHERE User=%? AND Host=%? AND DB=%? AND Table_name=%? AND Column_name=%?", user.User.Username, user.User.Hostname, dbName, tbl.Meta().Name.O, col.Name.O)

		ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
		_, err = internalSession.GetSQLExecutor().ExecuteInternal(ctx, sql.String())
		if err != nil {
			return err
		}
	}
	return nil
}

// composeGlobalPrivUpdate composes update stmt assignment list string for global scope privilege update.
func composeGlobalPrivUpdate(sql *strings.Builder, priv mysql.PrivilegeType, value string) error {
	if priv != mysql.AllPriv {
		if priv != mysql.GrantPriv && !mysql.AllGlobalPrivs.Has(priv) {
			return exeerrors.ErrWrongUsage.GenWithStackByArgs("GLOBAL GRANT", "NON-GLOBAL PRIVILEGES")
		}
		sqlescape.MustFormatSQL(sql, "%n=%?", priv.ColumnString(), value)
		return nil
	}

	for i, v := range mysql.AllGlobalPrivs {
		if i > 0 {
			sqlescape.MustFormatSQL(sql, ",")
		}
		sqlescape.MustFormatSQL(sql, "%n=%?", v.ColumnString(), value)
	}
	return nil
}

// composeDBPrivUpdate composes update stmt assignment list for db scope privilege update.
func composeDBPrivUpdate(sql *strings.Builder, priv mysql.PrivilegeType, value string) error {
	if priv != mysql.AllPriv {
		if priv != mysql.GrantPriv && !mysql.AllDBPrivs.Has(priv) {
			return exeerrors.ErrWrongUsage.GenWithStackByArgs("DB GRANT", "NON-DB PRIVILEGES")
		}
		sqlescape.MustFormatSQL(sql, "%n=%?", priv.ColumnString(), value)
		return nil
	}

	for i, p := range mysql.AllDBPrivs {
		if i > 0 {
			sqlescape.MustFormatSQL(sql, ",")
		}
		sqlescape.MustFormatSQL(sql, "%n=%?", p.ColumnString(), value)
	}
	return nil
}

// composeTablePrivUpdateForGrant composes update stmt assignment list for table scope privilege update.
func composeTablePrivUpdateForGrant(ctx sessionctx.Context, sql *strings.Builder, priv mysql.PrivilegeType, name string, host string, db string, tbl string) error {
	var newTablePriv, newColumnPriv []string
	if priv != mysql.AllPriv {
		currTablePriv, currColumnPriv, err := getTablePriv(ctx, name, host, db, tbl)
		if err != nil {
			return err
		}
		newTablePriv = SetFromString(currTablePriv)
		newTablePriv = addToSet(newTablePriv, priv.SetString())

		newColumnPriv = SetFromString(currColumnPriv)
		if mysql.AllColumnPrivs.Has(priv) {
			newColumnPriv = addToSet(newColumnPriv, priv.SetString())
		}
	} else {
		for _, p := range mysql.AllTablePrivs {
			newTablePriv = addToSet(newTablePriv, p.SetString())
		}

		for _, p := range mysql.AllColumnPrivs {
			newColumnPriv = addToSet(newColumnPriv, p.SetString())
		}
	}

	sqlescape.MustFormatSQL(sql, `Table_priv=%?, Column_priv=%?, Grantor=%?`, setToString(newTablePriv), setToString(newColumnPriv), ctx.GetSessionVars().User.String())
	return nil
}

// composeColumnPrivUpdateForGrant composes update stmt assignment list for column scope privilege update.
func composeColumnPrivUpdateForGrant(ctx sessionctx.Context, sql *strings.Builder, priv mysql.PrivilegeType, name string, host string, db string, tbl string, col string) error {
	var newColumnPriv []string
	if priv != mysql.AllPriv {
		currColumnPriv, err := getColumnPriv(ctx, name, host, db, tbl, col)
		if err != nil {
			return err
		}
		newColumnPriv = SetFromString(currColumnPriv)
		newColumnPriv = addToSet(newColumnPriv, priv.SetString())
	} else {
		for _, p := range mysql.AllColumnPrivs {
			newColumnPriv = addToSet(newColumnPriv, p.SetString())
		}
	}

	sqlescape.MustFormatSQL(sql, `Column_priv=%?`, setToString(newColumnPriv))
	return nil
}

// recordExists is a helper function to check if the sql returns any row.
func recordExists(sctx sessionctx.Context, sql string, args ...any) (bool, error) {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	rs, err := sctx.GetSQLExecutor().ExecuteInternal(ctx, sql, args...)
	if err != nil {
		return false, err
	}
	rows, _, err := getRowsAndFields(sctx, rs)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// globalPrivEntryExists checks if there is an entry with key user-host in mysql.global_priv.
func globalPrivEntryExists(ctx sessionctx.Context, name string, host string) (bool, error) {
	return recordExists(ctx, `SELECT * FROM %n.%n WHERE User=%? AND Host=%?;`, mysql.SystemDB, mysql.GlobalPrivTable, name, host)
}

// dbUserExists checks if there is an entry with key user-host-db in mysql.DB.
func dbUserExists(ctx sessionctx.Context, name string, host string, db string) (bool, error) {
	return recordExists(ctx, `SELECT * FROM %n.%n WHERE User=%? AND Host=%? AND DB=%?;`, mysql.SystemDB, mysql.DBTable, name, host, db)
}

// tableUserExists checks if there is an entry with key user-host-db-tbl in mysql.Tables_priv.
func tableUserExists(ctx sessionctx.Context, name string, host string, db string, tbl string) (bool, error) {
	return recordExists(ctx, `SELECT * FROM %n.%n WHERE User=%? AND Host=%? AND DB=%? AND Table_name=%?;`, mysql.SystemDB, mysql.TablePrivTable, name, host, db, tbl)
}

// columnPrivEntryExists checks if there is an entry with key user-host-db-tbl-col in mysql.Columns_priv.
func columnPrivEntryExists(ctx sessionctx.Context, name string, host string, db string, tbl string, col string) (bool, error) {
	return recordExists(ctx, `SELECT * FROM %n.%n WHERE User=%? AND Host=%? AND DB=%? AND Table_name=%? AND Column_name=%?;`, mysql.SystemDB, mysql.ColumnPrivTable, name, host, db, tbl, col)
}

// getTablePriv gets current table scope privilege set from mysql.Tables_priv.
// Return Table_priv and Column_priv.
func getTablePriv(sctx sessionctx.Context, name string, host string, db string, tbl string) (tPriv, cPriv string, err error) {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	rs, err := sctx.GetSQLExecutor().ExecuteInternal(ctx, `SELECT Table_priv, Column_priv FROM %n.%n WHERE User=%? AND Host=%? AND DB=%? AND Table_name=%?`, mysql.SystemDB, mysql.TablePrivTable, name, host, db, tbl)
	if err != nil {
		return "", "", err
	}
	rows, fields, err := getRowsAndFields(sctx, rs)
	if err != nil {
		return "", "", errors.Errorf("get table privilege fail for %s %s %s %s: %v", name, host, db, tbl, err)
	}
	if len(rows) < 1 {
		return "", "", errors.Errorf("get table privilege fail for %s %s %s %s", name, host, db, tbl)
	}
	row := rows[0]
	if fields[0].Column.GetType() == mysql.TypeSet {
		tablePriv := row.GetSet(0)
		tPriv = tablePriv.Name
	}
	if fields[1].Column.GetType() == mysql.TypeSet {
		columnPriv := row.GetSet(1)
		cPriv = columnPriv.Name
	}
	return tPriv, cPriv, nil
}

// getColumnPriv gets current column scope privilege set from mysql.Columns_priv.
// Return Column_priv.
func getColumnPriv(sctx sessionctx.Context, name string, host string, db string, tbl string, col string) (string, error) {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	rs, err := sctx.GetSQLExecutor().ExecuteInternal(ctx, `SELECT Column_priv FROM %n.%n WHERE User=%? AND Host=%? AND DB=%? AND Table_name=%? AND Column_name=%?;`, mysql.SystemDB, mysql.ColumnPrivTable, name, host, db, tbl, col)
	if err != nil {
		return "", err
	}
	rows, fields, err := getRowsAndFields(sctx, rs)
	if err != nil {
		return "", errors.Errorf("get column privilege fail for %s %s %s %s: %s", name, host, db, tbl, err)
	}
	if len(rows) < 1 {
		return "", errors.Errorf("get column privilege fail for %s %s %s %s %s", name, host, db, tbl, col)
	}
	cPriv := ""
	if fields[0].Column.GetType() == mysql.TypeSet {
		setVal := rows[0].GetSet(0)
		cPriv = setVal.Name
	}
	return cPriv, nil
}

// getTargetSchemaAndTable finds the schema and table by dbName and tableName.
func getTargetSchemaAndTable(ctx context.Context, sctx sessionctx.Context, dbName, tableName string, is infoschema.InfoSchema) (string, table.Table, error) {
	if len(dbName) == 0 {
		dbName = sctx.GetSessionVars().CurrentDB
		if len(dbName) == 0 {
			return "", nil, errors.New("miss DB name for grant privilege")
		}
	}
	name := ast.NewCIStr(tableName)
	tbl, err := is.TableByName(ctx, ast.NewCIStr(dbName), name)
	if terror.ErrorEqual(err, infoschema.ErrTableNotExists) {
		return dbName, nil, err
	}
	if err != nil {
		return "", nil, err
	}
	return dbName, tbl, nil
}

// getRowsAndFields is used to extract rows from record sets.
func getRowsAndFields(sctx sessionctx.Context, rs sqlexec.RecordSet) ([]chunk.Row, []*resolve.ResultField, error) {
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnPrivilege)
	if rs == nil {
		return nil, nil, errors.Errorf("nil recordset")
	}
	rows, err := getRowFromRecordSet(ctx, sctx, rs)
	if err != nil {
		return nil, nil, err
	}
	if err = rs.Close(); err != nil {
		return nil, nil, err
	}
	return rows, rs.Fields(), nil
}

func getRowFromRecordSet(ctx context.Context, se sessionctx.Context, rs sqlexec.RecordSet) ([]chunk.Row, error) {
	var rows []chunk.Row
	req := rs.NewChunk(nil)
	for {
		err := rs.Next(ctx, req)
		if err != nil || req.NumRows() == 0 {
			return rows, err
		}
		iter := chunk.NewIterator4Chunk(req)
		for r := iter.Begin(); r != iter.End(); r = iter.Next() {
			rows = append(rows, r)
		}
		req = chunk.Renew(req, se.GetSessionVars().MaxChunkSize)
	}
}
