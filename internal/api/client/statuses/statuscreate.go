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

package statuses

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	apimodel "github.com/superseriousbusiness/gotosocial/internal/api/model"
	apiutil "github.com/superseriousbusiness/gotosocial/internal/api/util"
	"github.com/superseriousbusiness/gotosocial/internal/config"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/oauth"
	"github.com/superseriousbusiness/gotosocial/internal/validate"
)

// StatusCreatePOSTHandler swagger:operation POST /api/v1/statuses statusCreate
//
// Create a new status.
//
// The parameters can also be given in the body of the request, as JSON, if the content-type is set to 'application/json'.
// The parameters can also be given in the body of the request, as XML, if the content-type is set to 'application/xml'.
//
//	---
//	tags:
//	- statuses
//
//	consumes:
//	- application/json
//	- application/xml
//	- application/x-www-form-urlencoded
//
//	produces:
//	- application/json
//
//	security:
//	- OAuth2 Bearer:
//		- write:statuses
//
//	responses:
//		'200':
//			description: "The newly created status."
//			schema:
//				"$ref": "#/definitions/status"
//		'400':
//			description: bad request
//		'401':
//			description: unauthorized
//		'403':
//			description: forbidden
//		'404':
//			description: not found
//		'406':
//			description: not acceptable
//		'500':
//			description: internal server error
func (m *Module) StatusCreatePOSTHandler(c *gin.Context) {
	authed, err := oauth.Authed(c, true, true, true, true)
	if err != nil {
		apiutil.ErrorHandler(c, gtserror.NewErrorUnauthorized(err, err.Error()), m.processor.InstanceGetV1)
		return
	}

	if _, err := apiutil.NegotiateAccept(c, apiutil.JSONAcceptHeaders...); err != nil {
		apiutil.ErrorHandler(c, gtserror.NewErrorNotAcceptable(err, err.Error()), m.processor.InstanceGetV1)
		return
	}

	form := &apimodel.AdvancedStatusCreateForm{}
	if err := c.ShouldBind(form); err != nil {
		apiutil.ErrorHandler(c, gtserror.NewErrorBadRequest(err, err.Error()), m.processor.InstanceGetV1)
		return
	}

	// DO NOT COMMIT THIS UNCOMMENTED, IT WILL CAUSE MASS CHAOS.
	// this is being left in as an ode to kim's shitposting.
	//
	// user := authed.Account.DisplayName
	// if user == "" {
	// 	user = authed.Account.Username
	// }
	// form.Status += "\n\nsent from " + user + "'s iphone\n"

	if err := validateNormalizeCreateStatus(form); err != nil {
		apiutil.ErrorHandler(c, gtserror.NewErrorBadRequest(err, err.Error()), m.processor.InstanceGetV1)
		return
	}

	apiStatus, errWithCode := m.processor.Status().Create(
		c.Request.Context(),
		authed.Account,
		authed.Application,
		form,
	)
	if errWithCode != nil {
		apiutil.ErrorHandler(c, errWithCode, m.processor.InstanceGetV1)
		return
	}

	c.JSON(http.StatusOK, apiStatus)
}

// validateNormalizeCreateStatus checks the form
// for disallowed combinations of attachments and
// overlength inputs.
//
// Side effect: normalizes the post's language tag.
func validateNormalizeCreateStatus(form *apimodel.AdvancedStatusCreateForm) error {
	hasStatus := form.Status != ""
	hasMedia := len(form.MediaIDs) != 0
	hasPoll := form.Poll != nil

	if !hasStatus && !hasMedia && !hasPoll {
		return errors.New("no status, media, or poll provided")
	}

	if hasMedia && hasPoll {
		return errors.New("can't post media + poll in same status")
	}

	maxChars := config.GetStatusesMaxChars()
	if length := len([]rune(form.Status)) + len([]rune(form.SpoilerText)); length > maxChars {
		return fmt.Errorf("status too long, %d characters provided (including spoiler/content warning) but limit is %d", length, maxChars)
	}

	maxMediaFiles := config.GetStatusesMediaMaxFiles()
	if len(form.MediaIDs) > maxMediaFiles {
		return fmt.Errorf("too many media files attached to status, %d attached but limit is %d", len(form.MediaIDs), maxMediaFiles)
	}

	if form.Poll != nil {
		if err := validateNormalizeCreatePoll(form); err != nil {
			return err
		}
	}

	if form.Language != "" {
		language, err := validate.Language(form.Language)
		if err != nil {
			return err
		}
		form.Language = language
	}

	return nil
}

func validateNormalizeCreatePoll(form *apimodel.AdvancedStatusCreateForm) error {
	maxPollOptions := config.GetStatusesPollMaxOptions()
	maxPollChars := config.GetStatusesPollOptionMaxChars()

	// Normalize poll expiry if necessary.
	// If we parsed this as JSON, expires_in
	// may be either a float64 or a string.
	if ei := form.Poll.ExpiresInI; ei != nil {
		switch e := ei.(type) {
		case float64:
			form.Poll.ExpiresIn = int(e)

		case string:
			expiresIn, err := strconv.Atoi(e)
			if err != nil {
				return fmt.Errorf("could not parse expires_in value %s as integer: %w", e, err)
			}

			form.Poll.ExpiresIn = expiresIn

		default:
			return fmt.Errorf("could not parse expires_in type %T as integer", ei)
		}
	}

	if len(form.Poll.Options) == 0 {
		return errors.New("poll with no options")
	}

	if len(form.Poll.Options) > maxPollOptions {
		return fmt.Errorf("too many poll options provided, %d provided but limit is %d", len(form.Poll.Options), maxPollOptions)
	}

	for _, p := range form.Poll.Options {
		if length := len([]rune(p)); length > maxPollChars {
			return fmt.Errorf("poll option too long, %d characters provided but limit is %d", length, maxPollChars)
		}
	}

	return nil
}
