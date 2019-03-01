/*
Copyright 2018 Pressinfra SRL

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apphelper

import (
	"fmt"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"

	"github.com/presslabs/mysql-operator/pkg/sidecar/app"
)

var log = logf.Log.WithName("sidecar.apphelper")

const (
	// timeOut represents the number of tries to check mysql to be ready.
	timeOut = 60
	// connRetry represents the number of tries to connect to master server
	connRetry = 10
)

// RunRunCommand is the main command, and represents the runtime helper that
// configures the mysql server
func RunRunCommand(cfg *app.MysqlConfig) error {
	log.Info("start initialization")

	// wait for mysql to be ready
	if err := waitForMysqlReady(cfg); err != nil {
		return fmt.Errorf("mysql is not ready, err: %s", err)
	}

	// deactivate super read only
	log.Info("temporary disable SUPER_READ_ONLY")
	if err := app.RunQuery(cfg, "SET GLOBAL READ_ONLY = 1; SET GLOBAL SUPER_READ_ONLY = 0;"); err != nil {
		return fmt.Errorf("failed to configure master node, err: %s", err)
	}

	rSubject := app.NewSubject(cfg)

	rSubject.AddObserver("configure-orchestrator", configureOrchestratorUser)
	rSubject.AddObserver("configure-app-credentials", refreshAppCredentials)

	rSubject.Start(cfg.Stop)

	log.V(1).Info("configure replication user")
	if err := configureReplicationUser(cfg); err != nil {
		return err
	}

	log.V(1).Info("configure metrics exporter user")
	if err := configureExporterUser(cfg); err != nil {
		return err
	}

	// if it's slave set replication source (master host)
	log.V(1).Info("configure topology")
	if err := configureTopology(cfg); err != nil {
		return err
	}

	// mark setup as complete by writing a row in config table
	log.V(1).Info("flag setup as complet")
	if err := markConfigurationDone(cfg); err != nil {
		return err
	}

	// if it's master node then make it writtable else make it read only
	log.V(1).Info("configure read only flag")
	if err := configReadOnly(cfg); err != nil {
		return err
	}

	log.V(1).Info("start http server")
	srv := newServer(cfg)
	return srv.ListenAndServe()
}

func configureOrchestratorUser(cfg *app.MysqlConfig) error {
	if !app.UpdateAll(cfg.OrchestratorUser, cfg.OrchestratorPassword) {
		return nil
	}

	finish, err := temporaryDisableSuperReadOnly(cfg)
	if err != nil {
		return err
	}
	defer finish()

	query := `
	  SET @@SESSION.SQL_LOG_BIN = 0;

      CREATE USER IF NOT EXISTS ?@'%%';
      ALTER USER ?@'%%' IDENTIFIED BY ?;

	  GRANT SUPER, PROCESS, REPLICATION SLAVE, REPLICATION CLIENT, RELOAD ON *.* TO ?@'%%';
	  GRANT SELECT ON %s.* TO ?@'%%';
	  GRANT SELECT ON mysql.slave_master_info TO ?@'%%';
	`

	// insert toolsDBName, it's not user input so it's safe. Can't use
	// placeholders for table names, see:
	// https://github.com/golang/go/issues/18478
	query = fmt.Sprintf(query, app.ToolsDbName)

	user := cfg.OrchestratorUser.String()
	pass := cfg.OrchestratorPassword.String()
	if err := app.RunQuery(cfg, query, user, user, pass, user, user, user); err != nil {
		return fmt.Errorf("failed to configure orchestrator (user/pass/access), err: %s", err)
	}

	log.Info("finish orchestrator user")

	return nil
}

func configureReplicationUser(cfg *app.MysqlConfig) error {
	query := `
	  SET @@SESSION.SQL_LOG_BIN = 0;

      CREATE USER IF NOT EXISTS ?@'%';
      ALTER USER ?@'%' IDENTIFIED BY ?;

	  GRANT SELECT, PROCESS, RELOAD, LOCK TABLES, REPLICATION CLIENT,
            REPLICATION SLAVE ON *.* TO ?@'%';
	`

	user := cfg.ReplicationUser
	pass := cfg.ReplicationPassword
	if err := app.RunQuery(cfg, query, user, user, pass, user); err != nil {
		return fmt.Errorf("failed to configure replication user: %s", err)
	}

	return nil
}

func configureExporterUser(cfg *app.MysqlConfig) error {

	query := `
	  SET @@SESSION.SQL_LOG_BIN = 0;

      CREATE USER IF NOT EXISTS ?@'localhost';
      ALTER USER ?@'localhost' IDENTIFIED BY ? WITH MAX_USER_CONNECTIONS 3;

	  GRANT SELECT, PROCESS, REPLICATION CLIENT ON *.* TO ?@'localhost';
	`

	user := cfg.MetricsUser
	pass := cfg.MetricsPassword
	if err := app.RunQuery(cfg, query, user, user, pass, user); err != nil {
		return fmt.Errorf("failed to metrics exporter user: %s", err)
	}

	return nil
}

func waitForMysqlReady(cfg *app.MysqlConfig) error {
	log.V(1).Info("wait for mysql to be ready")

	for i := 0; i < timeOut; i++ {
		time.Sleep(1 * time.Second)
		if err := app.RunQuery(cfg, "SELECT 1"); err == nil {
			break
		}
	}
	if err := app.RunQuery(cfg, "SELECT 1"); err != nil {
		log.V(1).Info("mysql is not ready", "error", err)
		return err
	}

	log.V(1).Info("mysql is ready")
	return nil

}

func configReadOnly(cfg *app.MysqlConfig) error {
	var query string
	if cfg.NodeRole == app.MasterNode {
		query = "SET GLOBAL READ_ONLY = 0"
	} else {
		query = "SET GLOBAL SUPER_READ_ONLY = 1"
	}
	if err := app.RunQuery(cfg, query); err != nil {
		return fmt.Errorf("failed to set read_only config, err: %s", err)
	}
	return nil
}

// configureTopology has to be run before slave is started
func configureTopology(cfg *app.MysqlConfig) error {
	if cfg.NodeRole == app.SlaveNode {
		log.Info("setting up as slave")
		if app.ShouldBootstrapNode() {
			log.Info("doing bootstrap")
			if gtid, err := app.ReadPurgedGTID(); err == nil {
				log.Info("RESET MASTER and setting GTID_PURGED", "gtid", gtid)
				if errQ := app.RunQuery(
					cfg, "RESET MASTER; SET GLOBAL GTID_PURGED=?", gtid); errQ != nil {
					return errQ
				}
			} else {
				log.V(-1).Info("can't determine what GTID to purge", "error", err)
			}
		}

		// slave node
		query := `
		  CHANGE MASTER TO MASTER_AUTO_POSITION=1,
			MASTER_USER=?,
			MASTER_PASSWORD=?,
		    MASTER_HOST=?,
		    MASTER_CONNECT_RETRY=?;
		`
		user := cfg.ReplicationUser
		pass := cfg.ReplicationPassword
		if err := app.RunQuery(
			cfg, query, user, pass, cfg.MasterHost, connRetry,
		); err != nil {
			return fmt.Errorf("failed to configure slave node, err: %s", err)
		}

		query = "START SLAVE;"
		if err := app.RunQuery(cfg, query); err != nil {
			log.Info("failed to start slave in the simple mode, trying a second method")
			// TODO: https://bugs.mysql.com/bug.php?id=83713
			query2 := `
			  reset slave;
			  start slave IO_THREAD;
			  stop slave IO_THREAD;
			  reset slave;
			  start slave;
			`
			if err := app.RunQuery(cfg, query2); err != nil {
				return fmt.Errorf("failed to start slave node, err: %s", err)
			}
		}
	}

	return nil
}

func markConfigurationDone(cfg *app.MysqlConfig) error {
	query := `
	  SET @@SESSION.SQL_LOG_BIN = 0;
	  BEGIN;
	  CREATE DATABASE IF NOT EXISTS %[1]s;
	      CREATE TABLE IF NOT EXISTS %[1]s.%[2]s  (
		name varchar(255) NOT NULL,
		value varchar(255) NOT NULL,
		inserted_at datetime NOT NULL
	      ) ENGINE=csv;

	  INSERT INTO %[1]s.%[2]s VALUES ("init_completed", "?", now());
	  COMMIT;
	`

	// insert tables and databases names. It's safe because is not user input.
	// see: https://github.com/golang/go/issues/18478
	query = fmt.Sprintf(query, app.ToolsDbName, app.ToolsInitTableName)

	if err := app.RunQuery(cfg, query, cfg.Hostname); err != nil {
		return fmt.Errorf("failed to mark configuration done, err: %s", err)
	}

	return nil
}

func temporaryDisableSuperReadOnly(cfg *app.MysqlConfig) (func(), error) {
	setReadOnly := make(chan struct{}, 1)
	close := func() {
		// send the close signal
		setReadOnly <- struct{}{}
	}

	superReadOnly, err := app.ReadMysqlVariable(cfg, true, "SUPER_READ_ONLY")
	if err != nil {
		return close, err
	}

	log.Info("node super read only", "superReadOnly", superReadOnly)

	if superReadOnly == "0" {
		return close, nil
	}

	query := "SET @@GLOBAL.SUPER_READ_ONLY = 0;"
	if err := app.RunQuery(cfg, query); err != nil {
		return close, err
	}

	go func() {
		// wait for actions to finish
		<-setReadOnly
		query := "SET @@GLOBAL.SUPER_READ_ONLY = 1;"
		if err := app.RunQuery(cfg, query); err != nil {
			log.Error(err, "failed to set super read only")
		}
	}()

	return close, nil
}

func refreshAppCredentials(cfg *app.MysqlConfig) error {
	if !app.UpdateAll(cfg.MysqlPassword) {
		return nil
	}

	// update also the mysql user, this is only to read the user from secret
	app.UpdateAll(cfg.MysqlUser)

	finish, err := temporaryDisableSuperReadOnly(cfg)
	if err != nil {
		return err
	}
	defer finish()

	query := `
	  SET @@SESSION.SQL_LOG_BIN = 0;

      ALTER USER IF EXISTS ?@'%' IDENTIFIED BY ?;
	`

	user := cfg.MysqlUser.String()
	pass := cfg.MysqlPassword.String()
	if err := app.RunQuery(cfg, query, user, pass); err != nil {
		return fmt.Errorf("failed to configure application user: %s", err)
	}

	return nil

}
