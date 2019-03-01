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

package app

import (
	"database/sql"
	"fmt"

	// add mysql driver
	_ "github.com/go-sql-driver/mysql"
)

// RunQuery executes a query
func RunQuery(cfg *MysqlConfig, q string, args ...interface{}) error {
	if cfg.MysqlDSN == nil {
		log.Info("could not get mysql connection DSN")
		return fmt.Errorf("no DSN specified")
	}

	db, err := sql.Open("mysql", *cfg.MysqlDSN)
	if err != nil {
		return err
	}
	defer func() {
		if cErr := db.Close(); cErr != nil {
			log.Error(cErr, "failed closing the database connection")
		}
	}()

	log.V(1).Info("running query", "query", q, "args", args)
	if _, err := db.Exec(q, args...); err != nil {
		return err
	}

	return nil
}

// ReadMysqlVariable reads the mysql variable
func ReadMysqlVariable(cfg *MysqlConfig, global bool, varName string) (string, error) {
	if cfg.MysqlDSN == nil {
		log.Info("could not get mysql connection DSN")
		return "", fmt.Errorf("no DSN specified")
	}

	db, err := sql.Open("mysql", *cfg.MysqlDSN)
	if err != nil {
		return "", err
	}
	defer func() {
		if cErr := db.Close(); cErr != nil {
			log.Error(cErr, "failed closing the database connection")
		}
	}()

	varType := "SESSION"
	if global {
		varType = "GLOBAL"
	}

	q := "SELECT @@?.?;"

	log.V(1).Info("running query", "query", q, "variable", varName, "global", global)
	row := db.QueryRow(q, varType, varName)
	if row == nil {
		return "", fmt.Errorf("no row found")
	}

	var value string
	err = row.Scan(&value)
	if err != nil {
		return "", err
	}

	return value, nil
}
