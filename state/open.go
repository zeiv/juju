// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package state

import (
	"fmt"
	"strings"

	"github.com/juju/errors"
	"github.com/juju/names"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/txn"

	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environmentserver/authentication"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/mongo"
	"github.com/juju/juju/state/api/params"
	"github.com/juju/juju/state/presence"
	"github.com/juju/juju/state/watcher"
)

// Open connects to the server described by the given
// info, waits for it to be initialized, and returns a new State
// representing the environment connected to.
//
// A policy may be provided, which will be used to validate and
// modify behaviour of certain operations in state. A nil policy
// may be provided.
//
// Open returns unauthorizedError if access is unauthorized.
func Open(info *authentication.MongoInfo, opts mongo.DialOpts, policy Policy) (*State, error) {
	logger.Infof("opening state, mongo addresses: %q; entity %q", info.Addrs, info.Tag)
	logger.Debugf("dialing mongo")
	session, err := mongo.DialWithInfo(info.Info, opts)
	if err != nil {
		return nil, err
	}
	logger.Debugf("connection established")

	st, err := newState(session, info, policy)
	if err != nil {
		session.Close()
		return nil, err
	}
	return st, nil
}

// Initialize sets up an initial empty state and returns it.
// This needs to be performed only once for a given environment.
// It returns unauthorizedError if access is unauthorized.
func Initialize(info *authentication.MongoInfo, cfg *config.Config, opts mongo.DialOpts, policy Policy) (rst *State, err error) {
	st, err := Open(info, opts, policy)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			st.Close()
		}
	}()
	// A valid environment is used as a signal that the
	// state has already been initalized. If this is the case
	// do nothing.
	if _, err := st.Environment(); err == nil {
		return st, nil
	} else if !errors.IsNotFound(err) {
		return nil, err
	}
	logger.Infof("initializing environment")
	if err := checkEnvironConfig(cfg); err != nil {
		return nil, err
	}
	uuid, ok := cfg.UUID()
	if !ok {
		return nil, errors.Errorf("environment uuid was not supplied")
	}
	st.environTag = names.NewEnvironTag(uuid)
	ops := []txn.Op{
		createConstraintsOp(st, environGlobalKey, constraints.Value{}),
		createSettingsOp(st, environGlobalKey, cfg.AllAttrs()),
		createEnvironmentOp(st, cfg.Name(), uuid),
		{
			C:      stateServersC,
			Id:     environGlobalKey,
			Assert: txn.DocMissing,
			Insert: &stateServersDoc{},
		}, {
			C:      stateServersC,
			Id:     apiHostPortsKey,
			Assert: txn.DocMissing,
			Insert: &apiHostPortsDoc{},
		}, {
			C:      stateServersC,
			Id:     stateServingInfoKey,
			Assert: txn.DocMissing,
			Insert: &params.StateServingInfo{},
		},
	}
	if err := st.runTransaction(ops); err == txn.ErrAborted {
		// The config was created in the meantime.
		return st, nil
	} else if err != nil {
		return nil, err
	}
	return st, nil
}

var indexes = []struct {
	collection string
	key        []string
	unique     bool
}{
	// After the first public release, do not remove entries from here
	// without adding them to a list of indexes to drop, to ensure
	// old databases are modified to have the correct indexes.
	{relationsC, []string{"endpoints.relationname"}, false},
	{relationsC, []string{"endpoints.servicename"}, false},
	{unitsC, []string{"service"}, false},
	{unitsC, []string{"principal"}, false},
	{unitsC, []string{"machineid"}, false},
	// TODO(thumper): schema change to remove this index.
	{usersC, []string{"name"}, false},
	{networksC, []string{"providerid"}, true},
	{networkInterfacesC, []string{"interfacename", "machineid"}, true},
	{networkInterfacesC, []string{"macaddress", "networkname"}, true},
	{networkInterfacesC, []string{"networkname"}, false},
	{networkInterfacesC, []string{"machineid"}, false},
}

// The capped collection used for transaction logs defaults to 10MB.
// It's tweaked in export_test.go to 1MB to avoid the overhead of
// creating and deleting the large file repeatedly in tests.
var (
	logSize      = 10000000
	logSizeTests = 1000000
)

func maybeUnauthorized(err error, msg string) error {
	if err == nil {
		return nil
	}
	if isUnauthorized(err) {
		return errors.Unauthorizedf("%s: unauthorized mongo access: %v", msg, err)
	}
	return fmt.Errorf("%s: %v", msg, err)
}

func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	// Some unauthorized access errors have no error code,
	// just a simple error string.
	if strings.HasPrefix(err.Error(), "auth fail") {
		return true
	}
	if err, ok := err.(*mgo.QueryError); ok {
		return err.Code == 10057 ||
			err.Message == "need to login" ||
			err.Message == "unauthorized" ||
			strings.HasPrefix(err.Message, "not authorized")
	}
	return false
}

func newState(session *mgo.Session, mongoInfo *authentication.MongoInfo, policy Policy) (_ *State, resultErr error) {
	admin := session.DB("admin")
	authenticated := false
	if mongoInfo.Tag != nil {
		if err := admin.Login(mongoInfo.Tag.String(), mongoInfo.Password); err != nil {
			return nil, maybeUnauthorized(err, fmt.Sprintf("cannot log in to admin database as %q", mongoInfo.Tag))
		}
		authenticated = true
	} else if mongoInfo.Password != "" {
		if err := admin.Login(AdminUser, mongoInfo.Password); err != nil {
			return nil, maybeUnauthorized(err, "cannot log in to admin database")
		}
		authenticated = true
	}

	db := session.DB("juju")
	pdb := session.DB("presence")
	st := &State{
		mongoInfo:     mongoInfo,
		policy:        policy,
		authenticated: authenticated,
		db:            db,
	}
	log := db.C(txnLogC)
	logInfo := mgo.CollectionInfo{Capped: true, MaxBytes: logSize}
	// The lack of error code for this error was reported upstream:
	//     https://jira.klmongodb.org/browse/SERVER-6992
	err := log.Create(&logInfo)
	if err != nil && err.Error() != "collection already exists" {
		return nil, maybeUnauthorized(err, "cannot create log collection")
	}
	txns := db.C(txnsC)
	err = txns.Create(&mgo.CollectionInfo{})
	if err != nil && err.Error() != "collection already exists" {
		return nil, maybeUnauthorized(err, "cannot create transaction collection")
	}

	st.watcher = watcher.New(log)
	defer func() {
		if resultErr != nil {
			if err := st.watcher.Stop(); err != nil {
				logger.Errorf("failed to stop watcher: %v", err)
			}
		}
	}()
	st.pwatcher = presence.NewWatcher(pdb.C(presenceC))
	defer func() {
		if resultErr != nil {
			if err := st.pwatcher.Stop(); err != nil {
				logger.Errorf("failed to stop presence watcher: %v", err)
			}
		}
	}()

	for _, item := range indexes {
		index := mgo.Index{Key: item.key, Unique: item.unique}
		if err := db.C(item.collection).EnsureIndex(index); err != nil {
			return nil, errors.Annotate(err, "cannot create database index")
		}
	}

	return st, nil
}

// MongoConnectionInfo returns information for connecting to mongo
func (st *State) MongoConnectionInfo() *authentication.MongoInfo {
	return st.mongoInfo
}

// CACert returns the certificate used to validate the state connection.
func (st *State) CACert() string {
	return st.mongoInfo.CACert
}

func (st *State) Close() error {
	err1 := st.watcher.Stop()
	err2 := st.pwatcher.Stop()
	st.mu.Lock()
	var err3 error
	if st.allManager != nil {
		err3 = st.allManager.Stop()
	}
	st.mu.Unlock()
	st.db.Session.Close()
	for _, err := range []error{err1, err2, err3} {
		if err != nil {
			return err
		}
	}
	return nil
}
