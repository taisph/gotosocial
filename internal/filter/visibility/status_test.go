// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package visibility_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/suite"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
)

type StatusVisibleTestSuite struct {
	FilterStandardTestSuite
}

func (suite *StatusVisibleTestSuite) TestOwnStatusVisible() {
	testStatus := suite.testStatuses["local_account_1_status_1"]
	testAccount := suite.testAccounts["local_account_1"]
	ctx := context.Background()

	visible, err := suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)

	suite.True(visible)
}

func (suite *StatusVisibleTestSuite) TestOwnDMVisible() {
	ctx := context.Background()

	testStatusID := suite.testStatuses["local_account_2_status_6"].ID
	testStatus, err := suite.db.GetStatusByID(ctx, testStatusID)
	suite.NoError(err)
	testAccount := suite.testAccounts["local_account_2"]

	visible, err := suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)

	suite.True(visible)
}

func (suite *StatusVisibleTestSuite) TestDMVisibleToTarget() {
	ctx := context.Background()

	testStatusID := suite.testStatuses["local_account_2_status_6"].ID
	testStatus, err := suite.db.GetStatusByID(ctx, testStatusID)
	suite.NoError(err)
	testAccount := suite.testAccounts["local_account_1"]

	visible, err := suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)

	suite.True(visible)
}

func (suite *StatusVisibleTestSuite) TestDMNotVisibleIfNotMentioned() {
	ctx := context.Background()

	testStatusID := suite.testStatuses["local_account_2_status_6"].ID
	testStatus, err := suite.db.GetStatusByID(ctx, testStatusID)
	suite.NoError(err)
	testAccount := suite.testAccounts["admin_account"]

	visible, err := suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)

	suite.False(visible)
}

func (suite *StatusVisibleTestSuite) TestStatusNotVisibleIfNotMutuals() {
	ctx := context.Background()

	suite.db.DeleteByID(ctx, suite.testFollows["local_account_2_local_account_1"].ID, &gtsmodel.Follow{})

	testStatusID := suite.testStatuses["local_account_1_status_4"].ID
	testStatus, err := suite.db.GetStatusByID(ctx, testStatusID)
	suite.NoError(err)
	testAccount := suite.testAccounts["local_account_2"]

	visible, err := suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)

	suite.False(visible)
}

func (suite *StatusVisibleTestSuite) TestStatusNotVisibleIfNotFollowing() {
	ctx := context.Background()

	suite.db.DeleteByID(ctx, suite.testFollows["admin_account_local_account_1"].ID, &gtsmodel.Follow{})

	testStatusID := suite.testStatuses["local_account_1_status_5"].ID
	testStatus, err := suite.db.GetStatusByID(ctx, testStatusID)
	suite.NoError(err)
	testAccount := suite.testAccounts["admin_account"]

	visible, err := suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)

	suite.False(visible)
}

func (suite *StatusVisibleTestSuite) TestStatusNotVisibleIfNotMutualsCached() {
	ctx := context.Background()
	testStatusID := suite.testStatuses["local_account_1_status_4"].ID
	testStatus, err := suite.db.GetStatusByID(ctx, testStatusID)
	suite.NoError(err)
	testAccount := suite.testAccounts["local_account_2"]

	// Perform a status visibility check while mutuals, this shsould be true.
	visible, err := suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)
	suite.True(visible)

	err = suite.db.DeleteFollowByID(ctx, suite.testFollows["local_account_2_local_account_1"].ID)
	suite.NoError(err)

	// Perform a status visibility check after unfollow, this should be false.
	visible, err = suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)
	suite.False(visible)
}

func (suite *StatusVisibleTestSuite) TestStatusNotVisibleIfNotFollowingCached() {
	ctx := context.Background()
	testStatusID := suite.testStatuses["local_account_1_status_5"].ID
	testStatus, err := suite.db.GetStatusByID(ctx, testStatusID)
	suite.NoError(err)
	testAccount := suite.testAccounts["admin_account"]

	// Perform a status visibility check while following, this shsould be true.
	visible, err := suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)
	suite.True(visible)

	err = suite.db.DeleteFollowByID(ctx, suite.testFollows["admin_account_local_account_1"].ID)
	suite.NoError(err)

	// Perform a status visibility check after unfollow, this should be false.
	visible, err = suite.filter.StatusVisible(ctx, testAccount, testStatus)
	suite.NoError(err)
	suite.False(visible)
}

func TestStatusVisibleTestSuite(t *testing.T) {
	suite.Run(t, new(StatusVisibleTestSuite))
}
