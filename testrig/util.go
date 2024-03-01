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

package testrig

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"time"

	"github.com/superseriousbusiness/gotosocial/internal/filter/visibility"
	"github.com/superseriousbusiness/gotosocial/internal/messages"
	tlprocessor "github.com/superseriousbusiness/gotosocial/internal/processing/timeline"
	wprocessor "github.com/superseriousbusiness/gotosocial/internal/processing/workers"
	"github.com/superseriousbusiness/gotosocial/internal/state"
	"github.com/superseriousbusiness/gotosocial/internal/timeline"
	"github.com/superseriousbusiness/gotosocial/internal/typeutils"
)

// Starts workers on the provided state using noop processing functions.
// Useful when you *don't* want to trigger side effects in a test.
func StartNoopWorkers(state *state.State) {
	state.Workers.EnqueueClientAPI = func(context.Context, ...messages.FromClientAPI) {}
	state.Workers.EnqueueFediAPI = func(context.Context, ...messages.FromFediAPI) {}
	state.Workers.ProcessFromClientAPI = func(context.Context, messages.FromClientAPI) error { return nil }
	state.Workers.ProcessFromFediAPI = func(context.Context, messages.FromFediAPI) error { return nil }

	_ = state.Workers.Scheduler.Start()
	_ = state.Workers.ClientAPI.Start(1, 10)
	_ = state.Workers.Federator.Start(1, 10)
	_ = state.Workers.Media.Start(1, 10)
}

// Starts workers on the provided state using processing functions from the given
// workers processor. Useful when you *do* want to trigger side effects in a test.
func StartWorkers(state *state.State, wProcessor *wprocessor.Processor) {
	state.Workers.EnqueueClientAPI = wProcessor.EnqueueClientAPI
	state.Workers.EnqueueFediAPI = wProcessor.EnqueueFediAPI
	state.Workers.ProcessFromClientAPI = wProcessor.ProcessFromClientAPI
	state.Workers.ProcessFromFediAPI = wProcessor.ProcessFromFediAPI

	_ = state.Workers.Scheduler.Start()
	_ = state.Workers.ClientAPI.Start(1, 10)
	_ = state.Workers.Federator.Start(1, 10)
	_ = state.Workers.Media.Start(1, 10)
}

func StopWorkers(state *state.State) {
	_ = state.Workers.Scheduler.Stop()
	_ = state.Workers.ClientAPI.Stop()
	_ = state.Workers.Federator.Stop()
	_ = state.Workers.Media.Stop()
}

func StartTimelines(state *state.State, filter *visibility.Filter, converter *typeutils.Converter) {
	state.Timelines.Home = timeline.NewManager(
		tlprocessor.HomeTimelineGrab(state),
		tlprocessor.HomeTimelineFilter(state, filter),
		tlprocessor.HomeTimelineStatusPrepare(state, converter),
		tlprocessor.SkipInsert(),
	)
	if err := state.Timelines.Home.Start(); err != nil {
		panic(fmt.Sprintf("error starting home timeline: %s", err))
	}

	state.Timelines.List = timeline.NewManager(
		tlprocessor.ListTimelineGrab(state),
		tlprocessor.ListTimelineFilter(state, filter),
		tlprocessor.ListTimelineStatusPrepare(state, converter),
		tlprocessor.SkipInsert(),
	)
	if err := state.Timelines.List.Start(); err != nil {
		panic(fmt.Sprintf("error starting list timeline: %s", err))
	}
}

// CreateMultipartFormData is a handy function for taking a fieldname and a filename, and creating a multipart form bytes buffer
// with the file contents set in the given fieldname. The extraFields param can be used to add extra FormFields to the request, as necessary.
// The returned bytes.Buffer b can be used like so:
//
//	httptest.NewRequest(http.MethodPost, "https://example.org/whateverpath", bytes.NewReader(b.Bytes()))
//
// The returned *multipart.Writer w can be used to set the content type of the request, like so:
//
//	req.Header.Set("Content-Type", w.FormDataContentType())
func CreateMultipartFormData(fieldName string, fileName string, extraFields map[string][]string) (bytes.Buffer, *multipart.Writer, error) {
	var b bytes.Buffer

	w := multipart.NewWriter(&b)
	var fw io.Writer

	if fileName != "" {
		file, err := os.Open(fileName)
		if err != nil {
			return b, nil, err
		}
		if fw, err = w.CreateFormFile(fieldName, file.Name()); err != nil {
			return b, nil, err
		}
		if _, err = io.Copy(fw, file); err != nil {
			return b, nil, err
		}
	}

	for k, vs := range extraFields {
		for _, v := range vs {
			if err := w.WriteField(k, v); err != nil {
				return b, nil, err
			}
		}
	}

	if err := w.Close(); err != nil {
		return b, nil, err
	}
	return b, w, nil
}

// URLMustParse tries to parse the given URL and panics if it can't.
// Should only be used in tests.
func URLMustParse(stringURL string) *url.URL {
	u, err := url.Parse(stringURL)
	if err != nil {
		panic(err)
	}
	return u
}

// TimeMustParse tries to parse the given time as RFC3339, and panics if it can't.
// Should only be used in tests.
func TimeMustParse(timeString string) time.Time {
	t, err := time.Parse(time.RFC3339, timeString)
	if err != nil {
		panic(err)
	}
	return t
}

// WaitFor calls condition every 200ms, returning true
// when condition() returns true, or false after 5s.
//
// It's useful for when you're waiting for something to
// happen, but you don't know exactly how long it will take,
// and you want to fail if the thing doesn't happen within 5s.
func WaitFor(condition func() bool) bool {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()

	for {
		select {
		case <-tick.C:
			if condition() {
				return true
			}
		case <-timeout.C:
			return false
		}
	}
}
