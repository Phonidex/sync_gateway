//  Copyright (c) 2013 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package rest

import (
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/couchbaselabs/sync_gateway/auth"
	"github.com/couchbaselabs/sync_gateway/base"
	"github.com/couchbaselabs/sync_gateway/channels"
	"github.com/couchbaselabs/sync_gateway/db"
)

const kDefaultSessionTTL = 24 * time.Hour

// Respond with a JSON struct containing info about the current login session
func (h *handler) respondWithSessionInfo() error {

	response := h.formatSessionResponse(h.user)

	h.writeJSON(response)
	return nil
}

// GET /_session returns info about the current user
func (h *handler) handleSessionGET() error {
	return h.respondWithSessionInfo()
}

// POST /_session creates a login session and sets its cookie
func (h *handler) handleSessionPOST() error {
	var params struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	err := h.readJSONInto(&params)
	if err != nil {
		return err
	}
	var user auth.User
	user, err = h.db.Authenticator().GetUser(params.Name)
	if err != nil {
		return err
	}

	if !user.Authenticate(params.Password) {
		user = nil
	}
	return h.makeSession(user)
}

// DELETE /_session logs out the current session
func (h *handler) handleSessionDELETE() error {
	cookie := h.db.Authenticator().DeleteSessionForCookie(h.rq)
	if cookie == nil {
		return base.HTTPErrorf(http.StatusNotFound, "no session")
	}
	http.SetCookie(h.response, cookie)
	return nil
}

func (h *handler) makeSession(user auth.User) error {
	if user == nil {
		return base.HTTPErrorf(http.StatusUnauthorized, "Invalid login")
	}
	h.user = user
	auth := h.db.Authenticator()
	session, err := auth.CreateSession(user.Name(), kDefaultSessionTTL)
	if err != nil {
		return err
	}
	cookie := auth.MakeSessionCookie(session)
	cookie.Path = "/" + h.db.Name + "/"
	http.SetCookie(h.response, cookie)
	return h.respondWithSessionInfo()
}

func (h *handler) makeSessionFromEmail(email string, createUserIfNeeded bool) error {

	// Email is verified. Look up the user and make a login session for her:
	user, err := h.db.Authenticator().GetUserByEmail(email)
	if err != nil {
		return err
	}
	if user == nil {
		// The email address is authentic but we have no user account for it.
		if !createUserIfNeeded {
			return base.HTTPErrorf(http.StatusUnauthorized, "No such user")
		}

		if len(email) < 1 {
			return base.HTTPErrorf(http.StatusBadRequest, "Cannot register new user: email is missing")
		}

		// Create a User with the given email address as username and a random password.
		user, err = h.registerNewUser(email)
		if err != nil {
			return err
		}
	}
	return h.makeSession(user)

}

// ADMIN API: Generates a login session for a user and returns the session ID and cookie name.
func (h *handler) createUserSession() error {
	h.assertAdminOnly()
	var params struct {
		Name string `json:"name"`
		TTL  int    `json:"ttl"`
	}
	params.TTL = int(kDefaultSessionTTL / time.Second)
	err := h.readJSONInto(&params)
	if err != nil {
		return err
	} else if params.Name == "" || params.Name == "GUEST" || !auth.IsValidPrincipalName(params.Name) {
		return base.HTTPErrorf(http.StatusBadRequest, "Invalid or missing user name")
	} else if user, err := h.db.Authenticator().GetUser(params.Name); user == nil {
		if err == nil {
			err = base.HTTPErrorf(http.StatusNotFound, "No such user %q", params.Name)
		}
		return err
	}
	ttl := time.Duration(params.TTL) * time.Second
	if ttl < 1.0 {
		return base.HTTPErrorf(http.StatusBadRequest, "Invalid or missing ttl")
	}

	session, err := h.db.Authenticator().CreateSession(params.Name, ttl)
	if err != nil {
		return err
	}
	var response struct {
		SessionID  string    `json:"session_id"`
		Expires    time.Time `json:"expires"`
		CookieName string    `json:"cookie_name"`
	}
	response.SessionID = session.ID
	response.Expires = session.Expiration
	response.CookieName = auth.CookieName
	h.writeJSON(response)
	return nil
}

func (h *handler) getUserSession() error {

	h.assertAdminOnly()
	session, err := h.db.Authenticator().GetSession(mux.Vars(h.rq)["sessionid"])

	if session == nil {
		if err == nil {
			err = kNotFoundError
		}
		return err
	}

	return h.respondWithSessionInfoForSession(session)
}

// ADMIN API: Deletes a specified session.  If username is present on the request, validates
// that the session being deleted is associated with the user.
func (h *handler) deleteUserSession() error {
	h.assertAdminOnly()
	userName := mux.Vars(h.rq)["name"]
	if userName != "" {
		return h.deleteUserSessionWithValidation(mux.Vars(h.rq)["sessionid"], userName)
	} else {
		return h.db.Authenticator().DeleteSession(mux.Vars(h.rq)["sessionid"])
	}
}

// ADMIN API: Deletes a collection of sessions for a user.  If "*" is included as a
// key, removes all session for the user.
func (h *handler) deleteUserSessions() error {
	h.assertAdminOnly()

	var sessionIds []string
	deleteAll := false
	input, err := h.readJSON()
	if err == nil {
		keys, ok := input["keys"].([]interface{})
		sessionIds = make([]string, len(keys))

		for i := 0; i < len(keys); i++ {
			if sessionIds[i], ok = keys[i].(string); !ok {
				break
			}
			// if * is passed in as a session id, we want to delete all sessions for the user
			if sessionIds[i] == "*" {
				deleteAll = true
			}
		}
		if !ok {
			err = base.HTTPErrorf(http.StatusBadRequest, "Bad/missing keys")
		}
	}

	userName := mux.Vars(h.rq)["name"]
	if deleteAll {
		err = h.db.DeleteUserSessions(userName)
		if err != nil {
			return err
		}
	} else {
		for _, sessionId := range sessionIds {
			err = h.deleteUserSessionWithValidation(sessionId, userName)
			if err != nil {
				// fail silently for failed delete
			}
		}
	}

	return err
}

// Delete a session if associated with the user provided
func (h *handler) deleteUserSessionWithValidation(sessionId string, userName string) error {

	// Validate that the session being deleted belongs to the user.  This adds some
	// overhead - for user-agnostic session deletion should use deleteSession
	session, getErr := h.db.Authenticator().GetSession(sessionId)
	if getErr == nil {
		if session.Username == userName {
			delErr := h.db.Authenticator().DeleteSession(sessionId)
			if delErr != nil {
				return delErr
			}
		}
	}
	return nil
}

// Respond with a JSON struct containing info about the current login session
func (h *handler) respondWithSessionInfoForSession(session *auth.LoginSession) error {

	var userName string
	if session != nil {
		userName = session.Username
	}

	user, err := h.db.Authenticator().GetUser(userName)

	// let the empty user case succeed
	if err != nil {
		return err
	}

	response := h.formatSessionResponse(user)
	if response != nil {
		h.writeJSON(response)
	}
	return nil
}

// Formats session response similar to what is returned by CouchDB
func (h *handler) formatSessionResponse(user auth.User) db.Body {

	var name *string
	allChannels := channels.TimedSet{}

	if user != nil {
		userName := user.Name()
		if userName != "" {
			name = &userName
		}
		allChannels = user.Channels()
	}

	// Return a JSON struct similar to what CouchDB returns:
	userCtx := db.Body{"name": name, "channels": allChannels}
	handlers := []string{"default", "cookie"}
	if h.PersonaEnabled() {
		handlers = append(handlers, "persona")
	}
	response := db.Body{"ok": true, "userCtx": userCtx, "authentication_handlers": handlers}
	return response

}
