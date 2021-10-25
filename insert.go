// Copyright 2019 Yunion
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

package sqlchemy

import (
	"fmt"
	"reflect"
	"strings"

	"yunion.io/x/pkg/util/timeutils"

	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/gotypes"
	"yunion.io/x/pkg/util/reflectutils"
)

// Insert perform a insert operation, the value of the record is store in dt
func (t *STableSpec) Insert(dt interface{}) error {
	if !t.Database().backend.CanInsert() {
		return errors.ErrNotSupported
	}
	return t.insert(dt, false, false)
}

// InsertOrUpdate perform a insert or update operation, the value of the record is string in dt
// MySQL: INSERT INTO ... ON DUPLICATE KEY UPDATE ...
// works only for the cases that all values of primary keys are determeted before insert
func (t *STableSpec) InsertOrUpdate(dt interface{}) error {
	if !t.Database().backend.CanInsertOrUpdate() {
		return errors.ErrNotSupported
	}
	return t.insert(dt, true, false)
}

func (t *STableSpec) InsertSqlPrep(dataFields reflectutils.SStructFieldValueSet, update bool) (string, []interface{}, error) {
	var autoIncField string
	createdAtFields := make([]string, 0)

	names := make([]string, 0)
	format := make([]string, 0)
	values := make([]interface{}, 0)

	updates := make([]string, 0)
	updateValues := make([]interface{}, 0)

	primaryKeys := make([]string, 0)

	now := timeutils.UtcNow()

	for _, c := range t.Columns() {
		isAutoInc := false
		if c.IsAutoIncrement() {
			isAutoInc = true
		}

		k := c.Name()

		ov, find := dataFields.GetInterface(k)

		if !find {
			continue
		}

		if c.IsPrimary() {
			primaryKeys = append(primaryKeys, fmt.Sprintf("`%s`", k))
		}

		if c.IsCreatedAt() || c.IsUpdatedAt() {
			createdAtFields = append(createdAtFields, k)
			names = append(names, fmt.Sprintf("`%s`", k))
			if c.IsZero(ov) {
				// format = append(format, t.Database().backend.CurrentUTCTimeStampString())
				values = append(values, now)
				format = append(format, "?")
			} else {
				values = append(values, ov)
				format = append(format, "?")
			}

			if update && c.IsUpdatedAt() {
				if c.IsZero(ov) {
					updates = append(updates, fmt.Sprintf("`%s` = ?", k))
					updateValues = append(updateValues, now)
				} else {
					updates = append(updates, fmt.Sprintf("`%s` = ?", k))
					updateValues = append(updateValues, ov)
				}
			}

			continue
		}

		if update && c.IsAutoVersion() {
			updates = append(updates, fmt.Sprintf("`%s` = `%s` + 1", k, k))
			continue
		}

		if c.IsSupportDefault() && (len(c.Default()) > 0 || c.IsString()) && !gotypes.IsNil(ov) && c.IsZero(ov) && !c.AllowZero() { // empty text value
			val := c.ConvertFromString(c.Default())
			values = append(values, val)
			names = append(names, fmt.Sprintf("`%s`", k))
			format = append(format, "?")

			if update && !c.IsPrimary() {
				updates = append(updates, fmt.Sprintf("`%s` = ?", k))
				updateValues = append(updateValues, val)
			}
			continue
		}

		if !gotypes.IsNil(ov) && (!c.IsZero(ov) || (!c.IsPointer() && !c.IsText())) && !isAutoInc {
			v := c.ConvertFromValue(ov)
			values = append(values, v)
			names = append(names, fmt.Sprintf("`%s`", k))
			format = append(format, "?")

			if update && !c.IsPrimary() {
				updates = append(updates, fmt.Sprintf("`%s` = ?", k))
				updateValues = append(updateValues, v)
			}
			continue
		}

		if c.IsPrimary() {
			if isAutoInc {
				if len(autoIncField) > 0 {
					panic(fmt.Sprintf("multiple auto_increment columns: %q, %q", autoIncField, k))
				}
				autoIncField = k
			} else if c.IsText() {
				values = append(values, "")
				names = append(names, fmt.Sprintf("`%s`", k))
				format = append(format, "?")
			} else {
				return "", nil, errors.Wrapf(ErrEmptyPrimaryKey, "cannot insert for null primary key %q", k)
			}

			continue
		}
	}

	var insertSql string
	if !update {
		insertSql = templateEval(t.Database().backend.InsertSQLTemplate(), struct {
			Table   string
			Columns string
			Values  string
		}{
			Table:   t.name,
			Columns: strings.Join(names, ", "),
			Values:  strings.Join(format, ", "),
		})
	} else {
		insertSql = templateEval(t.Database().backend.InsertOrUpdateSQLTemplate(), struct {
			Table       string
			Columns     string
			Values      string
			PrimaryKeys string
			SetValues   string
		}{
			Table:       t.name,
			Columns:     strings.Join(names, ", "),
			Values:      strings.Join(format, ", "),
			PrimaryKeys: strings.Join(primaryKeys, ", "),
			SetValues:   strings.Join(updates, ", "),
		})
		values = append(values, updateValues...)
	}

	return insertSql, values, nil
}

func beforeInsert(val reflect.Value) {
	switch val.Kind() {
	case reflect.Struct:
		structType := val.Type()
		for i := 0; i < val.NumField(); i++ {
			fieldType := structType.Field(i)
			if fieldType.Anonymous {
				beforeInsert(val.Field(i))
			}
		}
		valPtr := val.Addr()
		afterMarshalFunc := valPtr.MethodByName("BeforeInsert")
		if afterMarshalFunc.IsValid() && !afterMarshalFunc.IsNil() {
			afterMarshalFunc.Call([]reflect.Value{})
		}
	case reflect.Ptr:
		beforeInsert(val.Elem())
	}
}

func (t *STableSpec) insert(data interface{}, update bool, debug bool) error {
	beforeInsert(reflect.ValueOf(data))

	dataValue := reflect.ValueOf(data).Elem()
	dataFields := reflectutils.FetchStructFieldValueSet(dataValue)
	insertSql, values, err := t.InsertSqlPrep(dataFields, update)
	if err != nil {
		return errors.Wrap(err, "insertSqlPrep")
	}

	if DEBUG_SQLCHEMY || debug {
		log.Debugf("%s values: %#v", insertSql, values)
	}
	log.Debugf("sql: %s values: %#v", insertSql, values)

	tx, err := t.Database().db.Begin()
	if err != nil {
		return errors.Wrap(err, "Begin transaction")
	}
	stmt, err := tx.Prepare(insertSql)
	if err != nil {
		return errors.Wrapf(err, "Prepare sql %s", insertSql)
	}
	defer stmt.Close()

	results, err := stmt.Exec(values...)
	if err != nil {
		return errors.Wrap(err, "Exec")
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "Commit transaction")
	}

	if t.Database().backend.CanSupportRowAffected() {
		affectCnt, err := results.RowsAffected()
		if err != nil {
			return err
		}

		targetCnt := int64(1)
		if update {
			// for insertOrUpdate cases, if no duplication, targetCnt=1, else targetCnt=2
			targetCnt = 2
		}
		if (!update && affectCnt < 1) || affectCnt > targetCnt {
			return errors.Wrapf(ErrUnexpectRowCount, "Insert affected cnt %d != (1, %d)", affectCnt, targetCnt)
		}
	}
	/*
		if len(autoIncField) > 0 {
			lastId, err := results.LastInsertId()
			if err == nil {
				val, ok := reflectutils.FindStructFieldValue(dataValue, autoIncField)
				if ok {
					gotypes.SetValue(val, fmt.Sprint(lastId))
				}
			}
		}
	*/

	// query the value, so default value can be feedback into the object
	// fields = reflectutils.FetchStructFieldNameValueInterfaces(dataValue)
	q := t.Query()
	for _, c := range t.Columns() {
		if c.IsPrimary() {
			if c.IsAutoIncrement() {
				lastId, err := results.LastInsertId()
				if err != nil {
					return errors.Wrap(err, "fetching lastInsertId failed")
				}
				q = q.Equals(c.Name(), lastId)
			} else {
				priVal, _ := dataFields.GetInterface(c.Name())
				if !gotypes.IsNil(priVal) {
					q = q.Equals(c.Name(), priVal)
				}
			}
		}
	}
	err = q.First(data)
	if err != nil {
		return errors.Wrap(err, "query after insert failed")
	}

	return nil
}
