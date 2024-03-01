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

package dereferencing

import (
	"context"
	"errors"
	"io"
	"net/url"
	"time"

	"github.com/superseriousbusiness/activity/pub"
	"github.com/superseriousbusiness/gotosocial/internal/ap"
	"github.com/superseriousbusiness/gotosocial/internal/config"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtscontext"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/id"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/media"
	"github.com/superseriousbusiness/gotosocial/internal/transport"
	"github.com/superseriousbusiness/gotosocial/internal/util"
)

// accountFresh returns true if the given account is
// still considered "fresh" according to the desired
// freshness window (falls back to default if nil).
//
// Local accounts will always be considered fresh because
// there's no remote state that could have changed.
//
// True is also returned for suspended accounts, since
// we'll never want to try to refresh one of these.
//
// Return value of false indicates that the account
// is not fresh and should be refreshed from remote.
func accountFresh(
	account *gtsmodel.Account,
	window *FreshnessWindow,
) bool {
	if window == nil {
		window = DefaultAccountFreshness
	}

	if account.IsLocal() {
		// Can't refresh
		// local accounts.
		return true
	}

	if account.IsSuspended() {
		// Can't/won't refresh
		// suspended accounts.
		return true
	}

	if account.IsInstance() &&
		!account.IsNew() {
		// Existing instance account.
		// No need for refresh.
		return true
	}

	// Moment when the account is
	// considered stale according to
	// desired freshness window.
	staleAt := account.FetchedAt.Add(
		time.Duration(*window),
	)

	// It's still fresh if the time now
	// is not past the point of staleness.
	return !time.Now().After(staleAt)
}

// GetAccountByURI will attempt to fetch an accounts by its URI, first checking the database. In the case of a newly-met remote model, or a remote model
// whose last_fetched date is beyond a certain interval, the account will be dereferenced. In the case of dereferencing, some low-priority account information
// may be enqueued for asynchronous fetching, e.g. featured account statuses (pins). An ActivityPub object indicates the account was dereferenced.
func (d *Dereferencer) GetAccountByURI(ctx context.Context, requestUser string, uri *url.URL) (*gtsmodel.Account, ap.Accountable, error) {
	// Fetch and dereference account if necessary.
	account, accountable, err := d.getAccountByURI(ctx,
		requestUser,
		uri,
	)
	if err != nil {
		return nil, nil, err
	}

	if accountable != nil {
		// This account was updated, enqueue re-dereference featured posts.
		d.state.Workers.Federator.MustEnqueueCtx(ctx, func(ctx context.Context) {
			if err := d.dereferenceAccountFeatured(ctx, requestUser, account); err != nil {
				log.Errorf(ctx, "error fetching account featured collection: %v", err)
			}
		})
	}

	return account, accountable, nil
}

// getAccountByURI is a package internal form of .GetAccountByURI() that doesn't bother dereferencing featured posts on update.
func (d *Dereferencer) getAccountByURI(ctx context.Context, requestUser string, uri *url.URL) (*gtsmodel.Account, ap.Accountable, error) {
	var (
		account *gtsmodel.Account
		uriStr  = uri.String()
		err     error
	)

	// Search the database for existing account with URI.
	account, err = d.state.DB.GetAccountByURI(
		// request a barebones object, it may be in the
		// db but with related models not yet dereferenced.
		gtscontext.SetBarebones(ctx),
		uriStr,
	)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return nil, nil, gtserror.Newf("error checking database for account %s by uri: %w", uriStr, err)
	}

	if account == nil {
		// Else, search the database for existing by URL.
		account, err = d.state.DB.GetAccountByURL(
			gtscontext.SetBarebones(ctx),
			uriStr,
		)
		if err != nil && !errors.Is(err, db.ErrNoEntries) {
			return nil, nil, gtserror.Newf("error checking database for account %s by url: %w", uriStr, err)
		}
	}

	if account == nil {
		// Ensure that this is isn't a search for a local account.
		if uri.Host == config.GetHost() || uri.Host == config.GetAccountDomain() {
			return nil, nil, gtserror.SetUnretrievable(err) // this will be db.ErrNoEntries
		}

		// Create and pass-through a new bare-bones model for dereferencing.
		return d.enrichAccountSafely(ctx, requestUser, uri, &gtsmodel.Account{
			ID:     id.NewULID(),
			Domain: uri.Host,
			URI:    uriStr,
		}, nil)
	}

	if accountFresh(account, nil) {
		// This is an existing account that is up-to-date,
		// before returning ensure it is fully populated.
		if err := d.state.DB.PopulateAccount(ctx, account); err != nil {
			log.Errorf(ctx, "error populating existing account: %v", err)
		}

		return account, nil, nil
	}

	// Try to update existing account model.
	latest, accountable, err := d.enrichAccountSafely(ctx,
		requestUser,
		uri,
		account,
		nil,
	)
	if err != nil {
		log.Errorf(ctx, "error enriching remote account: %v", err)

		// Fallback to existing.
		return account, nil, nil
	}

	return latest, accountable, nil
}

// GetAccountByUsernameDomain will attempt to fetch an accounts by its username@domain, first checking the database. In the case of a newly-met remote model,
// or a remote model whose last_fetched date is beyond a certain interval, the account will be dereferenced. In the case of dereferencing, some low-priority
// account information may be enqueued for asynchronous fetching, e.g. featured account statuses (pins). An ActivityPub object indicates the account was dereferenced.
func (d *Dereferencer) GetAccountByUsernameDomain(ctx context.Context, requestUser string, username string, domain string) (*gtsmodel.Account, ap.Accountable, error) {
	account, accountable, err := d.getAccountByUsernameDomain(
		ctx,
		requestUser,
		username,
		domain,
	)
	if err != nil {
		return nil, nil, err
	}

	if accountable != nil {
		// This account was updated, enqueue re-dereference featured posts.
		d.state.Workers.Federator.MustEnqueueCtx(ctx, func(ctx context.Context) {
			if err := d.dereferenceAccountFeatured(ctx, requestUser, account); err != nil {
				log.Errorf(ctx, "error fetching account featured collection: %v", err)
			}
		})
	}

	return account, accountable, nil
}

// getAccountByUsernameDomain is a package internal form of
// GetAccountByUsernameDomain() that doesn't bother deref of featured posts.
func (d *Dereferencer) getAccountByUsernameDomain(
	ctx context.Context,
	requestUser string,
	username string,
	domain string,
) (*gtsmodel.Account, ap.Accountable, error) {
	if domain == config.GetHost() || domain == config.GetAccountDomain() {
		// We do local lookups using an empty domain,
		// else it will fail the db search below.
		domain = ""
	}

	// Search the database for existing account with USERNAME@DOMAIN.
	account, err := d.state.DB.GetAccountByUsernameDomain(
		// request a barebones object, it may be in the
		// db but with related models not yet dereferenced.
		gtscontext.SetBarebones(ctx),
		username, domain,
	)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return nil, nil, gtserror.Newf("error checking database for account %s@%s: %w", username, domain, err)
	}

	if account == nil {
		if domain == "" {
			// failed local lookup, will be db.ErrNoEntries.
			return nil, nil, gtserror.SetUnretrievable(err)
		}

		// Create and pass-through a new bare-bones model for dereferencing.
		account, accountable, err := d.enrichAccountSafely(ctx, requestUser, nil, &gtsmodel.Account{
			ID:       id.NewULID(),
			Domain:   domain,
			Username: username,
		}, nil)
		if err != nil {
			return nil, nil, err
		}

		return account, accountable, nil
	}

	// Try to update existing account model.
	latest, accountable, err := d.RefreshAccount(ctx,
		requestUser,
		account,
		nil,
		nil,
	)
	if err != nil {
		// Fallback to existing.
		return account, nil, nil //nolint
	}

	if accountable == nil {
		// This is existing up-to-date account, ensure it is populated.
		if err := d.state.DB.PopulateAccount(ctx, latest); err != nil {
			log.Errorf(ctx, "error populating existing account: %v", err)
		}
	}

	return latest, accountable, nil
}

// RefreshAccount updates the given account if it's a
// remote account, and considered stale / not fresh
// based on Account.FetchedAt and desired freshness.
//
// An updated account model is returned, but in the
// case of dereferencing, some low-priority account
// info may be enqueued for asynchronous fetching,
// e.g. featured account statuses (pins).
//
// An ActivityPub object indicates the account was
// dereferenced (i.e. updated).
func (d *Dereferencer) RefreshAccount(
	ctx context.Context,
	requestUser string,
	account *gtsmodel.Account,
	accountable ap.Accountable,
	window *FreshnessWindow,
) (*gtsmodel.Account, ap.Accountable, error) {
	// If no incoming data is provided,
	// check whether account needs refresh.
	if accountable == nil &&
		accountFresh(account, window) {
		return account, nil, nil
	}

	// Parse the URI from account.
	uri, err := url.Parse(account.URI)
	if err != nil {
		return nil, nil, gtserror.Newf("invalid account uri %q: %w", account.URI, err)
	}

	// Try to update + deref passed account model.
	latest, accountable, err := d.enrichAccountSafely(ctx,
		requestUser,
		uri,
		account,
		accountable,
	)
	if err != nil {
		log.Errorf(ctx, "error enriching remote account: %v", err)
		return nil, nil, gtserror.Newf("error enriching remote account: %w", err)
	}

	if accountable != nil {
		// This account was updated, enqueue re-dereference featured posts.
		d.state.Workers.Federator.MustEnqueueCtx(ctx, func(ctx context.Context) {
			if err := d.dereferenceAccountFeatured(ctx, requestUser, latest); err != nil {
				log.Errorf(ctx, "error fetching account featured collection: %v", err)
			}
		})
	}

	return latest, accountable, nil
}

// RefreshAccountAsync enqueues the given account for
// an asychronous update fetching, if it's a remote
// account, and considered stale / not fresh based on
// Account.FetchedAt and desired freshness.
//
// This is a more optimized form of manually enqueueing
// .UpdateAccount() to the federation worker, since it
// only enqueues update if necessary.
func (d *Dereferencer) RefreshAccountAsync(
	ctx context.Context,
	requestUser string,
	account *gtsmodel.Account,
	accountable ap.Accountable,
	window *FreshnessWindow,
) {
	// If no incoming data is provided,
	// check whether account needs refresh.
	if accountable == nil &&
		accountFresh(account, window) {
		return
	}

	// Parse the URI from account.
	uri, err := url.Parse(account.URI)
	if err != nil {
		log.Errorf(ctx, "invalid account uri %q: %v", account.URI, err)
		return
	}

	// Enqueue a worker function to enrich this account async.
	d.state.Workers.Federator.MustEnqueueCtx(ctx, func(ctx context.Context) {
		latest, accountable, err := d.enrichAccountSafely(ctx, requestUser, uri, account, accountable)
		if err != nil {
			log.Errorf(ctx, "error enriching remote account: %v", err)
			return
		}

		if accountable != nil {
			// This account was updated, enqueue re-dereference featured posts.
			if err := d.dereferenceAccountFeatured(ctx, requestUser, latest); err != nil {
				log.Errorf(ctx, "error fetching account featured collection: %v", err)
			}
		}
	})
}

// enrichAccountSafely wraps enrichAccount() to perform
// it within the State{}.FedLocks mutexmap, which protects
// dereferencing actions with per-URI mutex locks.
func (d *Dereferencer) enrichAccountSafely(
	ctx context.Context,
	requestUser string,
	uri *url.URL,
	account *gtsmodel.Account,
	accountable ap.Accountable,
) (*gtsmodel.Account, ap.Accountable, error) {
	// Noop if account suspended;
	// we don't want to deref it.
	if account.IsSuspended() {
		return account, nil, nil
	}

	// By default use account.URI
	// as the per-URI deref lock.
	var uriStr string
	if account.URI != "" {
		uriStr = account.URI
	} else {
		// No URI is set yet, instead generate a faux-one from user+domain.
		uriStr = "https://" + account.Domain + "/users/" + account.Username
	}

	// Acquire per-URI deref lock, wraping unlock
	// to safely defer in case of panic, while still
	// performing more granular unlocks when needed.
	unlock := d.state.FedLocks.Lock(uriStr)
	unlock = util.DoOnce(unlock)
	defer unlock()

	// Perform status enrichment with passed vars.
	latest, apubAcc, err := d.enrichAccount(ctx,
		requestUser,
		uri,
		account,
		accountable,
	)

	if gtserror.StatusCode(err) >= 400 {
		if account.IsNew() {
			// This was a new account enrich
			// attempt which failed before we
			// got to store it, so we can't
			// return anything useful.
			return nil, nil, err
		}

		// We had this account stored already
		// before this enrichment attempt.
		//
		// Update fetched_at to slow re-attempts
		// but don't return early. We can still
		// return the model we had stored already.
		account.FetchedAt = time.Now()
		if err := d.state.DB.UpdateAccount(ctx, account, "fetched_at"); err != nil {
			log.Error(ctx, "error updating %s fetched_at: %v", uriStr, err)
		}
	}

	// Unlock now
	// we're done.
	unlock()

	if errors.Is(err, db.ErrAlreadyExists) {
		// Ensure AP model isn't set,
		// otherwise this indicates WE
		// enriched the account.
		apubAcc = nil

		// DATA RACE! We likely lost out to another goroutine
		// in a call to db.Put(Account). Look again in DB by URI.
		latest, err = d.state.DB.GetAccountByURI(ctx, account.URI)
		if err != nil {
			err = gtserror.Newf("error getting account %s from database after race: %w", uriStr, err)
		}
	}

	return latest, apubAcc, err
}

// enrichAccount will enrich the given account, whether a
// new barebones model, or existing model from the database.
// It handles necessary dereferencing, webfingering etc.
func (d *Dereferencer) enrichAccount(
	ctx context.Context,
	requestUser string,
	uri *url.URL,
	account *gtsmodel.Account,
	apubAcc ap.Accountable,
) (*gtsmodel.Account, ap.Accountable, error) {
	// Pre-fetch a transport for requesting username, used by later deref procedures.
	tsport, err := d.transportController.NewTransportForUsername(ctx, requestUser)
	if err != nil {
		return nil, nil, gtserror.Newf("couldn't create transport: %w", err)
	}

	if account.Username != "" {
		// A username was provided so we can attempt a webfinger, this ensures up-to-date accountdomain info.
		accDomain, accURI, err := d.fingerRemoteAccount(ctx, tsport, account.Username, account.Domain)
		switch {

		case err != nil && account.URI == "":
			// This is a new account (to us) with username@domain
			// but failed webfinger, nothing more we can do.
			err := gtserror.Newf("error webfingering account: %w", err)
			return nil, nil, gtserror.SetUnretrievable(err)

		case err != nil:
			// Simply log this error and move on,
			// we already have an account URI.
			log.Errorf(ctx,
				"error webfingering[1] remote account %s@%s: %v",
				account.Username, account.Domain, err,
			)

		case err == nil && account.Domain != accDomain:
			// After webfinger, we now have correct account domain from which we can do a final DB check.
			alreadyAcc, err := d.state.DB.GetAccountByUsernameDomain(ctx, account.Username, accDomain)
			if err != nil && !errors.Is(err, db.ErrNoEntries) {
				return nil, nil, gtserror.Newf("db error getting account after webfinger: %w", err)
			}

			if alreadyAcc != nil {
				// We had this account stored
				// under discovered accountDomain.
				//
				// Proceed with this account.
				account = alreadyAcc
			}

			// Whether we had the account or not, we
			// now have webfinger info relevant to the
			// account, so fallthrough to set webfinger
			// info on either the account we just found,
			// or the stub account we were passed.
			fallthrough

		case err == nil:
			// Update account with latest info.
			account.URI = accURI.String()
			account.Domain = accDomain
			uri = accURI
		}
	}

	if uri == nil {
		// No URI provided / found,
		// must parse from account.
		uri, err = url.Parse(account.URI)
		if err != nil {
			return nil, nil, gtserror.Newf(
				"invalid uri %q: %w",
				account.URI, gtserror.SetUnretrievable(err),
			)
		}

		// Check URI scheme ahead of time for more useful errs.
		if uri.Scheme != "http" && uri.Scheme != "https" {
			err := errors.New("account URI scheme must be http or https")
			return nil, nil, gtserror.Newf(
				"invalid uri %q: %w",
				account.URI, gtserror.SetUnretrievable(err),
			)
		}
	}

	/*
		BY THIS POINT we must have an account URI set,
		either provided, pinned to the account, or
		obtained via webfinger call.
	*/

	// Check whether this account URI is a blocked domain / subdomain.
	if blocked, err := d.state.DB.IsDomainBlocked(ctx, uri.Host); err != nil {
		return nil, nil, gtserror.Newf("error checking blocked domain: %w", err)
	} else if blocked {
		return nil, nil, gtserror.Newf("%s is blocked", uri.Host)
	}

	// Mark deref+update handshake start.
	d.startHandshake(requestUser, uri)
	defer d.stopHandshake(requestUser, uri)

	if apubAcc == nil {
		// We were not given any (partial) ActivityPub
		// version of this account as a parameter.
		// Dereference latest version of the account.
		rsp, err := tsport.Dereference(ctx, uri)
		if err != nil {
			err := gtserror.Newf("error dereferencing %s: %w", uri, err)
			return nil, nil, gtserror.SetUnretrievable(err)
		}

		// Attempt to resolve ActivityPub acc from response.
		apubAcc, err = ap.ResolveAccountable(ctx, rsp.Body)

		// Tidy up now done.
		_ = rsp.Body.Close()

		if err != nil {
			// ResolveAccountable will set gtserror.WrongType
			// on the returned error, so we don't need to do it here.
			err = gtserror.Newf("error resolving accountable %s: %w", uri, err)
			return nil, nil, err
		}

		// Check whether input URI and final returned URI
		// have changed (i.e. we followed some redirects).
		if finalURIStr := rsp.Request.URL.String(); //
		finalURIStr != uri.String() {

			// NOTE: this URI check + database call is performed
			// AFTER reading and closing response body, for performance.
			//
			// Check whether we have this account stored under *final* URI.
			alreadyAcc, err := d.state.DB.GetAccountByURI(ctx, finalURIStr)
			if err != nil && !errors.Is(err, db.ErrNoEntries) {
				return nil, nil, gtserror.Newf("db error getting account after redirects: %w", err)
			}

			if alreadyAcc != nil {
				// We had this account stored
				// under discovered final URI.
				//
				// Proceed with this account.
				account = alreadyAcc
			}

			// Update the input URI to
			// the final determined URI
			// for later URI checks.
			uri = rsp.Request.URL
		}
	}

	/*
		BY THIS POINT we must have the ActivityPub
		representation of the account, either provided,
		or obtained via a dereference call.
	*/

	// Convert the dereferenced AP account object to our GTS model.
	//
	// We put this in the variable latestAcc because we might want
	// to compare the provided account model with this fresh version,
	// in order to check if anything changed since we last saw it.
	latestAcc, err := d.converter.ASRepresentationToAccount(ctx,
		apubAcc,
		account.Domain,
	)
	if err != nil {
		// ASRepresentationToAccount will set Malformed on the
		// returned error, so we don't need to do it here.
		err = gtserror.Newf("error converting accountable to gts model for account %s: %w", uri, err)
		return nil, nil, err
	}

	if account.Username == "" {
		// Assume the host from the
		// ActivityPub representation.
		id := ap.GetJSONLDId(apubAcc)
		if id == nil {
			return nil, nil, gtserror.New("no id property found on person, or id was not an iri")
		}

		// Get IRI host value.
		accHost := id.Host

		// No username was provided, so no webfinger was attempted earlier.
		//
		// Now we have a username we can attempt again, to ensure up-to-date
		// accountDomain info. For this final attempt we should use the domain
		// of the ID of the dereffed account, rather than the URI we were given.
		//
		// This avoids cases where we were given a URI like
		// https://example.org/@someone@somewhere.else and we've been redirected
		// from example.org to somewhere.else: we want to take somewhere.else
		// as the accountDomain then, not the example.org we were redirected from.
		latestAcc.Domain, _, err = d.fingerRemoteAccount(ctx,
			tsport,
			latestAcc.Username,
			accHost,
		)
		if err != nil {
			// Webfingering account still failed, so we're not certain
			// what the accountDomain actually is. Exit here for safety.
			return nil, nil, gtserror.Newf(
				"error webfingering remote account %s@%s: %w",
				latestAcc.Username, accHost, err,
			)
		}
	}

	if latestAcc.Domain == "" {
		// Ensure we have a domain set by this point,
		// otherwise it gets stored as a local user!
		return nil, nil, gtserror.Newf("empty domain for %s", uri)
	}

	// Ensure the final parsed account URI / URL matches
	// the input URI we fetched (or received) it as.
	if expect := uri.String(); latestAcc.URI != expect &&
		latestAcc.URL != expect {
		return nil, nil, gtserror.Newf(
			"dereferenced account uri %s does not match %s",
			latestAcc.URI, expect,
		)
	}

	/*
		BY THIS POINT we have more or less a fullly-formed
		representation of the target account, derived from
		a combination of webfinger lookups and dereferencing.
		Further fetching beyond this point is for peripheral
		things like account avatar, header, emojis.
	*/

	// Ensure internal db ID is
	// set and update fetch time.
	latestAcc.ID = account.ID
	latestAcc.FetchedAt = time.Now()

	// Ensure the account's avatar media is populated, passing in existing to check for chages.
	if err := d.fetchRemoteAccountAvatar(ctx, tsport, account, latestAcc); err != nil {
		log.Errorf(ctx, "error fetching remote avatar for account %s: %v", uri, err)
	}

	// Ensure the account's avatar media is populated, passing in existing to check for chages.
	if err := d.fetchRemoteAccountHeader(ctx, tsport, account, latestAcc); err != nil {
		log.Errorf(ctx, "error fetching remote header for account %s: %v", uri, err)
	}

	// Fetch the latest remote account emoji IDs used in account display name/bio.
	if _, err = d.fetchRemoteAccountEmojis(ctx, latestAcc, requestUser); err != nil {
		log.Errorf(ctx, "error fetching remote emojis for account %s: %v", uri, err)
	}

	if account.IsNew() {
		// Prefer published/created time from
		// apubAcc, fall back to FetchedAt value.
		if latestAcc.CreatedAt.IsZero() {
			latestAcc.CreatedAt = latestAcc.FetchedAt
		}

		// Set time of update from the last-fetched date.
		latestAcc.UpdatedAt = latestAcc.FetchedAt

		// This is new, put it in the database.
		err := d.state.DB.PutAccount(ctx, latestAcc)
		if err != nil {
			return nil, nil, gtserror.Newf("error putting in database: %w", err)
		}
	} else {
		// Prefer published time from apubAcc,
		// fall back to previous stored value.
		if latestAcc.CreatedAt.IsZero() {
			latestAcc.CreatedAt = account.CreatedAt
		}

		// Set time of update from the last-fetched date.
		latestAcc.UpdatedAt = latestAcc.FetchedAt

		// Carry over existing account language.
		latestAcc.Language = account.Language

		// This is an existing account, update the model in the database.
		if err := d.state.DB.UpdateAccount(ctx, latestAcc); err != nil {
			return nil, nil, gtserror.Newf("error updating database: %w", err)
		}
	}

	return latestAcc, apubAcc, nil
}

func (d *Dereferencer) fetchRemoteAccountAvatar(ctx context.Context, tsport transport.Transport, existing, latestAcc *gtsmodel.Account) error {
	if latestAcc.AvatarRemoteURL == "" {
		// No avatar set on newest model, leave
		// latest avatar attachment ID empty.
		return nil
	}

	// By default we keep the previous media attachment ID. This will only
	// be changed if and when we have the new media loaded into storage.
	latestAcc.AvatarMediaAttachmentID = existing.AvatarMediaAttachmentID

	// If we had a media attachment ID already, and the URL
	// of the attachment hasn't changed from existing -> latest,
	// then we may be able to just keep our existing attachment
	// without having to make any remote calls.
	if latestAcc.AvatarMediaAttachmentID != "" &&
		existing.AvatarRemoteURL == latestAcc.AvatarRemoteURL {

		// Ensure we have media attachment with the known ID.
		media, err := d.state.DB.GetAttachmentByID(ctx, existing.AvatarMediaAttachmentID)
		if err != nil && !errors.Is(err, db.ErrNoEntries) {
			return gtserror.Newf("error getting attachment %s: %w", existing.AvatarMediaAttachmentID, err)
		}

		// Ensure attachment has correct properties.
		if media != nil && media.RemoteURL == latestAcc.AvatarRemoteURL {
			// We already have the most up-to-date
			// media attachment, keep using it.
			return nil
		}
	}

	// If we reach here, we know we need to fetch the most
	// up-to-date version of the attachment from remote.

	// Parse and validate the newly provided media URL.
	avatarURI, err := url.Parse(latestAcc.AvatarRemoteURL)
	if err != nil {
		return gtserror.Newf("error parsing url %s: %w", latestAcc.AvatarRemoteURL, err)
	}

	// Acquire lock for derefs map.
	unlock := d.state.FedLocks.Lock(latestAcc.AvatarRemoteURL)
	unlock = util.DoOnce(unlock)
	defer unlock()

	// Look for an existing dereference in progress.
	processing, ok := d.derefAvatars[latestAcc.AvatarRemoteURL]

	if !ok {
		// Set the media data function to dereference avatar from URI.
		data := func(ctx context.Context) (io.ReadCloser, int64, error) {
			return tsport.DereferenceMedia(ctx, avatarURI)
		}

		// Create new media processing request from the media manager instance.
		processing = d.mediaManager.PreProcessMedia(data, latestAcc.ID, &media.AdditionalMediaInfo{
			Avatar:    func() *bool { v := true; return &v }(),
			RemoteURL: &latestAcc.AvatarRemoteURL,
		})

		// Store media in map to mark as processing.
		d.derefAvatars[latestAcc.AvatarRemoteURL] = processing

		defer func() {
			// On exit safely remove media from map.
			unlock := d.state.FedLocks.Lock(latestAcc.AvatarRemoteURL)
			delete(d.derefAvatars, latestAcc.AvatarRemoteURL)
			unlock()
		}()
	}

	// Unlock map.
	unlock()

	// Start media attachment loading (blocking call).
	if _, err := processing.LoadAttachment(ctx); err != nil {
		return gtserror.Newf("error loading attachment %s: %w", latestAcc.AvatarRemoteURL, err)
	}

	// Set the newly loaded avatar media attachment ID.
	latestAcc.AvatarMediaAttachmentID = processing.AttachmentID()

	return nil
}

func (d *Dereferencer) fetchRemoteAccountHeader(ctx context.Context, tsport transport.Transport, existing, latestAcc *gtsmodel.Account) error {
	if latestAcc.HeaderRemoteURL == "" {
		// No header set on newest model, leave
		// latest header attachment ID empty.
		return nil
	}

	// By default we keep the previous media attachment ID. This will only
	// be changed if and when we have the new media loaded into storage.
	latestAcc.HeaderMediaAttachmentID = existing.HeaderMediaAttachmentID

	// If we had a media attachment ID already, and the URL
	// of the attachment hasn't changed from existing -> latest,
	// then we may be able to just keep our existing attachment
	// without having to make any remote calls.
	if latestAcc.HeaderMediaAttachmentID != "" &&
		existing.HeaderRemoteURL == latestAcc.HeaderRemoteURL {

		// Ensure we have media attachment with the known ID.
		media, err := d.state.DB.GetAttachmentByID(ctx, existing.HeaderMediaAttachmentID)
		if err != nil && !errors.Is(err, db.ErrNoEntries) {
			return gtserror.Newf("error getting attachment %s: %w", existing.HeaderMediaAttachmentID, err)
		}

		// Ensure attachment has correct properties.
		if media != nil && media.RemoteURL == latestAcc.HeaderRemoteURL {
			// We already have the most up-to-date
			// media attachment, keep using it.
			return nil
		}
	}

	// If we reach here, we know we need to fetch the most
	// up-to-date version of the attachment from remote.

	// Parse and validate the newly provided media URL.
	headerURI, err := url.Parse(latestAcc.HeaderRemoteURL)
	if err != nil {
		return gtserror.Newf("error parsing url %s: %w", latestAcc.HeaderRemoteURL, err)
	}

	// Acquire lock for derefs map.
	unlock := d.state.FedLocks.Lock(latestAcc.HeaderRemoteURL)
	unlock = util.DoOnce(unlock)
	defer unlock()

	// Look for an existing dereference in progress.
	processing, ok := d.derefHeaders[latestAcc.HeaderRemoteURL]

	if !ok {
		// Set the media data function to dereference avatar from URI.
		data := func(ctx context.Context) (io.ReadCloser, int64, error) {
			return tsport.DereferenceMedia(ctx, headerURI)
		}

		// Create new media processing request from the media manager instance.
		processing = d.mediaManager.PreProcessMedia(data, latestAcc.ID, &media.AdditionalMediaInfo{
			Header:    func() *bool { v := true; return &v }(),
			RemoteURL: &latestAcc.HeaderRemoteURL,
		})

		// Store media in map to mark as processing.
		d.derefHeaders[latestAcc.HeaderRemoteURL] = processing

		defer func() {
			// On exit safely remove media from map.
			unlock := d.state.FedLocks.Lock(latestAcc.HeaderRemoteURL)
			delete(d.derefHeaders, latestAcc.HeaderRemoteURL)
			unlock()
		}()
	}

	// Unlock map.
	unlock()

	// Start media attachment loading (blocking call).
	if _, err := processing.LoadAttachment(ctx); err != nil {
		return gtserror.Newf("error loading attachment %s: %w", latestAcc.HeaderRemoteURL, err)
	}

	// Set the newly loaded avatar media attachment ID.
	latestAcc.HeaderMediaAttachmentID = processing.AttachmentID()

	return nil
}

func (d *Dereferencer) fetchRemoteAccountEmojis(ctx context.Context, targetAccount *gtsmodel.Account, requestingUsername string) (bool, error) {
	maybeEmojis := targetAccount.Emojis
	maybeEmojiIDs := targetAccount.EmojiIDs

	// It's possible that the account had emoji IDs set on it, but not Emojis
	// themselves, depending on how it was fetched before being passed to us.
	//
	// If we only have IDs, fetch the emojis from the db. We know they're in
	// there or else they wouldn't have IDs.
	if len(maybeEmojiIDs) > len(maybeEmojis) {
		maybeEmojis = make([]*gtsmodel.Emoji, 0, len(maybeEmojiIDs))
		for _, emojiID := range maybeEmojiIDs {
			maybeEmoji, err := d.state.DB.GetEmojiByID(ctx, emojiID)
			if err != nil {
				return false, err
			}
			maybeEmojis = append(maybeEmojis, maybeEmoji)
		}
	}

	// For all the maybe emojis we have, we either fetch them from the database
	// (if we haven't already), or dereference them from the remote instance.
	gotEmojis, err := d.populateEmojis(ctx, maybeEmojis, requestingUsername)
	if err != nil {
		return false, err
	}

	// Extract the ID of each fetched or dereferenced emoji, so we can attach
	// this to the account if necessary.
	gotEmojiIDs := make([]string, 0, len(gotEmojis))
	for _, e := range gotEmojis {
		gotEmojiIDs = append(gotEmojiIDs, e.ID)
	}

	var (
		changed  = false // have the emojis for this account changed?
		maybeLen = len(maybeEmojis)
		gotLen   = len(gotEmojis)
	)

	// if the length of everything is zero, this is simple:
	// nothing has changed and there's nothing to do
	if maybeLen == 0 && gotLen == 0 {
		return changed, nil
	}

	// if the *amount* of emojis on the account has changed, then the got emojis
	// are definitely different from the previous ones (if there were any) --
	// the account has either more or fewer emojis set on it now, so take the
	// discovered emojis as the new correct ones.
	if maybeLen != gotLen {
		changed = true
		targetAccount.Emojis = gotEmojis
		targetAccount.EmojiIDs = gotEmojiIDs
		return changed, nil
	}

	// if the lengths are the same but not all of the slices are
	// zero, something *might* have changed, so we have to check

	// 1. did we have emojis before that we don't have now?
	for _, maybeEmoji := range maybeEmojis {
		var stillPresent bool

		for _, gotEmoji := range gotEmojis {
			if maybeEmoji.URI == gotEmoji.URI {
				// the emoji we maybe had is still present now,
				// so we can stop checking gotEmojis
				stillPresent = true
				break
			}
		}

		if !stillPresent {
			// at least one maybeEmoji is no longer present in
			// the got emojis, so we can stop checking now
			changed = true
			targetAccount.Emojis = gotEmojis
			targetAccount.EmojiIDs = gotEmojiIDs
			return changed, nil
		}
	}

	// 2. do we have emojis now that we didn't have before?
	for _, gotEmoji := range gotEmojis {
		var wasPresent bool

		for _, maybeEmoji := range maybeEmojis {
			// check emoji IDs here as well, because unreferenced
			// maybe emojis we didn't already have would not have
			// had IDs set on them yet
			if gotEmoji.URI == maybeEmoji.URI && gotEmoji.ID == maybeEmoji.ID {
				// this got emoji was present already in the maybeEmoji,
				// so we can stop checking through maybeEmojis
				wasPresent = true
				break
			}
		}

		if !wasPresent {
			// at least one gotEmojis was not present in
			// the maybeEmojis, so we can stop checking now
			changed = true
			targetAccount.Emojis = gotEmojis
			targetAccount.EmojiIDs = gotEmojiIDs
			return changed, nil
		}
	}

	return changed, nil
}

// dereferenceAccountFeatured dereferences an account's featuredCollectionURI (if not empty). For each discovered status, this status will
// be dereferenced (if necessary) and marked as pinned (if necessary). Then, old pins will be removed if they're not included in new pins.
func (d *Dereferencer) dereferenceAccountFeatured(ctx context.Context, requestUser string, account *gtsmodel.Account) error {
	uri, err := url.Parse(account.FeaturedCollectionURI)
	if err != nil {
		return err
	}

	collect, err := d.dereferenceCollection(ctx, requestUser, uri)
	if err != nil {
		return err
	}

	// Get previous pinned statuses (we'll need these later).
	wasPinned, err := d.state.DB.GetAccountPinnedStatuses(ctx, account.ID)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return gtserror.Newf("error getting account pinned statuses: %w", err)
	}

	var statusURIs []*url.URL

	for {
		// Get next collect item.
		item := collect.NextItem()
		if item == nil {
			break
		}

		// Check for available IRI.
		itemIRI, _ := pub.ToId(item)
		if itemIRI == nil {
			continue
		}

		if itemIRI.Host != uri.Host {
			// If this status doesn't share a host with its featured
			// collection URI, we shouldn't trust it. Just move on.
			continue
		}

		// Already append this status URI to our slice.
		// We do this here so that even if we can't get
		// the status in the next part for some reason,
		// we still know it was *meant* to be pinned.
		statusURIs = append(statusURIs, itemIRI)

		// Search for status by URI. Note this may return an existing model
		// we have stored with an error from attempted update, so check both.
		status, _, _, err := d.getStatusByURI(ctx, requestUser, itemIRI)
		if err != nil {
			log.Errorf(ctx, "error getting status from featured collection %s: %v", itemIRI, err)

			if status == nil {
				// This is only unactionable
				// if no status was returned.
				continue
			}
		}

		// If the status was already pinned,
		// we don't need to do anything.
		if !status.PinnedAt.IsZero() {
			continue
		}

		if status.AccountURI != account.URI {
			// Someone's pinned a status that doesn't
			// belong to them, this doesn't work for us.
			continue
		}

		if status.BoostOfID != "" {
			// Someone's pinned a boost. This
			// also doesn't work for us. (note
			// we check using BoostOfID since
			// BoostOfURI isn't *always* set).
			continue
		}

		// All conditions are met for this status to
		// be pinned, so we can finally update it.
		status.PinnedAt = time.Now()
		if err := d.state.DB.UpdateStatus(ctx, status, "pinned_at"); err != nil {
			log.Errorf(ctx, "error updating status in featured collection %s: %v", status.URI, err)
			continue
		}
	}

	// Now that we know which statuses are pinned, we should
	// *unpin* previous pinned statuses that aren't included.
outerLoop:
	for _, status := range wasPinned {
		for _, statusURI := range statusURIs {
			if status.URI == statusURI.String() {
				// This status is included in most recent
				// pinned uris. No need to keep checking.
				continue outerLoop
			}
		}

		// Status was pinned before, but is not included
		// in most recent pinned uris, so unpin it now.
		status.PinnedAt = time.Time{}
		if err := d.state.DB.UpdateStatus(ctx, status, "pinned_at"); err != nil {
			log.Errorf(ctx, "error unpinning status %s: %v", status.URI, err)
			continue
		}
	}

	return nil
}
