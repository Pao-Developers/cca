/*
 * Handle the unified global state
 *
 * Copyright (C) 2024  Runxi Yu <https://runxiyu.org>
 * SPDX-License-Identifier: AGPL-3.0-or-later
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package main

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

/*
 * 0: Student access is disabled
 * 1: Student have read-only access
 * 2: Student can choose courses
 */
var state uint32 /* atomic */

func loadState() error {
	var _state uint32
	err := db.QueryRow(
		context.Background(),
		"SELECT value FROM misc WHERE key = 'state'",
	).Scan(&_state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_state = 0
			_, err := db.Exec(
				context.Background(),
				"INSERT INTO misc(key, value) VALUES ('state', $1)",
				_state,
			)
			if err != nil {
				return wrapError(errUnexpectedDBError, err)
			}
		} else {
			return wrapError(errUnexpectedDBError, err)
		}
	}
	atomic.StoreUint32(&state, _state)
	return nil
}

func saveStateValue(ctx context.Context, newState uint32) error {
	_, err := db.Exec(
		ctx,
		"UPDATE misc SET value = $1 WHERE key = 'state'",
		newState,
	)
	if err != nil {
		return wrapError(errUnexpectedDBError, err)
	}
	return nil
}

func setState(ctx context.Context, newState uint32) error {
	switch newState {
	case 0:
		cancelPool.Range(func(_, value interface{}) bool {
			cancel, ok := value.(*context.CancelFunc)
			if !ok {
				panic("chanPool has non-\"*contect.CancelFunc\" values")
			}
			(*cancel)()
			return false
		})
	case 1:
		propagate("STOP")
	case 2:
		propagate("START")
	default:
		return errInvalidState
	}
	err := saveStateValue(ctx, newState)
	if err != nil {
		return err
	}
	atomic.StoreUint32(&state, newState)
	return nil
}
